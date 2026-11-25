package dumps

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type hookFunc func(ctx context.Context, module string) error

func (f hookFunc) RunBackup(ctx context.Context, module string) error { return f(ctx, module) }

func runWith(t *testing.T, mods []string, hook func(d *Dumper, module string) error) (*Dumper, error) {
	t.Helper()
	d, _ := newDumper(t, mods, &fakeHooks{})
	d.Hooks = hookFunc(func(_ context.Context, m string) error { return hook(d, m) })
	_, err := d.Run(context.Background())
	return d, err
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func upload(d *Dumper, m, name, body string) error {
	return d.Accept(m, name, strings.NewReader(body))
}

// QA 1: a hook that exits 0 without uploading must not produce a marker.
func TestHookThatUploadsNothingFailsTheRun(t *testing.T) {
	d, err := runWith(t, []string{"wiki"}, func(*Dumper, string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "uploaded nothing") {
		t.Fatalf("got %v", err)
	}
	if exists(filepath.Join(d.Store.Dir, COMPLETE)) {
		t.Fatal("false success: a marker was written")
	}
}

// QA 1: yesterday's file must not stand in for today's missing upload.
func TestYesterdaysDumpDoesNotSatisfyTodaysRun(t *testing.T) {
	silent := false
	d, err := runWith(t, []string{"wiki"}, func(d *Dumper, m string) error { return upload(d, m, "a", "day1") })
	if err != nil {
		t.Fatal(err)
	}
	silent = true
	d.Hooks = hookFunc(func(_ context.Context, m string) error {
		if silent {
			return nil
		}
		return upload(d, m, "a", "x")
	})
	if _, err := d.Run(context.Background()); err == nil {
		t.Fatal("false success on the second run")
	}
}

// QA 1 and 8: only the set uploaded by this run is published; old names and old modules go.
func TestRunSwapsInOnlyTheNewSet(t *testing.T) {
	name := "old-name"
	d, err := runWith(t, []string{"gone", "wiki"}, func(d *Dumper, m string) error { return upload(d, m, name, "v") })
	if err != nil {
		t.Fatal(err)
	}
	name = "new-name"
	d.Modules = func(context.Context) ([]string, error) { return []string{"wiki"}, nil }
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	mods := filepath.Join(d.Store.Dir, "modules")
	if exists(filepath.Join(mods, "gone")) || exists(filepath.Join(mods, "wiki", "old-name")) || !exists(filepath.Join(mods, "wiki", "new-name")) {
		ents, _ := os.ReadDir(filepath.Join(mods, "wiki"))
		t.Fatalf("stale dumps kept: %v", ents)
	}
}

// QA 3: only the module whose hook is running may upload.
func TestOnlyTheRunningModuleMayUpload(t *testing.T) {
	var other error
	_, err := runWith(t, []string{"wiki"}, func(d *Dumper, m string) error {
		other = upload(d, "support", "x", "intruder")
		return upload(d, m, "x", "ok")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(other, ErrNoRun) {
		t.Fatalf("upload by a module whose hook is not running: %v", other)
	}
}

// gate is a reader that yields one byte, then blocks until released.
type gate struct {
	started, release chan struct{}
	sent             int
}

func (g *gate) Read(p []byte) (int, error) {
	switch g.sent {
	case 0:
		g.sent++
		p[0] = 'a'
		return 1, nil
	case 1:
		g.sent++
		close(g.started)
		<-g.release
		p[0] = 'b'
		return 1, nil
	}
	return 0, io.EOF
}

// QA 3: an upload still in flight when its hook ends can never land in the published set, so it
// cannot change data under the marker.
func TestUploadFinishingAfterItsHookIsDiscarded(t *testing.T) {
	g := &gate{started: make(chan struct{}), release: make(chan struct{})}
	late := make(chan error, 1)
	d, err := runWith(t, []string{"wiki"}, func(d *Dumper, m string) error {
		if err := upload(d, m, "good", "ok"); err != nil {
			return err
		}
		go func() { late <- d.Accept(m, "late", g) }()
		<-g.started
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	close(g.release)
	if err := <-late; !errors.Is(err, ErrNoRun) {
		t.Fatalf("late upload: %v", err)
	}
	var found []string
	_ = filepath.WalkDir(d.Store.Dir, func(p string, e os.DirEntry, _ error) error {
		if e != nil && (e.Name() == "late" || strings.HasPrefix(e.Name(), ".")) && p != d.Store.Dir {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("late file or temporaries left: %v", found)
	}
}

// QA 3: file count and total bytes per module are capped.
func TestUploadCapsPerModule(t *testing.T) {
	var third, big error
	_, err := runWith(t, []string{"wiki"}, func(d *Dumper, m string) error {
		d.Store.MaxFiles, d.Store.MaxModuleBytes = 2, 10
		if err := upload(d, m, "a", "1234"); err != nil {
			return err
		}
		if err := upload(d, m, "b", "1234"); err != nil {
			return err
		}
		third = upload(d, m, "c", "1")
		big = upload(d, m, "a", "123456789")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(third, ErrTooMany) || !errors.Is(big, ErrTooLarge) {
		t.Fatalf("third=%v big=%v", third, big)
	}
}

// QA 3 and 8: leftovers of a crashed run are removed at the start of the next one.
func TestRunStartRemovesLeftovers(t *testing.T) {
	d, _ := newDumper(t, nil, &fakeHooks{})
	left := []string{".run-old/tmp/.upload-1", "can-core/.state.db.tmp", "modules/wiki/.upload-9", "modules/purged/data"}
	for _, p := range left {
		full := filepath.Join(d.Store.Dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range append(left, ".run-old") {
		if exists(filepath.Join(d.Store.Dir, p)) {
			t.Errorf("%s survived", p)
		}
	}
}

// QA 8: purging a module deletes its dumps at once, not at the next run.
func TestForgetRemovesAModulesDumps(t *testing.T) {
	d, err := runWith(t, []string{"wiki"}, func(d *Dumper, m string) error { return upload(d, m, "a", "v") })
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Forget("wiki"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(d.Store.Dir, "modules", "wiki")) {
		t.Fatal("dumps kept after purge")
	}
	if err := d.Forget("../x"); !errors.Is(err, ErrBadName) {
		t.Fatalf("%v", err)
	}
}

// QA 12: every file and directory of the set is on stable storage before the marker appears.
func TestEverythingIsSyncedBeforeTheMarker(t *testing.T) {
	d, _ := newDumper(t, []string{"wiki"}, &fakeHooks{})
	d.Hooks = hookFunc(func(_ context.Context, m string) error { return upload(d, m, "a", "v") })
	synced := map[string]bool{}
	d.syncPath = func(p string) error {
		if exists(filepath.Join(d.Store.Dir, COMPLETE)) {
			t.Errorf("%s synced after the marker was written", p)
		}
		rel, _ := filepath.Rel(d.Store.Dir, p)
		synced[rel] = true
		return fsync(p)
	}
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".", "can-core", "can-core/state.db", "modules", "modules/wiki", "modules/wiki/a"} {
		if !synced[p] {
			t.Errorf("%s was not synced (synced: %v)", p, synced)
		}
	}
	// a failed sync means no marker
	d.syncPath = func(string) error { return errors.New("disk gone") }
	if _, err := d.Run(context.Background()); err == nil || exists(filepath.Join(d.Store.Dir, COMPLETE)) {
		t.Fatalf("marker despite a failed sync: %v", err)
	}
}

// QA 10: the state copy uses its own read connection, so it never holds can-core's single
// database connection (requests would queue behind a large copy).
func TestStateCopyDoesNotNeedTheApplicationsConnection(t *testing.T) {
	d, db := newDumper(t, nil, &fakeHooks{})
	tx, err := db.BeginTx(context.Background(), nil) // takes the pool's only connection
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := d.Run(ctx); err != nil {
		t.Fatalf("the run waited for the application's connection: %v", err)
	}
}

// QA 2: the list the backup script builds its SQLite exclusions from names exactly the modules
// dumped in this run.
func TestDumpedListNamesOnlyWhatWasDumped(t *testing.T) {
	d, _ := newDumper(t, []string{"empty", "forms", "wiki"}, &fakeHooks{})
	d.Database = func(_ context.Context, m string) string {
		if m == "wiki" {
			return "none"
		}
		return "sqlite"
	}
	d.Hooks = hookFunc(func(_ context.Context, m string) error {
		if m == "empty" {
			return ErrNoData
		}
		return upload(d, m, "a", "v")
	})
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(d.Store.Dir, DumpedList))
	if err != nil || string(b) != "forms sqlite\nwiki none\n" {
		t.Fatalf("%q %v", b, err)
	}
	// a failed run leaves no list behind
	d.Hooks = hookFunc(func(context.Context, string) error { return errors.New("x") })
	_, _ = d.Run(context.Background())
	if exists(filepath.Join(d.Store.Dir, DumpedList)) {
		t.Fatal("stale list of dumped modules")
	}
}
