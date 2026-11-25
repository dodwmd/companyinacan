package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rec is a recording fake of every restore dependency, in one event log.
type rec struct {
	events   []string
	failOnce map[string]error
	modules  []string
	pvcDirs  []string
	ledger   []Entry
	ledgErr  error
}

func (r *rec) log(s string) { r.events = append(r.events, s) }
func (r *rec) step(name string) error {
	r.log(name)
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

func (r *rec) Provision(context.Context, *Kit) error     { return r.step("provision") }
func (r *rec) ImportState(context.Context, string) error { return r.step("import-state") }
func (r *rec) Apply(context.Context) error               { return r.step("apply") }
func (r *rec) RestoreModules(context.Context) ([]string, error) {
	return r.modules, r.step("restore-modules")
}
func (r *rec) ScaleDown(context.Context, []string) error { return r.step("scale-down") }
func (r *rec) PVCDirs(context.Context) ([]string, error) { return r.pvcDirs, nil }
func (r *rec) Purge(_ context.Context, s, c string) error {
	return r.step("purge " + s + "/" + c)
}
func (r *rec) ReapplyTombstones(context.Context, []Entry) error { return r.step("reapply") }
func (r *rec) ScaleUp(context.Context) error                    { return r.step("scale-up") }
func (r *rec) Fetch(context.Context) ([]Entry, error) {
	r.log("fetch-ledger")
	return r.ledger, r.ledgErr
}

type restoreEnv struct {
	r    *rec
	rs   *FakeRestic
	o    *Restore
	root string
	out  *bytes.Buffer
}

func newRestoreEnv(t *testing.T) *restoreEnv {
	t.Helper()
	root := t.TempDir()
	dumps := "/var/lib/can/dumps"
	hs := "/var/lib/can/host-state"
	marker := []byte("run_id=r1\nfinished_at=1800000000\n")
	files := map[string][]byte{
		dumps + "/company.yaml":                      []byte("schema: 1\n"),
		dumps + "/state.db":                          []byte("state"),
		dumps + "/modules/a.dump":                    []byte("dump-a"),
		dumps + "/" + MarkerName:                     marker,
		hs + "/" + MarkerCopy:                        marker,
		"/pvc/pvc-OLD1_mod-forms_data/uploads/f.txt": []byte("file"),
	}
	files[hs+"/"+ManifestName] = []byte(
		hashOf(files[dumps+"/company.yaml"]) + "  ./company.yaml\n" +
			hashOf(files[dumps+"/state.db"]) + "  ./state.db\n" +
			hashOf(files[dumps+"/modules/a.dump"]) + "  ./modules/a.dump\n")
	rs := &FakeRestic{Snaps: []Snapshot{{ID: "snapA", Time: time.Unix(100, 0), Tags: []string{"can"}}}, Files: files}
	r := &rec{
		failOnce: map[string]error{}, modules: []string{"forms"},
		pvcDirs: []string{"pvc-NEW9_mod-forms_data", "pvc-NEW8_mod-wiki_data"},
		ledger:  MergeLedgers(nil, []Entry{{Subject: "s1", Class: "lead-submission", Date: "2026-11-01"}, {Subject: "s2", Class: "lead-submission", Date: "2026-11-02"}}),
	}
	if err := os.MkdirAll(filepath.Join(root, "pvc", "pvc-NEW9_mod-forms_data"), 0o755); err != nil {
		t.Fatal(err)
	}
	kit := testKit(t)
	out := &bytes.Buffer{}
	o := &Restore{
		Kit: kit, Provisioner: r, Restic: rs, Cluster: r, Ledger: r,
		Root: root, DumpsDir: dumps, HostStateDir: hs, PVCRoot: "/pvc", StagingDir: filepath.Join(root, "stage"),
		CompanyDir: filepath.Join(root, "company"), StatePath: filepath.Join(root, "restore.json"), Out: out,
	}
	return &restoreEnv{r: r, rs: rs, o: o, root: root, out: out}
}

func TestRestoreSixStepOrder(t *testing.T) {
	e := newRestoreEnv(t)
	if err := e.o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := e.r
	order := []string{"provision", "import-state", "apply", "restore-modules", "scale-down", "fetch-ledger", "purge s1/lead-submission", "purge s2/lead-submission", "reapply", "scale-up"}
	last := -1
	for _, ev := range order {
		i := r.index(ev)
		if i < 0 || i <= last {
			t.Fatalf("event %q out of order or missing in %v", ev, r.events)
		}
		last = i
	}
	if r.events[len(r.events)-1] != "scale-up" {
		t.Errorf("scale-up must be last: %v", r.events)
	}
	// Dumps restored first, then the PVC area into staging.
	real := e.realRestores()
	if len(real) != 2 || real[0].Includes[0] != "/var/lib/can/dumps" || real[0].Includes[1] != "/var/lib/can/host-state" || real[1].Includes[0] != "/pvc" {
		t.Fatalf("restores = %+v", e.rs.Restores)
	}
	if real[0].Snapshot != "snapA" {
		t.Errorf("snapshot not resolved: %v", real[0].Snapshot)
	}
	// company.yaml written back, PVC data copied into the NEW volume directory.
	if b, _ := os.ReadFile(filepath.Join(e.root, "company", "company.yaml")); string(b) != "schema: 1\n" {
		t.Errorf("company.yaml = %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(e.root, "pvc", "pvc-NEW9_mod-forms_data", "uploads", "f.txt")); err != nil || string(b) != "file" {
		t.Errorf("pvc copy: %v %q", err, b)
	}
	if !strings.Contains(e.out.String(), "[6/6]") {
		t.Errorf("progress output missing: %s", e.out.String())
	}
}

func TestRestoreResumes(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.failOnce["apply"] = errors.New("boom")
	if err := e.o.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	// A newer snapshot appearing in between must not change the pinned one.
	e.rs.Snaps = append(e.rs.Snaps, Snapshot{ID: "snapB", Time: time.Unix(200, 0), Tags: []string{"can"}})
	if err := e.o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := e.r.count("provision"); n != 1 {
		t.Errorf("provision ran %d times", n)
	}
	if n := e.r.count("import-state"); n != 1 {
		t.Errorf("import-state ran %d times", n)
	}
	if n := e.r.count("apply"); n != 2 {
		t.Errorf("apply ran %d times, want 2 (failed then retried)", n)
	}
	for _, c := range e.realRestores() {
		if c.Snapshot != "snapA" {
			t.Errorf("resume used snapshot %s", c.Snapshot)
		}
	}
	if e.r.count("scale-up") != 1 {
		t.Error("scale-up did not run exactly once")
	}
	// A completed restore is a no-op when re-run.
	n := len(e.r.events)
	if err := e.o.Run(context.Background()); err != nil || len(e.r.events) != n {
		t.Errorf("re-run did work: %v %v", err, e.r.events[n:])
	}
}

func TestRestoreResumesMidLedger(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.failOnce["purge s2/lead-submission"] = errors.New("hook failed")
	if err := e.o.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if e.r.count("scale-up") != 0 {
		t.Fatal("workloads scaled up with erasures unapplied")
	}
	if err := e.o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.r.count("purge s1/") != 1 {
		t.Errorf("already-applied purge repeated: %v", e.r.events)
	}
}

func TestRestoreLedgerMissingBlocksGoingLive(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.ledger, e.r.ledgErr = nil, ErrNoLedger
	err := e.o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ledger") {
		t.Fatalf("err = %v", err)
	}
	if e.r.count("scale-up") != 0 {
		t.Fatal("scaled up without a ledger")
	}
	e.o.AllowNoLedger = true
	if err := e.o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.r.count("scale-up") != 1 {
		t.Error("scale-up missing after --allow-no-ledger")
	}
}

func TestRestoreLedgerErrorBlocks(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.ledgErr = errors.New("bucket unreachable")
	e.o.AllowNoLedger = true // only covers a MISSING ledger, never a failing fetch
	if err := e.o.Run(context.Background()); err == nil || e.r.count("scale-up") != 0 {
		t.Fatalf("err=%v events=%v", err, e.r.events)
	}
}

func TestRestoreDetectsCorruptDumpsBeforeImport(t *testing.T) {
	e := newRestoreEnv(t)
	e.rs.Corrupt = map[string][]byte{"/var/lib/can/dumps/state.db": []byte("rotten")}
	if err := e.o.Run(context.Background()); err == nil {
		t.Fatal("corrupt dumps accepted")
	}
	if e.r.count("import-state") != 0 {
		t.Fatal("state imported from corrupt dumps")
	}
}

func TestRestoreUnmatchedPVCFails(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.pvcDirs = []string{"pvc-NEW8_mod-wiki_data"} // forms claim never recreated
	err := e.o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mod-forms_data") {
		t.Fatalf("err = %v", err)
	}
	if e.r.count("scale-up") != 0 {
		t.Fatal("scaled up with unrestored data")
	}
}

