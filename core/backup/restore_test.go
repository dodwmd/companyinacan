package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup/backuptest"
)

// rec is a recording fake of the cluster and the ledger source, in one event log.
type rec struct {
	events    []string
	failOnce  map[string]error
	live      []string // what Inspect reports
	volumes   []Volume
	ledger    []Entry
	ledgErr   error
	erased    []Entry
	accept    []string
	overwrite bool
	company   []byte
	atStart   func() // observes the host when the platform starts
}

func (r *rec) step(name string) error {
	r.events = append(r.events, name)
	if err := r.failOnce[name]; err != nil {
		delete(r.failOnce, name)
		return err
	}
	return nil
}
func (r *rec) count(prefix string) (n int) {
	for _, e := range r.events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return
}
func (r *rec) index(e string) int {
	for i, x := range r.events {
		if x == e {
			return i
		}
	}
	return -1
}

func (r *rec) Inspect(context.Context) ([]string, error) { return r.live, r.step("inspect") }
func (r *rec) Start(_ context.Context, o StartOptions) error {
	r.company, r.overwrite = o.CompanyYAML, o.Overwrite
	if r.atStart != nil {
		r.atStart()
	}
	return r.step("start")
}
func (r *rec) ApplyModules(context.Context, bool) (ApplyReport, error) {
	return ApplyReport{Modules: []string{"forms"}}, r.step("apply")
}
func (r *rec) Volumes(context.Context, []VolumeRef) ([]Volume, error) {
	return r.volumes, r.step("volumes")
}
func (r *rec) RunHooks(context.Context, []string) (HookReport, error) {
	return HookReport{}, r.step("hooks")
}
func (r *rec) Erase(_ context.Context, es []Entry, accept []string) (EraseReport, error) {
	r.erased, r.accept = es, accept
	return EraseReport{}, r.step("erase")
}
func (r *rec) Complete(context.Context) error { return r.step("complete") }
func (r *rec) Fetch(context.Context) ([]Entry, error) {
	r.events = append(r.events, "fetch-ledger")
	return r.ledger, r.ledgErr
}

type restoreEnv struct {
	r    *rec
	rs   *FakeRestic
	o    *Restore
	root string
	out  *bytes.Buffer
}

const (
	tDumps = "/var/lib/can/dumps"
	tHS    = "/var/lib/can/host-state"
)

// snapshotFiles builds the files of a consistent snapshot with a matching manifest.
func snapshotFiles(dumps map[string][]byte, pvc map[string][]byte) map[string][]byte {
	marker := []byte("run_id=r1\nfinished_at=1800000000\n")
	files := map[string][]byte{tDumps + "/" + MarkerName: marker, tHS + "/" + MarkerCopy: marker}
	names := make([]string, 0, len(dumps))
	for n := range dumps {
		names = append(names, n)
	}
	sort.Strings(names)
	var man strings.Builder
	for _, n := range names {
		files[tDumps+"/"+n] = dumps[n]
		man.WriteString(hashOf(dumps[n]) + "  ./" + n + "\n")
	}
	files[tHS+"/"+ManifestName] = []byte(man.String())
	for p, c := range pvc {
		files["/pvc/"+p] = c
	}
	return files
}

func goodDumps(t *testing.T) map[string][]byte {
	return map[string][]byte{
		"company.yaml":                  []byte("schema: 1\n"),
		"can-core/state.db":             backuptest.StateDB(t, backuptest.Module{ID: "forms", State: "enabled", Database: "sqlite"}),
		"can-core/secrets.json":         []byte(`{"version":1,"secrets":[{"namespace":"mod-forms","name":"can-module","data":{"K":"dg=="}}]}`),
		"modules/wiki/storage.tar.gz":   []byte("dump-a"),
		"modules/forms/placeholder.txt": []byte("x"),
	}
}

