package backup

import (
	"context"
	"crypto/rand"
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

	"go.yaml.in/yaml/v3"
)

// Restore (specs/042-restore) runs on the server being restored, as root, next to its k3s. The
// host half (restic, the dumps directory, the volume directories, the ledger) is done here; the
// cluster half goes through Cluster. can-core stays in restore mode, where it sends nothing and
// serves nobody, from the moment it first starts until Complete.

// StartOptions starts the platform with can-core in restore mode.
type StartOptions struct {
	ID          string // identifies this restore; can-core stays in restore mode until Complete
	CompanyYAML []byte
	Overwrite   bool // an existing can-core state may be replaced (confirmed by the operator)
}

// VolumeRef names a claim whose backed-up files are to be copied back.
type VolumeRef struct{ Namespace, Claim string }

func (v VolumeRef) String() string { return v.Namespace + "/" + v.Claim }

// Volume is a claim and the host directory behind it.
type Volume struct {
	VolumeRef
	Dir string
}

// ApplyReport is what recreating the modules did.
type ApplyReport struct {
	Modules  []string `json:"modules"`
	Secrets  int      `json:"secrets"`
	Warnings []string `json:"warnings,omitempty"`
}

// HookOptions are the overrides of the restore hooks.
type HookOptions struct {
	// Accept are modules whose dump may stay unloaded.
	Accept []string `json:"accept,omitempty"`
	// MissingBlobs are modules (files, jobs) restored although files their database names are not
	// in the volume copy.
	MissingBlobs []string `json:"missing_blobs,omitempty"`
}

// HookReport is what the module restore hooks did.
type HookReport struct {
	Restored []string `json:"restored"`
	Warnings []string `json:"warnings,omitempty"`
}

// EraseReport is what replaying the erasure ledger did.
type EraseReport struct {
	// Purged are "module: class subject" lines.
	Purged []string `json:"purged"`
	// Reflected counts the erasures older than the dump they would apply to: the dump already
	// reflects them, so they are not replayed into it.
	Reflected int `json:"reflected,omitempty"`
	// SkippedByHand lists the entries made by hand among them.
	SkippedByHand []string `json:"skipped_by_hand,omitempty"`
	// Unapplied are "module:class" pairs ("*:class" when no installed module holds the class)
	// for which tombstones exist but there is no way to replay them.
	Unapplied []string `json:"unapplied,omitempty"`
	// CoreRows counts rows removed from can-core's own state.
	CoreRows int `json:"core_rows"`
}

// CompleteReport is what leaving restore mode will bring back up.
type CompleteReport struct {
	Modules []string `json:"modules"`
}