func TestRestoreStateBelongsToOtherRepo(t *testing.T) {
	e := newRestoreEnv(t)
	if err := e.o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.o.Kit = testKit(t) // different restic password/bucket
	e.o.Kit.Restic.Repository = "s3:https://other.invalid/b/restic"
	if err := e.o.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestorePlanIsReadOnly(t *testing.T) {
	e := newRestoreEnv(t)
	steps := e.o.Plan()
	if len(steps) != 6 {
		t.Fatalf("%d steps", len(steps))
	}
	if len(e.r.events) != 0 || len(e.rs.Restores) != 0 {
		t.Fatal("plan touched dependencies")
	}
	if _, err := os.Stat(e.o.StatePath); err == nil {
		t.Fatal("plan wrote state")
	}
	for i, s := range steps {
		if s.ID == "" || s.Title == "" || s.Done {
			t.Errorf("step %d: %+v", i, s)
		}
	}
	// After a partial run the plan shows what is done.
	e.r.failOnce["apply"] = errors.New("x")
	_ = e.o.Run(context.Background())
	done := 0
	for _, s := range e.o.Plan() {
		if s.Done {
			done++
		}
	}
	if done != 2 {
		t.Errorf("done = %d, want 2", done)
	}
}

func TestMatchPVCs(t *testing.T) {
	m, un := MatchPVCs([]string{"pvc-a_ns_claim", "pvc-b_ns_other"}, []string{"pvc-z_ns_claim", "pvc-y_extra_thing"})
	if m["pvc-a_ns_claim"] != "pvc-z_ns_claim" || len(m) != 1 {
		t.Errorf("m = %v", m)
	}
	if len(un) != 1 || un[0] != "pvc-b_ns_other" {
		t.Errorf("unmatched = %v", un)
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
	m["/var/lib/can/dumps/"+MarkerName] = mk
	m["/var/lib/can/host-state/"+MarkerCopy] = mk
	return m
}

func pinFixture(e *restoreEnv) {
	good := okFiles(e, "good")
	bad := okFiles(e, "new")
	bad["/var/lib/can/host-state/"+MarkerCopy] = []byte("run_id=old\nfinished_at=1800000000\n") // marker changed during backup
	noMarker := okFiles(e, "x")
	delete(noMarker, "/var/lib/can/dumps/"+MarkerName)
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
	if err := e.o.Run(context.Background()); err != nil {
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
	err := e.o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "snapBad") || !strings.Contains(err.Error(), "--force-older") {
		t.Fatalf("err = %v", err)
	}
	if len(e.realRestores()) != 0 || e.r.count("import-state") != 0 {
		t.Error("restored from some snapshot although the newest failed its check")
	}
	// Naming one explicitly is the other way out.
	e2 := newRestoreEnv(t)
	pinFixture(e2)
	e2.o.Snapshot = "snapGood"
	if err := e2.o.Run(context.Background()); err != nil {
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
	err := e.o.Run(context.Background())
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
	if err := e.o.Run(context.Background()); err == nil || e.r.count("import-state") != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreRequiresMarkerInRestoredDumps(t *testing.T) {
	e := newRestoreEnv(t)
	delete(e.rs.Files, "/var/lib/can/dumps/"+MarkerName)
	e.o.Snapshot = "snapA" // explicit, bypasses the probe: checkDumps must still refuse
	if err := e.o.Run(context.Background()); err == nil || e.r.count("import-state") != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreRefusesNonBlankHost(t *testing.T) {
	e := newRestoreEnv(t)
	dumps := filepath.Join(e.root, "var/lib/can/dumps")
	_ = os.MkdirAll(dumps, 0o755)
	_ = os.WriteFile(filepath.Join(dumps, "existing.db"), []byte("precious"), 0o600)
	err := e.o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "blank host") {
		t.Fatalf("err = %v", err)
	}
	if len(e.realRestores()) != 0 {
		t.Error("restic restore ran over existing data")
	}
	e.o.ForceHost = true
	if err := e.o.Run(context.Background()); err != nil {
		t.Fatalf("--force-host: %v", err)
	}
}

func TestRestoreResumeAfterPartialDumpsIsAllowed(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.failOnce["import-state"] = errors.New("core not ready")
	if err := e.o.Run(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if err := e.o.Run(context.Background()); err != nil {
		t.Fatalf("resume refused its own restored dumps: %v", err)
	}
	if n := len(e.realRestores()); n != 2 {
		t.Errorf("dumps restored again on resume: %d restores", n)
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
	if err := e.o.Run(context.Background()); err == nil {
		t.Fatal("restore used a foreign staging directory")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("foreign staging content deleted")
	}
}

func TestRestoreRefusesToOverwriteExistingVolume(t *testing.T) {
	e := newRestoreEnv(t)
	vol := filepath.Join(e.root, "pvc", "pvc-NEW9_mod-forms_data")
	_ = os.WriteFile(filepath.Join(vol, "live.db"), []byte("live data"), 0o600)
	err := e.o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(vol, "live.db")); string(b) != "live data" {
		t.Error("existing volume data touched")
	}
	e.o.ForceVolumes = true
	if err := e.o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreLedgerRollbackDetected(t *testing.T) {
	e := newRestoreEnv(t)
	snapLedger := MergeLedgers(nil, []Entry{{Subject: "s1", Class: "lead-submission", Date: "2026-11-01"}, {Subject: "s9", Class: "lead-submission", Date: "2026-11-05"}})
	var b strings.Builder
	for _, en := range snapLedger {
		j, _ := json.Marshal(en)
		b.Write(j)
		b.WriteString("\n")
	}
	e.rs.Files["/var/lib/can/host-state/"+LedgerCopy] = []byte(b.String())
	// The bucket copy lacks s9 (older than the snapshot's own copy).
	err := e.o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "s9/lead-submission") {
		t.Fatalf("err = %v", err)
	}
	if e.r.count("scale-up") != 0 {
		t.Fatal("went live with a rolled-back ledger")
	}
	// If the bucket is ahead, the union is applied (snapshot-only entries included).
	e2 := newRestoreEnv(t)
	e2.rs.Files["/var/lib/can/host-state/"+LedgerCopy] = []byte(b.String())
	e2.r.ledger = MergeLedgers(e2.r.ledger, snapLedger)
	if err := e2.o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e2.r.count("purge s9/") != 1 {
		t.Errorf("union not applied: %v", e2.r.events)
	}
}

func TestRestoreBucketLedgerMissingButSnapshotHasOne(t *testing.T) {
	e := newRestoreEnv(t)
	j, _ := json.Marshal(MergeLedgers(nil, []Entry{{Subject: "s1", Class: "c", Date: "2026-11-01"}})[0])
	e.rs.Files["/var/lib/can/host-state/"+LedgerCopy] = append(j, '\n')
	e.r.ledger, e.r.ledgErr = nil, ErrNoLedger
	e.o.AllowNoLedger = true // must NOT be enough when the snapshot proves a ledger existed
	if err := e.o.Run(context.Background()); err == nil || e.r.count("scale-up") != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreSnapshotFlagConflictOnResume(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.failOnce["apply"] = errors.New("x")
	_ = e.o.Run(context.Background())
	e.o.Snapshot = "other"
	if err := e.o.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreLedgerAppliedKeyedBySubjectAndClass(t *testing.T) {
	e := newRestoreEnv(t)
	e.r.ledger = MergeLedgers(nil, []Entry{{Subject: "s1", Class: "a", Date: "2026-11-01"}, {Subject: "s1", Class: "b", Date: "2026-11-02"}})
	e.r.failOnce["purge s1/b"] = errors.New("hook failed")
	_ = e.o.Run(context.Background())
	_ = e.o.Run(context.Background())
	if e.r.count("purge s1/a") != 1 || e.r.count("purge s1/b") != 2 {
		t.Errorf("events = %v", e.r.events)
	}
}

func TestRestoreAcceptLedgerBehindWarnsLoudly(t *testing.T) {
	e := newRestoreEnv(t)
	snapLedger := MergeLedgers(nil, []Entry{{Subject: "s9", Class: "c", Date: "2026-11-05"}})
	j, _ := json.Marshal(snapLedger[0])
	e.rs.Files["/var/lib/can/host-state/"+LedgerCopy] = append(j, '\n')
	if err := e.o.Run(context.Background()); err == nil {
		t.Fatal("expected refusal")
	}
	e.o.AcceptLedgerBehind = true
	if err := e.o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "WARNING: --accept-ledger-behind") {
		t.Errorf("no warning:\n%s", e.out.String())
	}
}