func newRestoreEnv(t *testing.T) *restoreEnv {
	t.Helper()
	root := t.TempDir()
	dumps := goodDumps(t)
	delete(dumps, "modules/wiki/storage.tar.gz")
	delete(dumps, "modules/forms/placeholder.txt")
	files := snapshotFiles(dumps, map[string][]byte{
		"pvc-OLD1_mod-forms_data/uploads/f.txt":          []byte("file"),
		"pvc-OLD2_can-system_can-core-data/dump.request": []byte("x"),
		"pvc-OLD3_kube-system_traefik/acme.json":         []byte("x"),
	})
	rs := &FakeRestic{Snaps: []Snapshot{{ID: "snapA", Time: time.Unix(100, 0), Tags: []string{"can"}}}, Files: files}
	r := &rec{
		failOnce: map[string]error{},
		volumes:  []Volume{{VolumeRef{"mod-forms", "data"}, "/pvc/pvc-NEW9_mod-forms_data"}},
		ledger:   MergeLedgers(nil, []Entry{{Subject: "s1", Class: "lead-submission", Date: "2026-11-01"}, {Subject: "s2", Class: "lead-submission", Date: "2026-11-02"}}),
	}
	if err := os.MkdirAll(filepath.Join(root, "pvc", "pvc-NEW9_mod-forms_data"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	o := &Restore{
		Kit: testKit(t), Restic: rs, Cluster: r, Ledger: r,
		Root: root, DumpsDir: tDumps, HostStateDir: tHS, PVCRoot: "/pvc", StagingDir: filepath.Join(root, "stage", "s"),
		CompanyDir: filepath.Join(root, "company"), StatePath: filepath.Join(root, "restore.json"), Out: out,
		ConfigDir: "/etc/can/backup.d", LedgerPath: "/var/lib/can/erasure-ledger.jsonl",
	}
	return &restoreEnv{r: r, rs: rs, o: o, root: root, out: out}
}

func (e *restoreEnv) run() error { return e.o.Run(context.Background()) }

func (e *restoreEnv) read(p string) string {
	b, _ := os.ReadFile(filepath.Join(e.root, p))
	return string(b)
}

func TestRestoreOrderAndHostEffects(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.atStart = func() {
		if e.read("var/lib/can/dumps/can-core/state.db") == "" {
			t.Error("the dumps must be in place before can-core starts")
		}
		if e.read("var/lib/can/dumps/"+MarkerName) != "" {
			t.Error("a completion marker on a half-restored server would let it be backed up")
		}
	}
	if err := e.run(); err != nil {
		t.Fatal(err, e.out.String())
	}
	last := -1
	for _, ev := range []string{"inspect", "start", "apply", "volumes", "hooks", "erase", "complete"} {
		i := e.r.index(ev)
		if i <= last {
			t.Fatalf("event %q out of order or missing in %v", ev, e.r.events)
		}
		last = i
	}
	if e.r.events[len(e.r.events)-1] != "complete" {
		t.Errorf("complete must be last: %v", e.r.events)
	}
	if len(e.r.erased) != 2 || e.r.overwrite || string(e.r.company) != "schema: 1\n" {
		t.Errorf("erased=%v overwrite=%v company=%q", e.r.erased, e.r.overwrite, e.r.company)
	}
	if got := e.read("company/company.yaml"); got != "schema: 1\n" {
		t.Errorf("company.yaml = %q", got)
	}
	if got := e.read("pvc/pvc-NEW9_mod-forms_data/uploads/f.txt"); got != "file" {
		t.Errorf("volume copy: %q", got)
	}
	led, err := (&Ledger{Path: filepath.Join(e.root, "var/lib/can/erasure-ledger.jsonl")}).Read()
	if err != nil || len(led) != 2 {
		t.Errorf("the server's ledger: %v %v", led, err)
	}
	if e.read("etc/can/backup.d/"+PasswordFileName) != e.o.Kit.Restic.Password || !strings.Contains(e.read("etc/can/backup.d/"+EnvFileName), "RESTIC_REPOSITORY=") {
		t.Error("backup credentials were not written back")
	}
	if _, err := os.Stat(e.o.StagingDir); !os.IsNotExist(err) {
		t.Errorf("the staged snapshot (secrets, erased data) must be removed: %v", err)
	}
	// A completed restore is a no-op when re-run.
	n := len(e.r.events)
	if err := e.run(); err != nil || len(e.r.events) != n {
		t.Errorf("re-run did work: %v %v", err, e.r.events[n:])
	}
}

func TestRestoreResumesEveryStep(t *testing.T) {
	for _, step := range []string{"start", "apply", "volumes", "hooks", "erase", "complete"} {
		t.Run(step, func(t *testing.T) {
			e := newRestoreEnv(t)
			e.r.failOnce[step] = errors.New("boom")
			if err := e.run(); err == nil || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("err = %v", err)
			}
			if step != "complete" && e.r.count("complete") != 0 {
				t.Fatal("went live although a step failed")
			}
			// A newer snapshot appearing in between must not change the pinned one.
			e.rs.Snaps = append(e.rs.Snaps, Snapshot{ID: "snapB", Time: time.Unix(200, 0), Tags: []string{"can"}})
			if err := e.run(); err != nil {
				t.Fatal(err)
			}
			if n := e.r.count(step); n != 2 {
				t.Errorf("%s ran %d times", step, n)
			}
			if e.r.count("inspect") != 1 {
				t.Error("the blank-server check must not run again on its own half-restored server")
			}
			for _, c := range e.realRestores() {
				if c.Snapshot != "snapA" {
					t.Errorf("resume used snapshot %s", c.Snapshot)
				}
			}
			if n := len(e.realRestores()); n != 1 {
				t.Errorf("the snapshot was fetched %d times", n)
			}
		})
	}
}

func TestRestoreRefetchesALostStagingDirectory(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.failOnce["apply"] = errors.New("boom")
	if e.run() == nil {
		t.Fatal("expected failure")
	}
	if err := os.RemoveAll(e.o.StagingDir); err != nil {
		t.Fatal(err)
	}
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if got := e.read("pvc/pvc-NEW9_mod-forms_data/uploads/f.txt"); got != "file" {
		t.Errorf("volume copy: %q", got)
	}
}

func TestRestoreRefusesNonBlankServer(t *testing.T) {
	for name, dirty := range map[string]func(*restoreEnv){
		"dumps":   func(e *restoreEnv) { put(e, "var/lib/can/dumps/existing.db") },
		"cluster": func(e *restoreEnv) { e.r.live = []string{"deployment can-system/can-core exists"} },
		"ledger": func(e *restoreEnv) {
			put(e, "var/lib/can/erasure-ledger.jsonl.keep")
			put(e, "etc/can/backup.d/"+PasswordFileName)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRestoreEnv(t)
			dirty(e)
			for _, confirm := range []string{"", "yes", "wrong name"} {
				e.o.ConfirmOverwrite = confirm
				err := e.run()
				if err == nil || !strings.Contains(err.Error(), "not blank") || !strings.Contains(err.Error(), "--confirm-overwrite") {
					t.Fatalf("confirm %q: %v", confirm, err)
				}
			}
			if e.r.count("start") != 0 || len(e.realRestores()) != 0 {
				t.Fatal("a refused restore changed something")
			}
			if _, err := os.Stat(e.o.StatePath); err == nil {
				t.Fatal("a refused restore left state behind")
			}
			e.o.ConfirmOverwrite = e.o.Kit.Company.Name
			if err := e.run(); err != nil {
				t.Fatalf("confirmed: %v", err)
			}
			if !e.r.overwrite || e.read("var/lib/can/dumps/existing.db") != "" {
				t.Errorf("overwrite=%v", e.r.overwrite)
			}
		})
	}
}

