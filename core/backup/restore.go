package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Provisioner brings up a blank host: provision, k3s, can-core in restore mode
// (SPEC.md section 6.5 step 1). Implementations must be idempotent.
type Provisioner interface {
	Provision(ctx context.Context, k *Kit) error
}

// Cluster is everything restore needs from the freshly started can-core and
// k3s. can-core (epic E03) provides the real implementation; tests use a fake.
type Cluster interface {
	// ImportState lets can-core import its own state from the restored dumps
	// directory, re-create the exported Secrets and namespaces, and leave restore mode.
	ImportState(ctx context.Context, dumpsDir string) error
	// Apply runs `can apply` from the restored company.yaml as a restore apply
	// (install seeding hook skipped); modules come up with empty PVCs.
	Apply(ctx context.Context) error
	// RestoreModules runs each module's replace-all restore hook and leaves the
	// module scaled to 0. It returns the module ids.
	RestoreModules(ctx context.Context) ([]string, error)
	// ScaleDown scales the modules' workloads to 0.
	ScaleDown(ctx context.Context, modules []string) error
	// PVCDirs lists the local-path directory names (pvc-<uid>_<ns>_<claim>) of
	// the volumes that exist on this cluster.
	PVCDirs(ctx context.Context) ([]string, error)
	// Purge re-runs a module's purge(subject, class) hook on the restored data.
	Purge(ctx context.Context, subject, class string) error
	// ReapplyTombstones re-applies the tombstones to can-core's own restored state.
	ReapplyTombstones(ctx context.Context, es []Entry) error
	// ScaleUp scales the workloads up: the moment the company goes live.
	ScaleUp(ctx context.Context) error
}

// LedgerSource fetches the erasure ledger; it returns ErrNoLedger when the
// bucket holds none.
type LedgerSource interface {
	Fetch(ctx context.Context) ([]Entry, error)
}

// BucketLedger fetches the ledger from the bucket named in the kit.
type BucketLedger struct {
	Store    ObjectStore
	Password string
}

// Fetch implements LedgerSource.
func (b BucketLedger) Fetch(ctx context.Context) ([]Entry, error) {
	return FetchLedger(ctx, b.Store, b.Password)
}

// Restore is the six-step restore of SPEC.md section 6.5 as a resumable state
// machine. Each completed step is recorded in StatePath; a failed step is
// retried on the next Run and completed steps are skipped.
type Restore struct {
	Kit         *Kit
	Provisioner Provisioner
	Restic      ResticAPI
	Cluster     Cluster
	Ledger      LedgerSource

	Root         string // host root the dumps are restored under (default "/")
	DumpsDir     string
	HostStateDir string // holds the manifest, marker copy and ledger copy (default Settings.HostStateDir)
	PVCRoot      string
	StagingDir   string // scratch for the PVC restore (must have room for the data)
	CompanyDir   string // company.yaml is written back here ("" skips)
	StatePath    string
	Snapshot     string // "" = latest, pinned on first run

	AllowNoLedger bool // proceed when the bucket has no ledger at all
	// Each override is separate and dangerous in its own way.
	ForceHost          bool // restore over a non-blank host (overwrites existing files)
	ForceVolumes       bool // copy into volumes that already hold data
	AcceptLedgerBehind bool // apply the union although the bucket ledger lacks snapshot entries
	ForceOlder         bool // skip a newer `can` snapshot that fails its consistency check
	Out                io.Writer
}

// Restore step ids, in order.
const (
	StepProvision = "provision"
	StepDumps     = "dumps"
	StepApply     = "apply"
	StepHooks     = "module-hooks"
	StepPVCs      = "pvcs"
	StepErasure   = "erasure"
)

