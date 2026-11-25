package dumps

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type fakeHooks struct {
	calls []string
	fail  map[string]error
	d     *Dumper
	// markerDuringHook records whether the marker existed while each hook ran.
	markerDuringHook []bool
}

func (f *fakeHooks) RunBackup(_ context.Context, module string) error {
	f.calls = append(f.calls, module)
	_, err := os.Stat(filepath.Join(f.d.Store.Dir, COMPLETE))
	f.markerDuringHook = append(f.markerDuringHook, err == nil)
	if e := f.fail[module]; e != nil {
		return e
	}
	return f.d.Accept(module, "dump.bin", strings.NewReader("data of "+module))
}

var _ io.Reader = (*strings.Reader)(nil)

func newDumper(t *testing.T, mods []string, hooks *fakeHooks) (*Dumper, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	d := &Dumper{
		Store: &Store{Dir: filepath.Join(t.TempDir(), "dumps")}, DB: db.DB, Hooks: hooks,
		Modules: func(context.Context) ([]string, error) { return mods, nil },
		Now:     func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	hooks.d = d
	return d, db
}

func TestRunDumpsStateRunsHooksThenWritesMarkerLast(t *testing.T) {
	h := &fakeHooks{}
	d, db := newDumper(t, []string{"wiki", "support"}, h)
	// a stale marker from the previous run must be gone before anything is dumped
	if err := os.MkdirAll(d.Store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := backup.WriteMarker(filepath.Join(d.Store.Dir, COMPLETE), backup.Marker{RunID: "old", FinishedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE probe(v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO probe VALUES ('hello')`); err != nil {
		t.Fatal(err)
	}
	res, err := d.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, seen := range h.markerDuringHook {
		if seen {
			t.Fatalf("hook %d ran while the old marker still existed", i)
		}
	}
	m, err := backup.ReadMarker(filepath.Join(d.Store.Dir, COMPLETE))
	if err != nil || m.RunID != res.RunID || m.RunID == "old" {
		t.Fatalf("marker %+v err %v result %+v", m, err, res)
	}
	// the self dump is a consistent, readable copy of the state
	cp, err := sql.Open("sqlite", "file:"+filepath.Join(d.Store.Dir, "can-core", "state.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cp.Close() }()
	var v string
	if err := cp.QueryRow(`SELECT v FROM probe`).Scan(&v); err != nil || v != "hello" {
		t.Fatalf("state copy: %q %v", v, err)
	}
	if len(h.calls) != 2 {
		t.Fatalf("hooks run: %v", h.calls)
	}
	if res.Files != 3 {
		t.Fatalf("files counted: %d, want state.db + 2 module dumps", res.Files)
	}
}

func TestRunWithoutMarkerWhenAHookFails(t *testing.T) {
	h := &fakeHooks{fail: map[string]error{"support": errors.New("boom")}}
	d, _ := newDumper(t, []string{"support", "wiki"}, h)
	if _, err := d.Run(context.Background()); err == nil {
		t.Fatal("a failed hook must fail the run")
	}
	if _, err := os.Stat(filepath.Join(d.Store.Dir, COMPLETE)); err == nil {
		t.Fatal("no marker may exist after a failed run: the backup would claim consistency")
	}
	if len(h.calls) != 2 {
		t.Fatalf("one failing module must not stop the others from being dumped: %v", h.calls)
	}
}

func TestAcceptOnlyDuringARun(t *testing.T) {
	d, _ := newDumper(t, nil, &fakeHooks{})
	if err := d.Accept("wiki", "x", strings.NewReader("y")); !errors.Is(err, ErrNoRun) {
		t.Fatalf("got %v, want ErrNoRun: an upload outside a run would change data under a valid marker", err)
	}
}

func TestOnlyOneRunAtATime(t *testing.T) {
	d, _ := newDumper(t, nil, &fakeHooks{})
	if !d.begin() {
		t.Fatal("first begin must succeed")
	}
	if _, err := d.Run(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
}

func TestNeedsRunWhenMarkerMissingOrStale(t *testing.T) {
	d, _ := newDumper(t, nil, &fakeHooks{})
	if !d.NeedsRun(20 * time.Hour) {
		t.Fatal("no marker: a run is needed")
	}
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.NeedsRun(20 * time.Hour) {
		t.Fatal("fresh marker: no run needed")
	}
	d.Now = func() time.Time { return time.Unix(1_700_000_000, 0).Add(21 * time.Hour) }
	if !d.NeedsRun(20 * time.Hour) {
		t.Fatal("stale marker: a run is needed")
	}
}

func TestNextRunIsTheNextClockTime(t *testing.T) {
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if got := NextRun(now, 2, 30); !got.Equal(time.Date(2026, 1, 1, 2, 30, 0, 0, time.UTC)) {
		t.Fatalf("got %v", got)
	}
	now = time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	if got := NextRun(now, 2, 30); !got.Equal(time.Date(2026, 1, 2, 2, 30, 0, 0, time.UTC)) {
		t.Fatalf("got %v", got)
	}
}

func TestRequestFileTriggersOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dump.request")
	if Requested(p) {
		t.Fatal("no file, no request")
	}
	if err := Request(p); err != nil {
		t.Fatal(err)
	}
	if !Requested(p) {
		t.Fatal("request not seen")
	}
	if Requested(p) {
		t.Fatal("a request is consumed when seen")
	}
}