func put(e *restoreEnv, p string) {
	full := filepath.Join(e.root, p)
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	_ = os.WriteFile(full, []byte("precious"), 0o600)
}

func TestRestoreStopsTheTimerOfAnOverwrittenServer(t *testing.T) {
	e := newRestoreEnv(t)
	stopped := 0
	e.o.StopTimer = func(context.Context) error { stopped++; return nil }
	e.r.live = []string{"namespace mod-forms exists"}
	e.o.ConfirmOverwrite = e.o.Kit.Company.Name
	if err := e.run(); err != nil || stopped != 1 {
		t.Fatalf("%v stopped=%d", err, stopped)
	}
}

func TestRestoreLedgerMissingBlocksBeforeAnythingChanges(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.ledger, e.r.ledgErr = nil, ErrNoLedger
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "ledger") || e.r.count("start") != 0 {
		t.Fatalf("err = %v events=%v", err, e.r.events)
	}
	e.o.AllowNoLedger = true
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreLedgerErrorBlocks(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.ledgErr = errors.New("bucket unreachable")
	e.o.AllowNoLedger = true // only covers a MISSING ledger, never a failing fetch
	if err := e.run(); err == nil || e.r.count("start") != 0 {
		t.Fatalf("err=%v events=%v", err, e.r.events)
	}
}