// Cluster is everything restore needs from k3s and can-core. Every method must be idempotent: a
// failed restore is resumed by calling it again.
type Cluster interface {
	// Halt stops a can-core that already runs on a server being overwritten, so that it cannot
	// dump over the dumps being restored; Start brings it back.
	Halt(ctx context.Context) error
	// Mode is what a can-core already on this server says about itself: "" when none is
	// deployed, "restoring <id>", "normal restored <id>", "normal", or "unknown" when it is
	// deployed but cannot be asked.
	Mode(ctx context.Context) (string, error)
	// Inspect lists what already runs on this server that a restore would overwrite; nothing for a
	// freshly bootstrapped k3s.
	Inspect(ctx context.Context) ([]string, error)
	// Start applies the platform and deploys can-core in restore mode; can-core imports its state
	// from the dumps directory.
	Start(ctx context.Context, o StartOptions) error
	// ApplyModules recreates the Secrets and every recorded module with its workloads at 0.
	ApplyModules(ctx context.Context, overwrite bool) (ApplyReport, error)
	// Volumes makes sure the claims exist on disk and returns their directories.
	Volumes(ctx context.Context, want []VolumeRef) ([]Volume, error)
	// RunHooks runs the replace-all restore hook of every module with a dump.
	RunHooks(ctx context.Context, o HookOptions) (HookReport, error)
	// Erase replays the tombstones against the restored data. It fails, naming them, when a
	// tombstone cannot be replayed and is not listed in accept.
	Erase(ctx context.Context, es []Entry, accept []string) (EraseReport, error)
	// Complete leaves restore mode and scales the modules up: the moment the company goes live.
	Complete(ctx context.Context) error
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

// Restore is the resumable restore state machine. Each completed step is recorded in StatePath;
// a failed step is retried on the next Run and completed steps are skipped.
type Restore struct {
	Kit     *Kit
	Restic  ResticAPI
	Cluster Cluster
	Ledger  LedgerSource

	Root         string // prefix of every host path (default "/")
	DumpsDir     string
	HostStateDir string // path inside the snapshot (manifest, marker and ledger copies)
	PVCRoot      string
	StagingDir   string // scratch holding the restored snapshot (needs room for it); absolute
	CompanyDir   string // company.yaml is written back here ("" skips)
	ConfigDir    string // restic.password and restic.env are written here ("" skips)
	LedgerPath   string // the server's erasure ledger ("" skips)
	StatePath    string
	Snapshot     string // "" = latest, pinned on first run
	// DirChosen says CompanyDir was asked for (--dir), not a default that follows the shell.
	DirChosen bool
	// DirChosen says CompanyDir was asked for (--dir), not a default that follows the shell.
	Source string
	Now    func() time.Time // default time.Now

	// Each override is separate and dangerous in its own way.
	ConfirmOverwrite   string   // the company name: restore over a server that is not blank
	AllowNoLedger      bool     // proceed when the bucket has no ledger at all
	AcceptLedgerBehind bool     // apply the union although the bucket ledger lacks snapshot entries
	ForceOlder         bool     // skip a newer `can` snapshot that fails its consistency check
	WithoutSecrets     bool     // restore a set without a Secrets export
	AcceptUnrestored   []string // module ids, or namespace/claim volumes, that may stay unrestored
	AcceptUnerased     []string // module:class tombstones that may stay unapplied
	AllowMissingBlobs  []string // modules restored without files destroyed after their dump
	// AcceptCompany restores a snapshot whose company.yaml names another company than the kit.
	AcceptCompany string

	// StopTimer stops the backup timer of a server being overwritten (nil: nothing to stop).
	StopTimer func(ctx context.Context) error
	// Chown hands a restored dumps tree to can-core's user (nil: leave ownership alone).
	Chown func(dir string, uid int) error
	Out   io.Writer
}

// Restore step ids, in order.
const (
	StepFetch    = "fetch"
	StepDumps    = "dumps"
	StepPlatform = "platform"
	StepApply    = "apply"
	StepVolumes  = "volumes"
	StepHooks    = "module-hooks"
	StepErasure  = "erasure"
	StepLive     = "live"
)

var stepTitles = []struct{ id, title, detail string }{
	{StepFetch, "Check the server is blank; fetch the snapshot and the erasure ledger and check them", "restic restore into the staging directory; marker, manifest hashes, state database; refuses now what could not be completed"},
	{StepDumps, "Put the dumps in place and write company.yaml back", "no completion marker is restored, so nothing can back up a half-restored server"},
	{StepPlatform, "Deploy the platform and can-core in restore mode", "can-core imports its state (admin login and TOTP preserved); no email, webhooks, dump runs or admin UI"},
	{StepApply, "Re-create the exported Secrets and every module, with workloads at 0", "no public routes, no seeding Jobs"},
	{StepVolumes, "Copy the backed-up volume files into the new volumes", "matched on <namespace>_<claim>; ownership preserved"},
	{StepHooks, "Run each module's replace-all restore hook", "dumps are downloaded from can-core by the module's own hook"},
	{StepErasure, "Replay the erasure tombstone ledger", "the module purge of every tombstoned subject and class; can-core's own derived copies"},
	{StepLive, "Leave restore mode and scale the modules up", "writes the ledger and backup credentials to the server first"},
}

// canCoreUID owns the dumps directory (deploy/can-core).
const canCoreUID = 65532

type stepState struct {
	Done     bool      `json:"done"`
	Finished time.Time `json:"finished,omitempty"`
	Error    string    `json:"error,omitempty"`
}

type restoreState struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
	ID          string `json:"id"`
	Snapshot    string `json:"snapshot"`
	// Claimed means the server was checked and is now this restore's; Overwrite that it was not
	// blank and the operator confirmed.
	Claimed   bool                 `json:"claimed,omitempty"`
	Overwrite bool                 `json:"overwrite,omitempty"`
	Steps     map[string]stepState `json:"steps"`
	Entries   []Entry              `json:"ledger,omitempty"`
	// Updated is when this restore last did something; a claim does not last forever.
	Updated    time.Time `json:"updated,omitempty"`
	SnapshotAt time.Time `json:"snapshot_at,omitempty"`
	// ForceOlder and CompanyDir are the choices a resume may not change.
	ForceOlder bool   `json:"force_older,omitempty"`
	CompanyDir string `json:"company_dir,omitempty"`
	// Volumes are the volume directories already copied: a resume leaves them alone.
	Volumes map[string]bool `json:"volumes,omitempty"`
}

// claimTTL is how long an unfinished restore may sit before its claim on the server expires.
const claimTTL = 72 * time.Hour

// RetentionWarning is said about everything a restore is allowed to leave behind.
func RetentionWarning(snapshot string) string {
	return fmt.Sprintf("what is not restored is NOT in this server's later backups either, and the snapshots that hold it are deleted %d days after they were made: recover it from snapshot %s before then, or it is lost for good", DefaultSettings().RetentionDays, snapshot)
}