var stepTitles = []struct{ id, title, detail string }{
	{StepProvision, "Provision host, install k3s, start can-core in restore mode", "provider bootstrap from the kit's provider info; idempotent"},
	{StepDumps, "Restore the dumps directory first; can-core imports its state and Secrets", "restic restore <dumps dir>, manifest hash check, then can-core state import (admin credentials and TOTP seed preserved)"},
	{StepApply, "Write company.yaml back and run a restore apply", "namespaces and modules with empty PVCs; install seeding skipped; Secrets reused"},
	{StepHooks, "Run each module's replace-all restore hook", "database dumps loaded; modules left scaled to 0"},
	{StepPVCs, "Restore PVC data into the new volumes", "workloads at 0; restic restore to staging; copy matched on <namespace>_<claim>"},
	{StepErasure, "Re-apply the erasure tombstone ledger, then scale up", "ledger fetched and decrypted with the restic password; purge hooks re-run; only then workloads scale up"},
}

type stepState struct {
	Done     bool      `json:"done"`
	Finished time.Time `json:"finished,omitempty"`
	Error    string    `json:"error,omitempty"`
}

type restoreState struct {
	Version     int                  `json:"version"`
	Fingerprint string               `json:"fingerprint"`
	Snapshot    string               `json:"snapshot"`
	Steps       map[string]stepState `json:"steps"`
	Modules     []string             `json:"modules,omitempty"`
	DumpsDone   bool                 `json:"dumps_restored,omitempty"`
	Applied     []string             `json:"ledger_applied,omitempty"` // "subject\x00class" keys already purged
}