// The ledger is read again at the erasure step: an erasure recorded while the restore ran is
// replayed, and a fetch failing then keeps the company in restore mode.
func TestRestoreReadsTheLedgerAgainBeforeErasing(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.atStart = func() {
		e.r.ledger = MergeLedgers(e.r.ledger, []Entry{{Subject: "s3", Class: "lead-submission", Date: "2026-11-03"}})
	}
	if err := e.run(); err != nil || len(e.r.erased) != 3 {
		t.Fatalf("%v %v", err, e.r.erased)
	}
	e2 := newRestoreEnv(t)
	e2.r.atStart = func() { e2.r.ledgErr = errors.New("gone") }
	if err := e2.run(); err == nil || e2.r.count("complete") != 0 {
		t.Fatalf("went live without the ledger: %v", err)
	}
}

func TestRestoreFailedErasureNeverGoesLive(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.failOnce["erase"] = errors.New("tombstones cannot be replayed: forms:lead-submission")
	e.o.AcceptUnerased = []string{"finance:finance-invoice"}
	if err := e.run(); err == nil || e.r.count("complete") != 0 || len(e.r.accept) != 1 {
		t.Fatalf("err=%v accept=%v", err, e.r.accept)
	}
}

func TestRestoreDetectsCorruptDumps(t *testing.T) {
	e := newRestoreEnv(t)
	e.rs.Corrupt = map[string][]byte{tDumps + "/can-core/state.db": []byte("rotten")}
	if err := e.run(); err == nil || e.r.count("start") != 0 {
		t.Fatalf("corrupt dumps accepted: %v", err)
	}
	if e.read("var/lib/can/dumps/can-core/state.db") != "" {
		t.Fatal("corrupt dumps were put in place")
	}
}