func (o *Restore) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// PlanStep is one line of `can restore --plan`.
type PlanStep struct {
	N      int    `json:"n"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Done   bool   `json:"done"`
}

// fingerprint tells repositories apart in the state file. The password is deliberately not part
// of it: a fast hash of it has no place on disk, and a wrong one cannot open the repository.
func (o *Restore) fingerprint() string {
	repo := o.Kit.Restic.Repository
	if o.Source != "" {
		repo = o.Source
	}
	h := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(h[:8])
}

func (o *Restore) hostPath(p string) string {
	root := o.Root
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, filepath.FromSlash(p))
}

func (o *Restore) load() (*restoreState, error) {
	st := &restoreState{Version: 2, Steps: map[string]stepState{}}
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
	if st.Version != 2 {
		return nil, fmt.Errorf("restore state %s is from another release of can; delete it to start over", o.StatePath)
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
	st.Updated = o.now().UTC()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSync(o.StatePath, b, 0o600)
}

// writeFileSync writes p atomically and flushes it and its directory.
func writeFileSync(p string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // operator-chosen path
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
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil { //nolint:gosec // same directory
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Plan lists the steps and which are already done. It touches nothing
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

func (o *Restore) warn(ws []string) {
	for _, w := range ws {
		o.say("WARNING: %s", w)
	}
}

// complete reports whether every step is done.
func (st *restoreState) complete() bool {
	for _, s := range stepTitles {
		if !st.Steps[s.id].Done {
			return false
		}
	}
	return true
}

// stale says why an unfinished restore's claim on this server no longer holds, "" if it does.
// A claim lets a resume skip the blank-server check, so it must not outlive the half-restored
// server it was made on.
func (o *Restore) stale(ctx context.Context, st *restoreState) (string, error) {
	if !st.Claimed {
		return "", nil
	}
	if age := o.now().Sub(st.Updated); age > claimTTL {
		return fmt.Sprintf("it has not run for %s", age.Round(time.Hour)), nil
	}
	mode, err := o.Cluster.Mode(ctx)
	if err != nil {
		return "", fmt.Errorf("ask can-core which restore this server is in: %w", err)
	}
	switch {
	case mode == "unknown", mode == "restoring "+st.ID:
		return "", nil
	case mode == "normal restored "+st.ID && st.Steps[StepErasure].Done:
		return "", nil // only going live was left
	case mode == "":
		if st.Steps[StepPlatform].Done {
			return "the can-core this restore deployed is gone", nil
		}
		return "", nil
	case !st.Steps[StepPlatform].Done && st.Overwrite:
		return "", nil // the server being overwritten still runs its old can-core
	}
	return fmt.Sprintf("can-core on this server says %q, not that it is in this restore", mode), nil
}

// conflicts refuses flags that contradict what an earlier run of this restore already did.
func (o *Restore) conflicts(st *restoreState) error {
	again := fmt.Sprintf("this restore was started differently; run it with the same flags, or delete %s to start over", o.StatePath)
	switch {
	case st.Snapshot != "" && o.Snapshot != "" && o.Snapshot != st.Snapshot:
		return fmt.Errorf("--snapshot %s conflicts with snapshot %s: %s", o.Snapshot, st.Snapshot, again)
	case st.Snapshot != "" && o.ForceOlder && !st.ForceOlder:
		return fmt.Errorf("--force-older conflicts with the already chosen snapshot %s: %s", st.Snapshot, again)
	case o.ConfirmOverwrite != "" && st.Claimed && (!st.Overwrite || o.ConfirmOverwrite != o.Kit.Company.Name):
		return fmt.Errorf("--confirm-overwrite %q conflicts (the server was blank when this restore claimed it, or the name differs): %s", o.ConfirmOverwrite, again)
	case st.CompanyDir != "" && o.DirChosen && o.CompanyDir != st.CompanyDir:
		return fmt.Errorf("--dir %s conflicts with %s, where company.yaml was written: %s", o.CompanyDir, st.CompanyDir, again)
	}
	return nil
}

// Run executes the remaining steps. It refuses a restore that already finished: running it
// again would do nothing, and saying "complete" would let the operator believe otherwise.
func (o *Restore) Run(ctx context.Context) error {
	st, err := o.load()
	if err != nil {
		return err
	}
	if st.complete() {
		return fmt.Errorf("this server was already restored from snapshot %s (finished %s): NOTHING was done now, and any --snapshot or --confirm-overwrite given was not applied. To restore again over this server, delete %s and run with --confirm-overwrite %q",
			st.Snapshot, st.Steps[StepLive].Finished.Format(time.RFC3339), o.StatePath, o.Kit.Company.Name)
	}
	why, err := o.stale(ctx, st)
	if err != nil {
		return err
	}
	if why != "" {
		o.say("the unfinished restore recorded in %s is stale (%s): starting over, with the blank-server check", o.StatePath, why)
		st = &restoreState{Version: 2, Steps: map[string]stepState{}}
	}
	if err := o.conflicts(st); err != nil {
		return err
	}
	st.Fingerprint = o.fingerprint()
	if st.ID == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		st.ID = hex.EncodeToString(b[:])
	}
	impls := map[string]func(context.Context, *restoreState) error{
		StepFetch: o.stepFetch, StepDumps: o.stepDumps, StepPlatform: o.stepPlatform, StepApply: o.stepApply,
		StepVolumes: o.stepVolumes, StepHooks: o.stepHooks, StepErasure: o.stepErasure, StepLive: o.stepLive,
	}
	for i, s := range stepTitles {
		if st.Steps[s.id].Done {
			o.say("[%d/%d] %s: already done", i+1, len(stepTitles), s.title)
			continue
		}
		o.say("[%d/%d] %s", i+1, len(stepTitles), s.title)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := impls[s.id](ctx, st); err != nil {
			st.Steps[s.id] = stepState{Error: err.Error()}
			if st.Claimed { // before the server is claimed nothing was changed: leave no trace
				_ = o.save(st)
			}
			return fmt.Errorf("restore step %d (%s) failed: %w; fix the cause and run the same command again to resume", i+1, s.id, err)
		}
		st.Steps[s.id] = stepState{Done: true, Finished: time.Now().UTC()}
		if err := o.save(st); err != nil {
			return err
		}
	}
	return nil
}

func (o *Restore) hostState() string {
	if o.HostStateDir == "" {
		return DefaultSettings().HostStateDir
	}
	return o.HostStateDir
}

// markersAt reads the dumps marker and the copy the backup took under, both
// below root, and requires them to agree (same run id and finish time).
func markersAt(root, dumpsDir, hostStateDir string) (Marker, error) {
	under := func(p string) string { return filepath.Join(root, filepath.FromSlash(p)) }
	m, err := ReadMarker(filepath.Join(under(dumpsDir), MarkerName))
	if err != nil {
		return m, fmt.Errorf("snapshot dumps: %w", err)
	}
	c, err := ReadMarker(filepath.Join(under(hostStateDir), MarkerCopy))
	if err != nil {
		return m, fmt.Errorf("snapshot host state: %w", err)
	}
	if c.RunID != m.RunID || !c.FinishedAt.Equal(m.FinishedAt) {
		return m, fmt.Errorf("dumps marker (run %s) differs from the marker the backup ran under (run %s): inconsistent snapshot", m.RunID, c.RunID)
	}
	return m, nil
}

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
		return st.Snapshot, nil
	}
	st.ForceOlder = o.ForceOlder
	if o.Snapshot != "" {
		st.Snapshot = o.Snapshot
		o.say("using snapshot %s (chosen with --snapshot)", o.Snapshot)
		return st.Snapshot, nil
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
		_, err = markersAt(tmp, o.DumpsDir, o.hostState())
		_ = os.RemoveAll(tmp)
		if err != nil {
			if !o.ForceOlder {
				return "", fmt.Errorf("the newest usable-looking snapshot %s (%s) fails its consistency check: %v; refusing to fall back to an older one silently (use --force-older to skip it, or --snapshot ID)", sn.ID, when, err)
			}
			o.say("WARNING: --force-older: skipping snapshot %s (%s): %v", sn.ID, when, err)
			continue
		}
		o.say("using snapshot %s (%s)", sn.ID, when)
		st.Snapshot, st.SnapshotAt = sn.ID, sn.Time
		return sn.ID, nil
	}
	return "", errors.New("repository has no usable snapshot (tagged can and not inconsistent)")
}

// checkBlank refuses a server that is not blank unless the operator confirmed by naming the
// company. It is run before fetching and again just before the first change: only that change
// makes the server this restore's (Claimed), so that a restore that never got that far does not
// skip the check when it is run again, however much later.
func (o *Restore) checkBlank(ctx context.Context, st *restoreState) error {
	if st.Claimed {
		return nil
	}
	var found []string
	for _, d := range []string{o.DumpsDir, o.hostState()} {
		ents, err := os.ReadDir(o.hostPath(d))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if len(ents) > 0 {
			found = append(found, d+" is not empty")
		}
	}
	for _, f := range []string{o.LedgerPath, path.Join(o.ConfigDir, PasswordFileName)} {
		if f == "" || f == PasswordFileName {
			continue
		}
		if _, err := os.Stat(o.hostPath(f)); err == nil {
			found = append(found, f+" exists")
		}
	}
	live, err := o.Cluster.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspect the cluster: %w", err)
	}
	found = append(found, live...)
	st.Overwrite = false
	if len(found) > 0 {
		if o.ConfirmOverwrite != o.Kit.Company.Name {
			return fmt.Errorf("this server is not blank, and a restore would overwrite it: %s. Restore onto a freshly bootstrapped server, or confirm with --confirm-overwrite %q (existing data is lost)", strings.Join(found, "; "), o.Kit.Company.Name)
		}
		o.say("WARNING: --confirm-overwrite: overwriting this server (%s)", strings.Join(found, "; "))
		st.Overwrite = true
	}
	return nil
}

const stagingMarker = ".can-restore-staging"
const stagingDone = ".can-restore-fetched"
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

func (o *Restore) staged(p string) string { return filepath.Join(o.StagingDir, filepath.FromSlash(p)) }

// ensureStaged restores the snapshot into the staging directory (once; again if the staging
// directory is gone) and hashes every dump against the manifest taken at backup time.
func (o *Restore) ensureStaged(ctx context.Context, st *restoreState) error {
	if err := checkStaging(o.StagingDir); err != nil {
		return err
	}
	done := filepath.Join(o.StagingDir, stagingDone)
	if b, err := os.ReadFile(done); err == nil && string(b) == st.ID+" "+st.Snapshot && st.Snapshot != "" { //nolint:gosec // our staging dir
		return nil
	}
	snap, err := o.pin(ctx, st)
	if err != nil {
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
	if err := o.Restic.Restore(ctx, snap, o.StagingDir, []string{o.DumpsDir, o.hostState(), o.PVCRoot}); err != nil {
		return err
	}
	if _, err := o.checkDumps(); err != nil {
		return err
	}
	return os.WriteFile(done, []byte(st.ID+" "+st.Snapshot), 0o600)
}

// checkDumps requires a consistent completion marker and hashes every restored dump against the
// manifest. It returns the manifest's file names.
func (o *Restore) checkDumps() ([]string, error) {
	if _, err := markersAt(o.StagingDir, o.DumpsDir, o.hostState()); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(o.staged(o.hostState()), ManifestName)) //nolint:gosec // our staging dir
	if err != nil {
		return nil, fmt.Errorf("restored host state has no %s: %w", ManifestName, err)
	}
	man, err := ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(man))
	for n := range man {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sum, err := hashFile(filepath.Join(o.staged(o.DumpsDir), filepath.FromSlash(n)))
		if err != nil {
			return nil, fmt.Errorf("restored dump %s: %w", n, err)
		}
		if sum != man[n] {
			return nil, fmt.Errorf("restored dump %s does not match the backup manifest", n)
		}
	}
	return names, nil
}

func (o *Restore) accepted(what string) bool { return slices.Contains(o.AcceptUnrestored, what) }

// checkCompany refuses a snapshot of another company than the kit's: one kit, one company.
func (o *Restore) checkCompany(dumps string) error {
	b, err := os.ReadFile(filepath.Join(dumps, CompanyDump)) //nolint:gosec // our staging dir
	if err != nil {
		return err
	}
	var c struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("the snapshot's company.yaml: %w", err)
	}
	switch {
	case c.Name == o.Kit.Company.Name:
		return nil
	case o.AcceptCompany != "" && o.AcceptCompany == c.Name:
		o.say("WARNING: --accept-company: the snapshot is of %q, the kit of %q", c.Name, o.Kit.Company.Name)
		return nil
	}
	return fmt.Errorf("the snapshot's company.yaml is of %q but this recovery kit is of %q: wrong kit or wrong repository. If the company was renamed since the kit was made, add --accept-company %q", c.Name, o.Kit.Company.Name, c.Name)
}

// stepFetch does every check that can be done before the server is changed.
func (o *Restore) stepFetch(ctx context.Context, st *restoreState) error {
	if _, err := o.pin(ctx, st); err != nil {
		return err
	}
	// Only after the repository answered: a wrong kit leaves no trace.
	if err := o.checkBlank(ctx, st); err != nil {
		return err
	}
	if err := o.ensureStaged(ctx, st); err != nil {
		return err
	}
	files, err := o.checkDumps()
	if err != nil {
		return err
	}
	c, err := CheckContents(ctx, o.staged(o.DumpsDir), files)
	if err != nil {
		return err
	}
	if err := o.checkCompany(o.staged(o.DumpsDir)); err != nil {
		return err
	}
	var blocking []string
	if !c.HasSecrets && !o.WithoutSecrets {
		blocking = append(blocking, c.Problems()[0])
	}
	for _, m := range append(append([]string{}, c.NoRestoreHook...), c.NotBackedUp...) {
		if !o.accepted(m) {
			blocking = append(blocking, fmt.Sprintf("module %q cannot be restored from this snapshot (no restore hook, or DATABASE IS NOT BACKED UP); --accept-unrestored %s to go on without it", m, m))
		}
	}
	if len(blocking) > 0 {
		return fmt.Errorf("this restore would be incomplete: %s", strings.Join(blocking, "; "))
	}
	o.warn(c.Problems())
	if len(o.AcceptUnrestored) > 0 {
		o.say("WARNING: --accept-unrestored %s: %s", strings.Join(o.AcceptUnrestored, ", "), RetentionWarning(st.Snapshot))
	}
	entries, err := o.mergedLedger(ctx)
	if err != nil {
		return err
	}
	o.say("snapshot checked: %d modules recorded, %d with dumps, %d erasure tombstones", len(c.Modules), len(c.Dumped), len(entries))
	return nil
}

func (o *Restore) stepDumps(ctx context.Context, st *restoreState) error {
	if err := o.ensureStaged(ctx, st); err != nil {
		return err
	}
	src, dst := o.staged(o.DumpsDir), o.hostPath(o.DumpsDir)
	if err := o.checkBlank(ctx, st); err != nil {
		return err
	}
	// Refused before anything is recorded or changed, so that another --dir can still be chosen.
	company, err := o.companyToWrite(src)
	if err != nil {
		return err
	}
	st.Claimed, st.CompanyDir = true, o.CompanyDir
	if err := o.save(st); err != nil {
		return err
	}
	if st.Overwrite {
		if o.StopTimer != nil {
			if err := o.StopTimer(ctx); err != nil {
				o.say("WARNING: could not stop the backup timer of this server (%v): stop it yourself, or it may back up a half-restored server", err)
			}
		}
		if err := o.Cluster.Halt(ctx); err != nil {
			return fmt.Errorf("stop the running can-core: %w", err)
		}
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if err := o.keepCollected(dst); err != nil {
		return err
	}
	// Replace-all: the claim established the directory is empty, ours, or confirmed.
	if err := emptyDir(dst); err != nil {
		return err
	}
	if err := CopyTree(src, dst); err != nil {
		return err
	}
	// No marker: a half-restored server must never be backed up.
	if err := os.Remove(filepath.Join(dst, MarkerName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Chmod(dst, 0o700); err != nil { //nolint:gosec // can-core's directory
		return err
	}
	if o.Chown != nil {
		if err := o.Chown(dst, canCoreUID); err != nil {
			return err
		}
	}
	if company == nil {
		return nil
	}
	if err := os.MkdirAll(o.CompanyDir, 0o755); err != nil { //nolint:gosec // company repo dir
		return err
	}
	return os.WriteFile(filepath.Join(o.CompanyDir, "company.yaml"), company, 0o644) //nolint:gosec // company.yaml holds no secrets
}

// companyToWrite is the backed-up company.yaml to put into CompanyDir: nil when there is nothing to
// write (no directory asked for, or it is already there), an error when another one is there.
func (o *Restore) companyToWrite(src string) ([]byte, error) {
	if o.CompanyDir == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(src, CompanyDump)) //nolint:gosec // our staging dir
	if err != nil {
		return nil, fmt.Errorf("restored dumps have no company.yaml: %w", err)
	}
	file := filepath.Join(o.CompanyDir, "company.yaml")
	switch existing, err := os.ReadFile(file); { //nolint:gosec // operator-chosen dir
	case err == nil && string(existing) != string(data):
		return nil, fmt.Errorf("%s exists and differs from the backed-up company.yaml; refusing to overwrite (choose another --dir)", file)
	case err == nil:
		return nil, nil
	case errors.Is(err, os.ErrNotExist):
		return data, nil
	default:
		return nil, err
	}
}

// keepCollected puts into the server's ledger what the can-core being overwritten had collected,
// before its dumps directory is emptied: no timer may have imported it yet.
func (o *Restore) keepCollected(dumps string) error {
	if o.LedgerPath == "" {
		return nil
	}
	dir := filepath.Join(dumps, CollectedDir)
	es, err := ReadCollected(dir)
	if err != nil {
		return fmt.Errorf("the erasures collected on this server cannot be read (repair or move %s aside): %w", dir, err)
	}
	if len(es) == 0 {
		return nil
	}
	ledger := o.hostPath(o.LedgerPath)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o700); err != nil {
		return err
	}
	_, err = (&Ledger{Path: ledger}).AppendMany(es)
	return err
}

// emptyDir removes the content of dir, not dir itself, except the entries named in keep.
func emptyDir(dir string, keep ...string) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if slices.Contains(keep, e.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (o *Restore) stepPlatform(ctx context.Context, st *restoreState) error {
	yaml, err := os.ReadFile(filepath.Join(o.hostPath(o.DumpsDir), CompanyDump)) //nolint:gosec // our restore root
	if err != nil {
		return fmt.Errorf("restored dumps have no company.yaml: %w", err)
	}
	return o.Cluster.Start(ctx, StartOptions{ID: st.ID, CompanyYAML: yaml, Overwrite: st.Overwrite})
}

func (o *Restore) stepApply(ctx context.Context, st *restoreState) error {
	rep, err := o.Cluster.ApplyModules(ctx, st.Overwrite)
	o.warn(rep.Warnings)
	if err != nil {
		return err
	}
	o.say("%d Secrets restored; modules created at 0 replicas: %s", rep.Secrets, orNothing(rep.Modules))
	return nil
}

func orNothing(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// backedUpVolumes maps the claim of every local-path directory in the staged snapshot
// (pvc-<uid>_<namespace>_<claim>) to that directory.
func (o *Restore) backedUpVolumes() (map[VolumeRef]string, error) {
	root := o.staged(o.PVCRoot)
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[VolumeRef]string{}
	for _, e := range ents {
		p := strings.SplitN(e.Name(), "_", 3)
		if !e.IsDir() || len(p) != 3 || !strings.HasPrefix(p[0], "pvc-") {
			continue
		}
		ref := VolumeRef{Namespace: p[1], Claim: p[2]}
		if other, dup := out[ref]; dup {
			return nil, fmt.Errorf("the snapshot holds two volumes for claim %s (%s and %s): remove the stale one on a copy, or choose another --snapshot", ref, filepath.Base(other), e.Name())
		}
		out[ref] = filepath.Join(root, e.Name())
	}
	return out, nil
}

func (o *Restore) stepVolumes(ctx context.Context, st *restoreState) error {
	if err := o.ensureStaged(ctx, st); err != nil {
		return err
	}
	vols, err := o.backedUpVolumes()
	if err != nil {
		return err
	}
	var want []VolumeRef
	for ref := range vols {
		switch {
		case strings.HasPrefix(ref.Namespace, "mod-"):
			want = append(want, ref)
		case ref.Namespace == "can-system" && ref.Claim == "can-core-data":
			// can-core's state comes from its consistent dump, not from these files.
		case ref.Namespace == "can-data" && ref.Claim == "postgres-data":
			// A file copy of a running database server (only in snapshots made before it was
			// excluded) is not a backup: the databases come from the modules' dumps.
			o.say("not restored: volume %s (the shared Postgres starts empty; each module's dump is loaded by its hook)", ref)
		case ref.Namespace == "kube-system":
			o.say("not restored: volume %s (certificates and ingress state are re-created by the new cluster)", ref)
		case o.accepted(ref.String()):
			o.say("WARNING: --accept-unrestored: volume %s is NOT restored; %s", ref, RetentionWarning(st.Snapshot))
		default:
			return fmt.Errorf("the snapshot holds volume %s, which this restore cannot bring back (--accept-unrestored %s to go on without it)", ref, ref)
		}
	}
	sort.Slice(want, func(i, j int) bool { return want[i].String() < want[j].String() })
	if len(want) == 0 {
		return nil
	}
	have, err := o.Cluster.Volumes(ctx, want)
	if err != nil {
		return err
	}
	dirs := map[VolumeRef]string{}
	for _, v := range have {
		dirs[v.VolumeRef] = v.Dir
	}
	for _, ref := range want {
		dir, ok := dirs[ref]
		if !ok {
			if !o.accepted(ref.String()) {
				return fmt.Errorf("backed-up volume %s has no matching claim on the new cluster (--accept-unrestored %s to go on without it)", ref, ref)
			}
			o.say("WARNING: --accept-unrestored: volume %s is NOT restored; %s", ref, RetentionWarning(st.Snapshot))
			continue
		}
		dst := o.hostPath(dir)
		guard := filepath.Join(dst, copyMarker)
		if !st.Volumes[dst] {
			if err := o.copyVolume(vols[ref], dst, st.Overwrite); err != nil {
				return fmt.Errorf("copying %s: %w", ref, err)
			}
			// Recorded before the guard goes: a resume must never find a copied volume that it
			// cannot tell from one that holds someone else's data.
			if st.Volumes == nil {
				st.Volumes = map[string]bool{}
			}
			st.Volumes[dst] = true
			if err := o.save(st); err != nil {
				return err
			}
			o.say("restored volume %s", ref)
		}
		if err := os.Remove(guard); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// copyVolume replaces the content of a new volume with the staged copy. A destination that
// already holds data is refused unless it carries our in-progress marker (an earlier
// interrupted copy) or the operator confirmed an overwrite. The marker is left for the caller
// to remove once the copy is recorded. Replacing, not merging, matters:
// a stale SQLite -wal file next to a restored database corrupts it.
func (o *Restore) copyVolume(src, dst string, overwrite bool) error {
	guard := filepath.Join(dst, copyMarker)
	if _, err := os.Lstat(guard); err != nil {
		ents, rerr := os.ReadDir(dst)
		if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return rerr
		}
		if len(ents) > 0 && !overwrite {
			return fmt.Errorf("volume %s already holds data; refusing to overwrite it", dst)
		}
	}
	if err := os.MkdirAll(dst, 0o755); err != nil { //nolint:gosec // CopyTree sets modes
		return err
	}
	// The guard is on disk before anything is removed and is never removed here: a crash at any
	// point leaves a directory that a resume knows to be its own half-copied one.
	if err := writeFileSync(guard, nil, 0o600); err != nil {
		return err
	}
	if err := emptyDir(dst, copyMarker); err != nil {
		return err
	}
	return CopyTree(src, dst)
}

func (o *Restore) stepHooks(ctx context.Context, st *restoreState) error {
	rep, err := o.Cluster.RunHooks(ctx, HookOptions{Accept: o.AcceptUnrestored, MissingBlobs: o.AllowMissingBlobs})
	o.warn(rep.Warnings)
	if err != nil {
		return err
	}
	o.say("restore hooks run for: %s", orNothing(rep.Restored))
	if len(o.AcceptUnrestored) > 0 {
		o.say("WARNING: %s", RetentionWarning(st.Snapshot))
	}
	return nil
}

// ErasureWarning is the limit of the replay: only what is in the ledger can be replayed.
func ErasureWarning(snapshot string) string {
	return fmt.Sprintf("only erasures in the erasure ledger were replayed. can-core records those of people, equity, board, entity, files, finance, forms, jobs and work (and `can backup ledger-add` adds by hand); nothing erased in any other module is recorded, and an erasure that had not reached the bucket when the old server was lost (up to about a quarter of an hour, longer if `can backup` reported a ledger problem) is missing. Check the erasure requests made since snapshot %s and repeat what is not listed above", snapshot)
}

// mergedLedger is every tombstone known: the bucket's ledger, the copy inside the snapshot and
// any ledger already on this server. A bucket ledger that is missing or behind the snapshot's
// copy blocks unless explicitly accepted; a fetch that fails always blocks.
func (o *Restore) mergedLedger(ctx context.Context) ([]Entry, error) {
	// The snapshot carries its own copy of the ledger: a bucket copy that is
	// shorter than it has been rolled back or truncated.
	snapEntries, err := (&Ledger{Path: filepath.Join(o.staged(o.hostState()), LedgerCopy)}).Read()
	if err != nil {
		return nil, fmt.Errorf("ledger copy inside the snapshot: %w", err)
	}
	es, err := o.Ledger.Fetch(ctx)
	switch {
	case errors.Is(err, ErrNoLedger):
		if len(snapEntries) > 0 && !o.AcceptLedgerBehind {
			return nil, fmt.Errorf("%w, but the snapshot's copy holds %d entries: the bucket object was deleted or rolled back; refusing to go live (--accept-ledger-behind to apply the snapshot's copy only)", err, len(snapEntries))
		}
		if len(snapEntries) == 0 && !o.AllowNoLedger {
			return nil, fmt.Errorf("%w: refusing to go live without applying erasures (re-run with --allow-no-ledger if no erasure was ever requested)", err)
		}
		if len(snapEntries) > 0 {
			o.say("WARNING: --accept-ledger-behind: the bucket has no ledger; applying only the snapshot's %d entries (erasures made after that backup are NOT applied)", len(snapEntries))
		}
		es = nil
	case err != nil:
		return nil, fmt.Errorf("fetching the erasure ledger: %w", err)
	}
	merged, err := CheckNotBehind(es, snapEntries)
	if err != nil {
		if !o.AcceptLedgerBehind {
			return nil, err
		}
		o.say("WARNING: --accept-ledger-behind: %v", err)
		merged = MergeLedgers(es, snapEntries)
	}
	if o.LedgerPath != "" {
		local, err := (&Ledger{Path: o.hostPath(o.LedgerPath)}).Read()
		if err != nil {
			return nil, fmt.Errorf("the erasure ledger already on this server: %w", err)
		}
		merged = MergeLedgers(merged, local)
	}
	return merged, nil
}

func (o *Restore) stepErasure(ctx context.Context, st *restoreState) error {
	if err := o.ensureStaged(ctx, st); err != nil {
		return err
	}
	es, err := o.mergedLedger(ctx)
	if err != nil {
		return err
	}
	rep, err := o.Cluster.Erase(ctx, es, o.AcceptUnerased)
	for _, p := range rep.Purged {
		o.say("erased: %s", p)
	}
	if err != nil {
		return err
	}
	for _, s := range rep.SkippedByHand {
		o.say("NOT replayed, the backup was made after the time recorded: %s. If that data was not really erased by then, erase it by hand now", s)
	}
	for _, u := range rep.Unapplied {
		o.say("WARNING: --accept-unerased: tombstones of %s were NOT replayed; erase that data by hand", u)
	}
	st.Entries = es
	o.say("%d erasure purges replayed; %d erasures the backup already reflects were left alone; %d derived rows removed from can-core", len(rep.Purged), rep.Reflected, rep.CoreRows)
	o.say("WARNING: %s", ErasureWarning(st.Snapshot))
	return nil
}

func (o *Restore) stepLive(ctx context.Context, st *restoreState) error {
	// The ledger goes back first: without it the next backup would report it vanished, and a
	// later restore would not know of these erasures.
	if o.LedgerPath != "" {
		if err := o.writeLedger(st.Entries); err != nil {
			return err
		}
	}
	// The repository's password must be on the server again, or setting up the backup timer
	// would find a repository it cannot open.
	if o.ConfigDir != "" {
		dir := o.hostPath(o.ConfigDir)
		if err := writeFileSync(filepath.Join(dir, PasswordFileName), []byte(o.Kit.Restic.Password), 0o600); err != nil {
			return err
		}
		if err := writeFileSync(filepath.Join(dir, EnvFileName), []byte(HostEnvFile(o.Kit)), 0o600); err != nil {
			return err
		}
	}
	if err := o.Cluster.Complete(ctx); err != nil {
		return err
	}
	// The staged snapshot holds secrets and data that may since have been erased.
	if err := checkStaging(o.StagingDir); err != nil {
		return err
	}
	return os.RemoveAll(o.StagingDir)
}

// writeLedger merges the entries the restore applied with whatever the server's ledger holds now:
// an erasure recorded there meanwhile must not be overwritten.
func (o *Restore) writeLedger(applied []Entry) error {
	path := o.hostPath(o.LedgerPath)
	local, err := (&Ledger{Path: path}).Read()
	if err != nil {
		return fmt.Errorf("the erasure ledger already on this server: %w", err)
	}
	have := map[string]bool{}
	for _, e := range applied {
		have[e.key()] = true
	}

	var late []string
	for _, e := range local {
		if !have[e.key()] {
			late = append(late, e.Subject+"/"+e.Class)
		}
	}
	if len(late) > 0 {
		o.say("WARNING: these entries of this server's ledger were added after the replay and were NOT applied, erase them by hand: %s", strings.Join(late, ", "))
	}
	var b []byte
	for _, e := range MergeLedgers(applied, local) {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		b = append(append(b, line...), '\n')
	}
	if len(b) == 0 {
		return nil
	}
	return writeFileSync(path, b, 0o600)
}

// CopyTree copies src into dst (created if needed), preserving permissions and
// symlinks (links are recreated, never followed, also not ones already in dst). Files and
// directories are flushed to disk before it returns: a restore records the copy as done.
func CopyTree(src, dst string) error {
	var dirs []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Writing as root: never through a link that is already there.
		if rel != "." {
			if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				if err := os.Remove(target); err != nil {
					return err
				}
			}
		}
		switch {
		case d.IsDir():
			dirs = append(dirs, target)
			if err := os.MkdirAll(target, info.Mode().Perm()|0o700); err != nil {
				return err
			}
			chownLike(target, info)
			return os.Chmod(target, info.Mode().Perm()|info.Mode()&(os.ModeSetgid|os.ModeSticky))
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
			if err := syncFile(out); err != nil {
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
	if err != nil {
		return err
	}
	for _, dir := range append(dirs, filepath.Dir(dst)) {
		syncDir(dir)
	}
	return nil
}

// syncFile flushes a copied file; a variable so that tests can see it happen.
var syncFile = (*os.File).Sync