// PlanStep is one line of `can restore --plan`.
type PlanStep struct {
	N      int    `json:"n"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Done   bool   `json:"done"`
}

func (o *Restore) fingerprint() string {
	h := sha256.Sum256([]byte(o.Kit.Restic.Repository + "\x00" + o.Kit.Restic.Password))
	return hex.EncodeToString(h[:8])
}

func (o *Restore) root() string {
	if o.Root == "" {
		return "/"
	}
	return o.Root
}

func (o *Restore) hostPath(p string) string { return filepath.Join(o.root(), filepath.FromSlash(p)) }

func (o *Restore) load() (*restoreState, error) {
	st := &restoreState{Version: 1, Steps: map[string]stepState{}}
	b, err := os.ReadFile(o.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("restore state %s is corrupt: %w (delete it to start over)", o.StatePath, err)
	}
	if st.Steps == nil {
		st.Steps = map[string]stepState{}
	}
	if st.Fingerprint != "" && st.Fingerprint != o.fingerprint() {
		return nil, fmt.Errorf("restore state %s belongs to a different repository or kit; delete it to start a new restore", o.StatePath)
	}
	return st, nil
}

func (o *Restore) save(st *restoreState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(o.StatePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := o.StatePath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // operator-chosen path
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, o.StatePath); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil { //nolint:gosec // same directory
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Plan lists the six steps and which are already done. It touches nothing
// (reading the state file is the only I/O).
func (o *Restore) Plan() []PlanStep {
	st, err := o.load()
	if err != nil {
		st = &restoreState{Steps: map[string]stepState{}}
	}
	out := make([]PlanStep, len(stepTitles))
	for i, s := range stepTitles {
		out[i] = PlanStep{N: i + 1, ID: s.id, Title: s.title, Detail: s.detail, Done: st.Steps[s.id].Done}
	}
	return out
}

func (o *Restore) say(format string, a ...any) {
	if o.Out != nil {
		_, _ = fmt.Fprintf(o.Out, format+"\n", a...)
	}
}

// Run executes the remaining steps.
func (o *Restore) Run(ctx context.Context) error {
	st, err := o.load()
	if err != nil {
		return err
	}
	st.Fingerprint = o.fingerprint()
	impls := map[string]func(context.Context, *restoreState) error{
		StepProvision: o.stepProvision, StepDumps: o.stepDumps, StepApply: o.stepApply,
		StepHooks: o.stepHooks, StepPVCs: o.stepPVCs, StepErasure: o.stepErasure,
	}
	for i, s := range stepTitles {
		if st.Steps[s.id].Done {
			o.say("[%d/6] %s: already done", i+1, s.title)
			continue
		}
		o.say("[%d/6] %s", i+1, s.title)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := impls[s.id](ctx, st); err != nil {
			st.Steps[s.id] = stepState{Error: err.Error()}
			_ = o.save(st)
			return fmt.Errorf("restore step %d (%s) failed: %w; fix the cause and run the same command again to resume", i+1, s.id, err)
		}
		st.Steps[s.id] = stepState{Done: true, Finished: time.Now().UTC()}
		if err := o.save(st); err != nil {
			return err
		}
	}
	return nil
}

func (o *Restore) stepProvision(ctx context.Context, _ *restoreState) error {
	return o.Provisioner.Provision(ctx, o.Kit)
}

func (o *Restore) hostState() string {
	if o.HostStateDir == "" {
		return DefaultSettings().HostStateDir
	}
	return o.HostStateDir
}

// markersAt reads the dumps marker and the copy the backup took under, both
// below root, and requires them to agree (same run id and finish time).
func (o *Restore) markersAt(root string) (Marker, error) {
	m, err := o.markersAtRaw(root)
	if err != nil {
		return m, errInconsistent{err}
	}
	return m, nil
}

func (o *Restore) markersAtRaw(root string) (Marker, error) {
	under := func(p string) string { return filepath.Join(root, filepath.FromSlash(p)) }
	m, err := ReadMarker(filepath.Join(under(o.DumpsDir), MarkerName))
	if err != nil {
		return m, fmt.Errorf("snapshot dumps: %w", err)
	}
	c, err := ReadMarker(filepath.Join(under(o.hostState()), MarkerCopy))
	if err != nil {
		return m, fmt.Errorf("snapshot host state: %w", err)
	}
	if c.RunID != m.RunID || !c.FinishedAt.Equal(m.FinishedAt) {
		return m, fmt.Errorf("dumps marker (run %s) differs from the marker the backup ran under (run %s): inconsistent snapshot", m.RunID, c.RunID)
	}
	return m, nil
}

// errInconsistent marks a snapshot whose own contents fail the consistency
// check (as opposed to a transient failure to read it).
type errInconsistent struct{ err error }

func (e errInconsistent) Error() string { return e.err.Error() }
func (e errInconsistent) Unwrap() error { return e.err }

// pin resolves and records the snapshot once, so a resume never mixes
// snapshots. Without --snapshot it walks the snapshots newest first, never
// silently:
//   - tagged "inconsistent" (the backup itself flagged it): skipped, reported;
//   - not tagged "can": ignored, reported;
//   - a transient restic failure while checking it: the restore fails (retry);
//   - a genuine inconsistency (marker missing or mismatching) in a newer "can"
//     snapshot: the restore fails unless ForceOlder is set or --snapshot names one.
//
// The chosen snapshot id and time are always reported.
func (o *Restore) pin(ctx context.Context, st *restoreState) (string, error) {
	if st.Snapshot != "" {
		if o.Snapshot != "" && o.Snapshot != st.Snapshot {
			return "", fmt.Errorf("this restore was started with snapshot %s; --snapshot %s conflicts (delete %s to start over)", st.Snapshot, o.Snapshot, o.StatePath)
		}
		return st.Snapshot, nil
	}
	if o.Snapshot != "" {
		st.Snapshot = o.Snapshot
		o.say("using snapshot %s (chosen with --snapshot)", o.Snapshot)
		return st.Snapshot, o.save(st)
	}
	snaps, err := o.Restic.Snapshots(ctx)
	if err != nil {
		return "", err
	}
	tried := 0
	for _, sn := range snaps {
		when := sn.Time.UTC().Format(time.RFC3339)
		if !slices.Contains(sn.Tags, "can") {
			o.say("ignoring snapshot %s (%s): not tagged can", sn.ID, when)
			continue
		}
		if slices.Contains(sn.Tags, "inconsistent") {
			o.say("skipping snapshot %s (%s): tagged inconsistent by the backup (the dump marker changed while it ran)", sn.ID, when)
			continue
		}
		if tried++; tried > 10 {
			return "", errors.New("no usable snapshot among the newest 10; use --snapshot to choose one")
		}
		tmp, err := os.MkdirTemp("", "can-probe-")
		if err != nil {
			return "", err
		}
		err = o.Restic.Restore(ctx, sn.ID, tmp, []string{path.Join(o.DumpsDir, MarkerName), path.Join(o.hostState(), MarkerCopy)})
		if err != nil {
			_ = os.RemoveAll(tmp)
			return "", fmt.Errorf("could not read snapshot %s (%s) to check it (not skipping it; fix the cause and re-run, or choose with --snapshot): %w", sn.ID, when, err)
		}
		_, err = o.markersAt(tmp)
		_ = os.RemoveAll(tmp)
		if err != nil {
			if !o.ForceOlder {
				return "", fmt.Errorf("the newest usable-looking snapshot %s (%s) fails its consistency check: %v; refusing to fall back to an older one silently (use --force-older to skip it, or --snapshot ID)", sn.ID, when, err)
			}
			o.say("WARNING: --force-older: skipping snapshot %s (%s): %v", sn.ID, when, err)
			continue
		}
		o.say("using snapshot %s (%s)", sn.ID, when)
		st.Snapshot = sn.ID
		return sn.ID, o.save(st)
	}
	return "", errors.New("repository has no usable snapshot (tagged can and not inconsistent)")
}

const dumpsGuardMarker = ".can-restore-in-progress"

// blankHost refuses to restore over existing data: the dumps and host-state
// directories must be absent or empty, or hold our in-progress marker.
func (o *Restore) blankHost() error {
	dumps := o.hostPath(o.DumpsDir)
	if _, err := os.Stat(filepath.Join(dumps, dumpsGuardMarker)); err == nil {
		return nil // resuming our own partial restore
	}
	for _, d := range []string{dumps, o.hostPath(o.hostState())} {
		ents, err := os.ReadDir(d)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(ents) > 0 && !o.ForceHost {
			return fmt.Errorf("%s is not empty: this does not look like a blank host (use --force-host to restore over it; existing files will be overwritten)", d)
		}
	}
	return nil
}

func (o *Restore) stepDumps(ctx context.Context, st *restoreState) error {
	snap, err := o.pin(ctx, st)
	if err != nil {
		return err
	}
	if !st.DumpsDone {
		if err := o.blankHost(); err != nil {
			return err
		}
		dumps := o.hostPath(o.DumpsDir)
		if err := os.MkdirAll(dumps, 0o700); err != nil {
			return err
		}
		guard := filepath.Join(dumps, dumpsGuardMarker)
		if err := os.WriteFile(guard, nil, 0o600); err != nil {
			return err
		}
		if err := o.Restic.Restore(ctx, snap, o.root(), []string{o.DumpsDir, o.hostState()}); err != nil {
			return err
		}
		if err := o.checkDumps(); err != nil {
			return err
		}
		_ = os.Remove(guard)
		st.DumpsDone = true
		if err := o.save(st); err != nil {
			return err
		}
	}
	return o.Cluster.ImportState(ctx, o.DumpsDir)
}

// checkDumps requires a consistent completion marker and hashes every restored
// dump against the manifest taken at backup time.
func (o *Restore) checkDumps() error {
	if _, err := o.markersAt(o.root()); err != nil {
		return err
	}
	dir := o.hostPath(o.DumpsDir)
	raw, err := os.ReadFile(filepath.Join(o.hostPath(o.hostState()), ManifestName)) //nolint:gosec // our restore root
	if err != nil {
		return fmt.Errorf("restored host state has no %s: %w", ManifestName, err)
	}
	man, err := ParseManifest(raw)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(man))
	for n := range man {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sum, err := hashFile(filepath.Join(dir, filepath.FromSlash(n)))
		if err != nil {
			return fmt.Errorf("restored dump %s: %w", n, err)
		}
		if sum != man[n] {
			return fmt.Errorf("restored dump %s does not match the backup manifest", n)
		}
	}
	return nil
}

func (o *Restore) stepApply(ctx context.Context, _ *restoreState) error {
	if o.CompanyDir != "" {
		src := filepath.Join(o.hostPath(o.DumpsDir), "company.yaml")
		data, err := os.ReadFile(src) //nolint:gosec // our restore root
		if err != nil {
			return fmt.Errorf("restored dumps have no company.yaml: %w", err)
		}
		dst := filepath.Join(o.CompanyDir, "company.yaml")
		switch existing, err := os.ReadFile(dst); { //nolint:gosec // operator-chosen dir
		case err == nil && string(existing) != string(data):
			return fmt.Errorf("%s exists and differs from the backed-up company.yaml; refusing to overwrite", dst)
		case err == nil:
		case errors.Is(err, os.ErrNotExist):
			if err := os.MkdirAll(o.CompanyDir, 0o755); err != nil { //nolint:gosec // company repo dir
				return err
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil { //nolint:gosec // company.yaml holds no secrets
				return err
			}
		default:
			return err
		}
	}
	return o.Cluster.Apply(ctx)
}

func (o *Restore) stepHooks(ctx context.Context, st *restoreState) error {
	mods, err := o.Cluster.RestoreModules(ctx)
	if err != nil {
		return err
	}
	st.Modules = mods
	return nil
}

const stagingMarker = ".can-restore-staging"
const copyMarker = ".can-restore-copying"

// checkStaging refuses root, shallow paths and non-empty unmarked directories:
// the staging directory is deleted, so it must be ours.
func checkStaging(p string) error {
	if p == "" {
		return errors.New("staging directory not set")
	}
	c := filepath.Clean(p)
	if !filepath.IsAbs(c) {
		return fmt.Errorf("staging directory %q must be absolute", p)
	}
	if n := len(strings.FieldsFunc(c, func(r rune) bool { return r == filepath.Separator })); n < 3 {
		return fmt.Errorf("staging directory %q is too shallow to be safe to delete (need at least 3 path components)", p)
	}
	ents, err := os.ReadDir(c)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(ents) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(c, stagingMarker)); err != nil {
		return fmt.Errorf("staging directory %s is not empty and was not created by can restore; refusing to delete it", c)
	}
	return nil
}

// copyVolume copies one staged volume into the new one. A destination that
// already holds data is refused unless it carries our in-progress marker (an
// earlier interrupted copy) or ForceVolumes is set.
func (o *Restore) copyVolume(src, dst string) error {
	guard := filepath.Join(dst, copyMarker)
	if _, err := os.Stat(guard); err != nil {
		ents, rerr := os.ReadDir(dst)
		if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return rerr
		}
		if len(ents) > 0 && !o.ForceVolumes {
			return fmt.Errorf("volume %s already holds data; refusing to overwrite it (use --force-volumes)", dst)
		}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // CopyTree sets modes
		return err
	}
	if err := os.WriteFile(guard, nil, 0o600); err != nil {
		return err
	}
	if err := CopyTree(src, dst); err != nil {
		return err
	}
	return os.Remove(guard)
}

func (o *Restore) stepPVCs(ctx context.Context, st *restoreState) error {
	snap, err := o.pin(ctx, st)
	if err != nil {
		return err
	}
	if err := checkStaging(o.StagingDir); err != nil {
		return err
	}
	if err := o.Cluster.ScaleDown(ctx, st.Modules); err != nil {
		return err
	}
	if err := os.RemoveAll(o.StagingDir); err != nil {
		return err
	}
	if err := os.MkdirAll(o.StagingDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.StagingDir, stagingMarker), nil, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(o.StagingDir) }()
	if err := o.Restic.Restore(ctx, snap, o.StagingDir, []string{o.PVCRoot}); err != nil {
		return err
	}
	stagedRoot := filepath.Join(o.StagingDir, filepath.FromSlash(o.PVCRoot))
	entries, err := os.ReadDir(stagedRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil // snapshot holds no PVC data
	}
	if err != nil {
		return err
	}
	var backedUp []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "pvc-") {
			backedUp = append(backedUp, e.Name())
		}
	}
	current, err := o.Cluster.PVCDirs(ctx)
	if err != nil {
		return err
	}
	matched, unmatched := MatchPVCs(backedUp, current)
	if len(unmatched) > 0 {
		return fmt.Errorf("backed-up volumes with no matching claim on the new cluster (not restored): %s", strings.Join(unmatched, ", "))
	}
	froms := make([]string, 0, len(matched))
	for from := range matched {
		froms = append(froms, from)
	}
	sort.Strings(froms)
	for _, from := range froms {
		if err := o.copyVolume(filepath.Join(stagedRoot, from), filepath.Join(o.hostPath(o.PVCRoot), matched[from])); err != nil {
			return fmt.Errorf("copying %s: %w", from, err)
		}
	}
	return nil
}

func (o *Restore) stepErasure(ctx context.Context, st *restoreState) error {
	// The snapshot carries its own copy of the ledger: a bucket copy that is
	// shorter than it has been rolled back or truncated.
	snapEntries, err := (&Ledger{Path: filepath.Join(o.hostPath(o.hostState()), LedgerCopy)}).Read()
	if err != nil {
		return fmt.Errorf("ledger copy inside the snapshot: %w", err)
	}
	es, err := o.Ledger.Fetch(ctx)
	switch {
	case errors.Is(err, ErrNoLedger):
		if len(snapEntries) > 0 && !o.AcceptLedgerBehind {
			return fmt.Errorf("%w, but the snapshot's copy holds %d entries: the bucket object was deleted or rolled back; refusing to go live (--accept-ledger-behind to apply the snapshot's copy only)", err, len(snapEntries))
		}
		if len(snapEntries) == 0 && !o.AllowNoLedger {
			return fmt.Errorf("%w: refusing to go live without applying erasures (re-run with --allow-no-ledger if no erasure was ever requested)", err)
		}
		es = nil
	case err != nil:
		return fmt.Errorf("fetching the erasure ledger: %w", err)
	}
	if errors.Is(err, ErrNoLedger) && len(snapEntries) > 0 {
		o.say("WARNING: --accept-ledger-behind: the bucket has no ledger; applying only the snapshot's %d entries (erasures made after that backup are NOT applied)", len(snapEntries))
	}
	merged, err := CheckNotBehind(es, snapEntries)
	if err != nil {
		if !o.AcceptLedgerBehind {
			return err
		}
		o.say("WARNING: --accept-ledger-behind: %v", err)
		merged = MergeLedgers(es, snapEntries)
	}
	applied := map[string]bool{}
	for _, k := range st.Applied {
		applied[k] = true
	}
	for _, e := range merged {
		if applied[e.key()] {
			continue
		}
		if err := o.Cluster.Purge(ctx, e.Subject, e.Class); err != nil {
			return fmt.Errorf("purge %s/%s: %w", e.Subject, e.Class, err)
		}
		st.Applied = append(st.Applied, e.key())
		if err := o.save(st); err != nil {
			return err
		}
	}
	if err := o.Cluster.ReapplyTombstones(ctx, merged); err != nil {
		return err
	}
	return o.Cluster.ScaleUp(ctx)
}

// MatchPVCs pairs backed-up local-path directories with the new cluster's by
// their <namespace>_<claim> suffix (the UID prefix differs on a fresh cluster).
// unmatched lists backed-up directories that found no partner.
func MatchPVCs(backedUp, current []string) (matched map[string]string, unmatched []string) {
	suffix := func(n string) string {
		_, s, _ := strings.Cut(n, "_")
		return s
	}
	cur := map[string]string{}
	for _, c := range current {
		cur[suffix(c)] = c
	}
	matched = map[string]string{}
	for _, b := range backedUp {
		if to, ok := cur[suffix(b)]; ok && suffix(b) != "" {
			matched[b] = to
		} else {
			unmatched = append(unmatched, b)
		}
	}
	sort.Strings(unmatched)
	return matched, unmatched
}

// CopyTree copies src into dst (created if needed), preserving permissions and
// symlinks (links are recreated, never followed).
func CopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()|0o700); err != nil {
				return err
			}
			chownLike(target, info)
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(l, target); err != nil {
				return err
			}
			chownLike(target, info)
			return nil
		case info.Mode().IsRegular():
			in, err := os.Open(p) //nolint:gosec // staged restore data
			if err != nil {
				return err
			}
			defer func() { _ = in.Close() }()
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm()) //nolint:gosec // volume dir
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			chownLike(target, info)
			return os.Chmod(target, info.Mode().Perm())
		}
		return nil // devices, sockets: not restored
	})
}