func TestRestoreRefusesWhatItCannotComplete(t *testing.T) {
	cases := map[string]struct {
		change func(*testing.T, map[string][]byte)
		accept func(*Restore)
		want   string
	}{
		"no secrets": {func(_ *testing.T, d map[string][]byte) { delete(d, SecretsDump) }, func(o *Restore) { o.WithoutSecrets = true }, "--without-secrets"},
		"dump without restore hook": {func(t *testing.T, d map[string][]byte) {
			d[StateDump] = backuptest.StateDB(t, backuptest.Module{ID: "people", State: "enabled", Database: "sqlite"})
			d["modules/people/x.gz"] = []byte("x")
			d[DumpedList] = []byte("people sqlite\n")
		}, func(o *Restore) { o.AcceptUnrestored = []string{"people"} }, "--accept-unrestored people"},
		"database without dump": {func(t *testing.T, d map[string][]byte) {
			d[StateDump] = backuptest.StateDB(t, backuptest.Module{ID: "crm", State: "disabled", Database: "mariadb", RestoreHook: "./r.sh"})
			d[SecretsDump] = []byte(`{"version":1,"secrets":[{"namespace":"mod-crm","name":"can-db","data":{"DB_HOST":"eA=="}}]}`)
		}, func(o *Restore) { o.AcceptUnrestored = []string{"crm"} }, "DATABASE IS NOT BACKED UP"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newRestoreEnv(t)
			d := goodDumps(t)
			delete(d, "modules/wiki/storage.tar.gz")
			delete(d, "modules/forms/placeholder.txt")
			c.change(t, d)
			e.rs.Files = snapshotFiles(d, nil)
			err := e.run()
			if err == nil || !strings.Contains(err.Error(), c.want) || e.r.count("start") != 0 {
				t.Fatalf("err = %v", err)
			}
			c.accept(e.o)
			if err := e.run(); err != nil {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
}

func TestRestoreUnmatchedVolumeFails(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.volumes = nil // the forms claim was never recreated
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "mod-forms/data") || e.r.count("complete") != 0 {
		t.Fatalf("err = %v", err)
	}
	e.o.AcceptUnrestored = []string{"mod-forms/data"}
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreUnknownVolumeNeedsAcceptance(t *testing.T) {
	e := newRestoreEnv(t)
	e.rs.Files["/pvc/pvc-X_can-data_pgdata/base"] = []byte("x")
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "--accept-unrestored can-data/pgdata") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreRefusesToOverwriteExistingVolume(t *testing.T) {
	e := newRestoreEnv(t)
	put(e, "pvc/pvc-NEW9_mod-forms_data/live.db")
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") || e.read("pvc/pvc-NEW9_mod-forms_data/live.db") != "precious" {
		t.Fatalf("err = %v", err)
	}
	// An interrupted copy of our own is redone, replacing what it left.
	put(e, "pvc/pvc-NEW9_mod-forms_data/"+copyMarker)
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if e.read("pvc/pvc-NEW9_mod-forms_data/live.db") != "" {
		t.Error("a volume copy replaces the content: a stale -wal would corrupt a database")
	}
}

func TestRestoreStateBelongsToOtherRepo(t *testing.T) {
	e := newRestoreEnv(t)
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	e.o.Kit = testKit(t) // different restic password/bucket
	e.o.Kit.Restic.Repository = "s3:https://other.invalid/b/restic"
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestorePlanIsReadOnly(t *testing.T) {
	e := newRestoreEnv(t)
	steps := e.o.Plan()
	if len(steps) != 8 {
		t.Fatalf("%d steps", len(steps))
	}
	if len(e.r.events) != 0 || len(e.rs.Restores) != 0 {
		t.Fatal("plan touched dependencies")
	}
	if _, err := os.Stat(e.o.StatePath); err == nil {
		t.Fatal("plan wrote state")
	}
	e.r.failOnce["apply"] = errors.New("x")
	_ = e.run()
	done := 0
	for _, s := range e.o.Plan() {
		if s.Done {
			done++
		}
	}
	if done != 3 {
		t.Errorf("done = %d, want 3", done)
	}
}

func TestCopyTree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "a/b"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "a/b/f"), []byte("x"), 0o640)
	_ = os.Symlink("/etc/passwd", filepath.Join(src, "link"))
	if err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a/b/f")); string(b) != "x" {
		t.Error("file not copied")
	}
	if st, _ := os.Lstat(filepath.Join(dst, "link")); st == nil || st.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink not preserved as a symlink")
	}
	st, _ := os.Stat(filepath.Join(dst, "a/b/f"))
	if st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", st.Mode())
	}
}

// A link already in the destination is replaced, never written through: restore runs as root.
func TestCopyTreeDoesNotFollowLinksInTheDestination(t *testing.T) {
	src, dst, outside := t.TempDir(), t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "d"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "d/f"), []byte("new"), 0o600)
	_ = os.WriteFile(filepath.Join(src, "g"), []byte("new"), 0o600)
	victim := filepath.Join(outside, "victim")
	_ = os.WriteFile(victim, []byte("safe"), 0o600)
	_ = os.Symlink(outside, filepath.Join(dst, "d"))
	_ = os.Symlink(victim, filepath.Join(dst, "g"))
	if err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "safe" {
		t.Error("wrote through a file link")
	}
	if _, err := os.Stat(filepath.Join(outside, "f")); err == nil {
		t.Error("wrote through a directory link")
	}
}

// realRestores drops the marker probes pin() does into a scratch directory.
func (e *restoreEnv) realRestores() []RestoreCall {
	var out []RestoreCall
	for _, c := range e.rs.Restores {
		if !strings.HasSuffix(c.Includes[0], "/"+MarkerName) {
			out = append(out, c)
		}
	}
	return out
}

func okFiles(e *restoreEnv, run string) map[string][]byte {
	m := map[string][]byte{}
	for k, v := range e.rs.Files {
		m[k] = v
	}
	mk := []byte("run_id=" + run + "\nfinished_at=1800000000\n")
	m[tDumps+"/"+MarkerName] = mk
	m[tHS+"/"+MarkerCopy] = mk
	return m
}

func pinFixture(e *restoreEnv) {
	good := okFiles(e, "good")
	bad := okFiles(e, "new")
	bad[tHS+"/"+MarkerCopy] = []byte("run_id=old\nfinished_at=1800000000\n") // marker changed during backup
	noMarker := okFiles(e, "x")
	delete(noMarker, tDumps+"/"+MarkerName)
	e.rs.Snaps = []Snapshot{
		{ID: "snapBad", Time: time.Unix(400, 0), Tags: []string{"can"}},
		{ID: "snapNoMarker", Time: time.Unix(300, 0), Tags: []string{"can"}},
		{ID: "snapFlagged", Time: time.Unix(250, 0), Tags: []string{"can", "inconsistent"}},
		{ID: "snapGood", Time: time.Unix(200, 0), Tags: []string{"can"}},
		{ID: "snapForeign", Time: time.Unix(500, 0), Tags: []string{"other"}},
	}
	e.rs.SnapFiles = map[string]map[string][]byte{"snapBad": bad, "snapNoMarker": noMarker, "snapFlagged": good, "snapGood": good, "snapForeign": good}
}

func TestRestorePinPrefersConsistentSnapshot(t *testing.T) {
	e := newRestoreEnv(t)
	pinFixture(e)
	e.o.ForceOlder = true
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.realRestores() {
		if c.Snapshot != "snapGood" {
			t.Errorf("restored from %s, want the older good snapshot", c.Snapshot)
		}
	}
	out := e.out.String()
	for _, want := range []string{
		"ignoring snapshot snapForeign",
		"WARNING: --force-older: skipping snapshot snapBad",
		"WARNING: --force-older: skipping snapshot snapNoMarker",
		"skipping snapshot snapFlagged",
		"using snapshot snapGood (1970-01-01T00:03:20Z)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestRestorePinDoesNotFallBackSilently(t *testing.T) {
	e := newRestoreEnv(t)
	pinFixture(e)
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "snapBad") || !strings.Contains(err.Error(), "--force-older") {
		t.Fatalf("err = %v", err)
	}
	if len(e.realRestores()) != 0 || e.r.count("start") != 0 {
		t.Error("restored from some snapshot although the newest failed its check")
	}
	// Naming one explicitly is the other way out.
	e2 := newRestoreEnv(t)
	pinFixture(e2)
	e2.o.Snapshot = "snapGood"
	if err := e2.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e2.out.String(), "using snapshot snapGood") {
		t.Errorf("chosen snapshot not reported:\n%s", e2.out.String())
	}
}

func TestRestorePinTransientErrorDoesNotSkip(t *testing.T) {
	e := newRestoreEnv(t)
	pinFixture(e)
	e.o.ForceOlder = true // even with permission to skip, a read failure is not an inconsistency
	e.rs.RestoreErr = errors.New("connection reset by peer")
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "could not read snapshot snapBad") || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range e.rs.Restores {
		if c.Snapshot != "snapBad" {
			t.Errorf("went on to probe %s after a transient error", c.Snapshot)
		}
	}
}

func TestRestoreNoUsableSnapshot(t *testing.T) {
	e := newRestoreEnv(t)
	e.rs.Snaps = []Snapshot{{ID: "s", Time: time.Unix(1, 0), Tags: []string{"can", "inconsistent"}}}
	if err := e.run(); err == nil || e.r.count("start") != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreRequiresMarkerInRestoredDumps(t *testing.T) {
	e := newRestoreEnv(t)
	delete(e.rs.Files, tDumps+"/"+MarkerName)
	e.o.Snapshot = "snapA" // explicit, bypasses the probe: the check of the staged files must still refuse
	if err := e.run(); err == nil || e.r.count("start") != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestStagingValidation(t *testing.T) {
	for _, p := range []string{"", "/", "/tmp", "/var/lib", "relative/dir/here"} {
		if checkStaging(p) == nil {
			t.Errorf("staging %q accepted", p)
		}
	}
	dir := filepath.Join(t.TempDir(), "a", "stage")
	if err := checkStaging(dir); err != nil {
		t.Errorf("absent dir rejected: %v", err)
	}
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "important"), []byte("x"), 0o600)
	if checkStaging(dir) == nil {
		t.Error("non-empty foreign directory accepted")
	}
	_ = os.WriteFile(filepath.Join(dir, stagingMarker), nil, 0o600)
	if err := checkStaging(dir); err != nil {
		t.Errorf("marked directory rejected: %v", err)
	}
}

func TestRestoreNeverDeletesForeignStaging(t *testing.T) {
	e := newRestoreEnv(t)
	_ = os.MkdirAll(e.o.StagingDir, 0o755)
	keep := filepath.Join(e.o.StagingDir, "keep.txt")
	_ = os.WriteFile(keep, []byte("x"), 0o600)
	if err := e.run(); err == nil {
		t.Fatal("restore used a foreign staging directory")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("foreign staging content deleted")
	}
}

func TestRestoreLedgerRollbackDetected(t *testing.T) {
	e := newRestoreEnv(t)
	full := MergeLedgers(e.r.ledger, []Entry{{Subject: "s9", Class: "lead-submission", Date: "2026-11-05"}})
	l := &Ledger{Path: filepath.Join(t.TempDir(), "l")}
	for _, en := range full {
		if _, _, err := l.Append(en.Subject, en.Class); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(l.Path)
	e.rs.Files[tHS+"/"+LedgerCopy] = b
	err := e.run()
	if err == nil || !strings.Contains(err.Error(), "rolled back") || e.r.count("start") != 0 {
		t.Fatalf("err = %v", err)
	}
	e.o.AcceptLedgerBehind = true
	if err := e.run(); err != nil {
		t.Fatal(err)
	}
	if len(e.r.erased) != 3 || !strings.Contains(e.out.String(), "WARNING: --accept-ledger-behind") {
		t.Errorf("erased=%v\n%s", e.r.erased, e.out.String())
	}
}

func TestRestoreBucketLedgerMissingButSnapshotHasOne(t *testing.T) {
	e := newRestoreEnv(t)
	l := &Ledger{Path: filepath.Join(t.TempDir(), "l")}
	_, _, _ = l.Append("s1", "lead-submission")
	b, _ := os.ReadFile(l.Path)
	e.rs.Files[tHS+"/"+LedgerCopy] = b
	e.r.ledger, e.r.ledgErr = nil, ErrNoLedger
	e.o.AllowNoLedger = true // never enough when the snapshot holds a copy
	if err := e.run(); err == nil || e.r.count("start") != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreSnapshotFlagConflictOnResume(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.failOnce["apply"] = errors.New("x")
	_ = e.run()
	e.o.Snapshot = "other"
	if err := e.run(); err == nil || !strings.Contains(err.Error(), "conflicts") {
		// the failed step does not need the snapshot; a later one does
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestFileStore(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "restic")
	if !LocalRepository(repo) || LocalRepository("s3:https://x/b/restic") {
		t.Fatal("LocalRepository")
	}
	s := FileStoreFor(repo)
	ctx := context.Background()
	if _, err := s.Get(ctx, LedgerObjectKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	if err := s.Put(ctx, LedgerObjectKey, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if b, err := s.Get(ctx, LedgerObjectKey); err != nil || string(b) != "x" {
		t.Fatal(err)
	}
	if _, err := (FileStore{Dir: "/nonexistent-dir/x"}).Get(ctx, LedgerObjectKey); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a wrong path is not an empty ledger: %v", err)
	}
	if s.Put(ctx, "../x", nil) == nil {
		t.Fatal("traversal")
	}
}
