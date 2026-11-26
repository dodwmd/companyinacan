package modmgr

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Limits.
const (
	// MaxPendingChanges bounds the proposals kept in the database; the oldest are dropped.
	MaxPendingChanges = 50
	// ChangeTTL is how long a proposal may be applied after it was prepared.
	ChangeTTL = 24 * time.Hour
	// ApplyTimeout bounds one UI-started reconcile.
	ApplyTimeout = 15 * time.Minute
)

// Errors of the change flow. Wrapped errors carry details for the page.
var (
	ErrInvalid       = errors.New("the change is not valid")
	ErrNotDeployable = errors.New("module cannot be enabled")
	ErrBlocked       = errors.New("the selection cannot work")
	ErrAckRequired   = errors.New("acknowledgement required")
	ErrStale         = errors.New("company.yaml changed after this change was prepared")
	ErrExpired       = errors.New("this change has expired")
	ErrBusy          = errors.New("another apply is running")
	ErrNotFound      = errors.New("not found")
	ErrNoChange      = errors.New("nothing to change")
	ErrConfirmation  = errors.New("the confirmation text does not match")
	ErrNotPurgeable  = errors.New("only a disabled module can be purged")
	ErrNoPurger      = errors.New("purge is not available in this installation")
)

// DetailError is a sentinel with the specifics the page lists.
type DetailError struct {
	Kind    error
	Details []string
	Missing []string // ErrAckRequired: the acknowledgement keys that were not given
}

func (e *DetailError) Error() string {
	return e.Kind.Error() + ": " + strings.Join(e.Details, "; ")
}

// Unwrap lets errors.Is match the sentinel.
func (e *DetailError) Unwrap() error { return e.Kind }

// AckError is returned when acknowledgements are missing: "licence:<id>" for a source-available
// module, "experimental:<id>" for an experimental one, "backup:<id>" (at apply) for one whose enabling
// stops all backups.
type AckError struct{ Missing []string }

func (e *AckError) Error() string {
	return ErrAckRequired.Error() + ": " + strings.Join(e.Missing, ", ")
}

// Unwrap lets errors.Is match ErrAckRequired.
func (e *AckError) Unwrap() error { return ErrAckRequired }

// AuditLog is the audit interface the service needs (audit.Log satisfies it).
type AuditLog interface {
	Append(ctx context.Context, e audit.Entry) (audit.Record, error)
	Query(ctx context.Context, f audit.Filter) ([]audit.Record, error)
}

// Purger destroys a disabled module's data (namespace, PVCs, Secrets, database). It is the only
// destructive operation of this package and is reached only through Service.Purge. Nil means purge
// is not available.
type Purger interface {
	Purge(ctx context.Context, module string) error
}

// Service is module management for the admin UI and API.
type Service struct {
	DB         *store.DB
	Audit      AuditLog
	ModulesDir string
	Company    CompanyStore
	Runner     Reconciling
	Purger     Purger
	// OnPurged removes what else holds a purged module's data (its backup dumps); optional.
	OnPurged func(id string) error
	// LastDump reads the outcome of the last backup dump run; optional.
	LastDump func() (backup.DumpRun, error)
	Secrets  SecretWriter
	Ingress  IngressStatus
	Disk     DiskReader
	Probe    ReadyProbe
	Net      NetInfo
	Instant  InstantProbe
	Bundles  []Bundle
	Now      func() time.Time
	// Async starts the reconcile of an apply; it defaults to `go f()`. Tests run it inline.
	Async func(func())
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Catalogue loads the module catalogue.
func (s *Service) Catalogue() (*Catalogue, error) { return LoadCatalogue(s.ModulesDir) }

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Change states.
const (
	ChangeProposed = "proposed"
	ChangeApplied  = "applied"
)

// Proposal is what an operator asks for.
type Proposal struct {
	Patch   Patch
	Acks    []string
	Summary string
}

// Change is a stored proposal.
type Change struct {
	ID, Actor, BaseVersion, Summary, State, RunID string
	CreatedAt                                     time.Time
	Patch                                         Patch
	Acks                                          []string
}

// ResourcePlan says what applying one action creates or keeps.
type ResourcePlan struct {
	Module, Action, Reason string
	Namespace              string
	Host                   string   // public ingress host, empty when the module has no UI
	Secrets                []string // Secrets can-core creates in the namespace
	DiskMB                 int      // declared persistent disk, 0 for none
	Database               string   // postgres, mariadb or sqlite; empty when none
	DBName                 string   // created database name (shared engines only)
	Keeps                  []string // disable: what stays: namespace, pvc, secrets, database
}

// Preview is a change as it would be applied now.
type Preview struct {
	Change       Change
	Version      string // current stored company.yaml version
	Stale        bool
	Diff         []DiffLine
	DiffTooLarge bool
	Actions      []reconcile.Action
	// Held are upgrades that applying this change would not run: they wait for approval
	// (UpgradeAck) or for a retry.
	Held      []reconcile.Action
	Resources []ResourcePlan
	Footprint FootprintResult
	Analysis  Analysis
	// Warnings are the settings and company facts of the new document that reach no module.
	Warnings []string
	// Issues that stop the change from being proposed or applied.
	Problems      []string
	Blockers      []string
	NotDeployable []string
	AckNeeded     []AckNeed
	// BackupStops lists the newly enabled modules whose backup hook cannot run. While one is enabled
	// every dump run fails, so applying needs BackupAck(module) to be confirmed explicitly.
	BackupStops []BackupStop

	newData []byte
	desired reconcile.Desired
	cat     *Catalogue
}

// BackupStop is a module whose enabling stops all backups.
type BackupStop struct {
	Module, Why string
	// Postgres is set when the module's database lives in the shared Postgres, which is then not backed up.
	Postgres bool
}

// BackupAck is the confirmation Apply requires for a module that stops backups.
func BackupAck(module string) string { return "backup:" + module }

func missingBackupAcks(stops []BackupStop, confirmed []string) error {
	var missing []string
	for _, b := range stops {
		if !slices.Contains(confirmed, BackupAck(b.Module)) {
			missing = append(missing, BackupAck(b.Module))
		}
	}
	if len(missing) > 0 {
		return &AckError{Missing: missing}
	}
	return nil
}

// AckNeed is an acknowledgement the change requires and whether it was given.
type AckNeed struct {
	Key, Module, Kind string // Kind: licence or experimental
	Given             bool
}

// Err returns the reason this preview cannot be applied, or nil.
func (p *Preview) Err() error {
	var missing []string
	for _, a := range p.AckNeeded {
		if !a.Given {
			missing = append(missing, a.Key)
		}
	}
	switch {
	case len(p.NotDeployable) > 0:
		return &DetailError{Kind: ErrNotDeployable, Details: p.NotDeployable}
	case len(p.Problems) > 0:
		return &DetailError{Kind: ErrInvalid, Details: p.Problems}
	case len(p.Blockers) > 0:
		return &DetailError{Kind: ErrBlocked, Details: p.Blockers}
	case len(missing) > 0:
		return &AckError{Missing: missing}
	}
	return nil
}

func decodeCompany(name string, data []byte) (*config.Company, []problem.Problem) {
	doc, ps := config.LoadBytes(name, data)
	if doc == nil {
		return nil, ps
	}
	c, err := config.DecodeCompany(doc)
	if err != nil {
		return nil, []problem.Problem{{Rule: "parse.decode", Severity: problem.Error, Message: "company.yaml cannot be read"}}
	}
	return c, nil
}

func enabledSet(c *config.Company) map[string]bool {
	out := map[string]bool{}
	for id, e := range c.Modules {
		if e.IsEnabled() {
			out[id] = true
		}
	}
	return out
}

func blockerKeys(a Analysis) map[string]string {
	out := map[string]string{}
	for _, n := range a.Needs {
		switch n.State {
		case NeedMissing, NeedPlanned, NeedNone:
			out[n.Module+"/"+n.Capability] = fmt.Sprintf("%s needs %s (%s)", n.Module, n.Capability, n.State)
		}
	}
	for _, c := range a.Conflicts {
		out["conflict/"+c.Capability] = fmt.Sprintf("%s is served by %s: designate one as primary", c.Capability, strings.Join(c.Modules, ", "))
	}
	return out
}

// build computes the effect of patch on the stored document without storing anything.
func (s *Service) build(ctx context.Context, cur Snapshot, patch Patch, acks []string) (*Preview, error) {
	if len(patch.Ops) == 0 {
		return nil, ErrNoChange
	}
	newData, err := ApplyPatch(cur.Data, patch)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, Sanitise(err.Error()))
	}
	pv := &Preview{Version: cur.Version, newData: newData}
	pv.DiffTooLarge = DiffTooLarge(string(cur.Data), string(newData))
	if !pv.DiffTooLarge {
		pv.Diff = Diff(string(cur.Data), string(newData))
	}
	cat, err := s.Catalogue()
	if err != nil {
		return nil, err
	}
	pv.cat = cat
	oldC, ps := decodeCompany("company.yaml", cur.Data)
	if oldC == nil {
		return nil, fmt.Errorf("%w: the stored company.yaml cannot be read (%s)", ErrInvalid, firstProblem(ps))
	}
	newC, ps := decodeCompany("company.yaml", newData)
	if newC == nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, firstProblem(ps))
	}
	oldSel, newSel := enabledSet(oldC), enabledSet(newC)

	// Modules that are newly enabled must be deployable and acknowledged.
	given := map[string]bool{}
	for _, a := range acks {
		given[a] = true
	}
	var ids []string
	for id := range newSel {
		if !oldSel[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := cat.Get(id)
		switch {
		case e == nil:
			pv.Problems = append(pv.Problems, "module "+id+" is not in the catalogue")
			continue
		case !e.Deployable():
			pv.NotDeployable = append(pv.NotDeployable, id)
			continue
		}
		if e.SourceAvailable() {
			k := "licence:" + id
			pv.AckNeeded = append(pv.AckNeeded, AckNeed{Key: k, Module: id, Kind: "licence", Given: given[k]})
		}
		if why := e.BackupWarning(); why != "" {
			pv.BackupStops = append(pv.BackupStops, BackupStop{Module: id, Why: why, Postgres: e.Manifest.Requires.Database == "postgres"})
		}
		if e.Status() == "experimental" {
			k := "experimental:" + id
			pv.AckNeeded = append(pv.AckNeeded, AckNeed{Key: k, Module: id, Kind: "experimental", Given: given[k]})
		}
	}
	// Settings written by the patch must satisfy the module's own schema; a module without a schema
	// takes no settings from the UI.
	for _, o := range patch.Ops {
		if len(o.Path) == 3 && o.Path[0] == "modules" && o.Path[2] == "settings" {
			e := cat.Get(o.Path[1])
			m, _ := o.Value.(map[string]any)
			switch {
			case e == nil:
				pv.Problems = append(pv.Problems, "module "+o.Path[1]+" is not in the catalogue")
			case !newSel[o.Path[1]]:
				pv.Problems = append(pv.Problems, "settings of "+o.Path[1]+" can only be changed while the module is enabled")
			case e.Schema == nil && len(m) > 0:
				pv.Problems = append(pv.Problems, "module "+o.Path[1]+" has no settings schema; its settings cannot be changed here")
			case e.Schema != nil:
				for f := range e.NewProblems(oldC.Modules[o.Path[1]].Settings, m) {
					pv.Problems = append(pv.Problems, "settings of "+o.Path[1]+": invalid "+f)
				}
			}
		}
	}

	pv.Analysis = cat.Analyse(newSel, newC.Primary)
	before := blockerKeys(cat.Analyse(oldSel, oldC.Primary))
	var bk []string
	for k, msg := range blockerKeys(pv.Analysis) {
		if _, had := before[k]; !had {
			bk = append(bk, msg)
		}
	}
	sort.Strings(bk)
	pv.Blockers = bk

	var mods []*config.Module
	for _, id := range sortedSelected(newSel, cat) {
		mods = append(mods, cat.Get(id).Manifest)
	}
	pv.Footprint = Footprint(mods, serverRAM(newC))

	d, dps := reconcile.LoadDesiredBytes("company.yaml", newData, s.ModulesDir)
	if problem.HasErrors(dps) {
		for _, p := range dps {
			if p.Severity == problem.Error {
				pv.Problems = append(pv.Problems, Sanitise(p.Message))
			}
		}
		return pv, nil
	}
	pv.desired = d
	for _, p := range dps {
		if p.Rule == "semantic.organisation" || strings.HasPrefix(p.Rule, "semantic.setting-") {
			pv.Warnings = append(pv.Warnings, Sanitise(p.Message))
		}
	}
	if s.Runner != nil {
		planned, err := s.Runner.Plan(ctx, d)
		if err != nil {
			return nil, err
		}
		pv.Actions, pv.Held = reconcile.SplitHeld(planned)
	}
	pv.Resources = resources(pv.Actions, cat, newC.Domain)
	return pv, nil
}

func serverRAM(c *config.Company) int {
	switch v := c.Provider.Options["server_ram_mb"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// ServerRAMMB is the declared server size of a company document (0 when not declared).
func ServerRAMMB(c *config.Company) int { return serverRAM(c) }

// Names of the Secrets can-core creates in every module namespace (core/server/installer).
const (
	secretModule  = "can-module"
	secretDB      = "can-db"
	secretRelease = "can-release"
)

func resources(actions []reconcile.Action, cat *Catalogue, domain string) []ResourcePlan {
	var out []ResourcePlan
	for _, a := range actions {
		rp := ResourcePlan{Module: a.Module, Action: string(a.Kind), Reason: Sanitise(a.Reason), Namespace: "mod-" + a.Module}
		if a.Kind == reconcile.Disable {
			rp.Keeps = []string{"namespace", "pvc", "secrets"}
			if e := cat.Get(a.Module); e != nil && e.Manifest != nil && sharedDB(e.Manifest.Requires.Database) {
				rp.Keeps = append(rp.Keeps, "database")
			}
			out = append(out, rp)
			continue
		}
		rp.Secrets = []string{secretModule, secretRelease}
		if e := cat.Get(a.Module); e != nil && e.Manifest != nil {
			m := e.Manifest
			if m.Runtime.Subdomain != "" && domain != "" {
				rp.Host = m.Runtime.Subdomain + "." + domain
			}
			rp.DiskMB = m.Resources.DiskMB
			rp.Database = m.Requires.Database
			if sharedDB(m.Requires.Database) {
				rp.Secrets = append(rp.Secrets, secretDB)
				rp.DBName = "mod_" + strings.ReplaceAll(a.Module, "-", "_")
			}
		}
		out = append(out, rp)
	}
	return out
}

func sharedDB(d string) bool { return d == "postgres" || d == "mariadb" }

// Propose validates a change against the stored company.yaml and records it. Nothing is written to
// company.yaml; Apply does that.
func (s *Service) Propose(ctx context.Context, actor string, in Proposal) (Change, error) {
	cur, err := s.Company.Get(ctx)
	if err != nil {
		return Change{}, err
	}
	pv, err := s.build(ctx, cur, in.Patch, in.Acks)
	if err != nil {
		return Change{}, err
	}
	if err := pv.Err(); err != nil {
		return Change{}, err
	}
	pj, err := in.Patch.Marshal()
	if err != nil {
		return Change{}, err
	}
	aj, _ := json.Marshal(in.Acks)
	ch := Change{ID: newID(), Actor: actor, CreatedAt: s.now().UTC(), BaseVersion: cur.Version, Patch: in.Patch, Acks: in.Acks, Summary: Sanitise(in.Summary), State: ChangeProposed}
	if _, err := s.audit(ctx, actor, "company.change.propose", ch.ID, map[string]any{"paths": patchPaths(in.Patch), "base": cur.Version}); err != nil {
		return Change{}, err
	}
	err = s.DB.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO company_changes(id, actor, created_at, base_version, patch_json, acks_json, summary, state) VALUES (?,?,?,?,?,?,?,?)`,
			ch.ID, actor, ch.CreatedAt.Unix(), ch.BaseVersion, string(pj), string(aj), ch.Summary, ChangeProposed); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM company_changes WHERE state='proposed' AND id NOT IN
			(SELECT id FROM company_changes WHERE state='proposed' ORDER BY created_at DESC, rowid DESC LIMIT ?)`, MaxPendingChanges)
		return err
	})
	return ch, err
}

func patchPaths(p Patch) []string {
	out := make([]string, 0, len(p.Ops))
	for _, o := range p.Ops {
		out = append(out, strings.Join(o.Path, "."))
	}
	return out
}

func (s *Service) audit(ctx context.Context, actor, action, target string, detail map[string]any) (audit.Record, error) {
	return s.Audit.Append(ctx, audit.Entry{Actor: actor, Action: action, Target: target, Detail: detail})
}

// GetChange loads a stored change.
func (s *Service) GetChange(ctx context.Context, id string) (Change, error) {
	var ch Change
	var created int64
	var pj, aj string
	err := s.DB.QueryRowContext(ctx, `SELECT id, actor, created_at, base_version, patch_json, acks_json, summary, state, run_id FROM company_changes WHERE id=?`, id).
		Scan(&ch.ID, &ch.Actor, &created, &ch.BaseVersion, &pj, &aj, &ch.Summary, &ch.State, &ch.RunID)
	if errors.Is(err, sql.ErrNoRows) {
		return Change{}, ErrNotFound
	}
	if err != nil {
		return Change{}, err
	}
	ch.CreatedAt = time.Unix(created, 0).UTC()
	if ch.Patch, err = ParsePatch([]byte(pj)); err != nil {
		return Change{}, err
	}
	_ = json.Unmarshal([]byte(aj), &ch.Acks)
	return ch, nil
}

// Preview shows what a stored change would do against the current company.yaml.
func (s *Service) Preview(ctx context.Context, id string) (*Preview, error) {
	ch, err := s.GetChange(ctx, id)
	if err != nil {
		return nil, err
	}
	cur, err := s.Company.Get(ctx)
	if err != nil {
		return nil, err
	}
	pv, err := s.build(ctx, cur, ch.Patch, ch.Acks)
	if err != nil {
		return nil, err
	}
	pv.Change = ch
	pv.Stale = ch.BaseVersion != cur.Version && ch.State == ChangeProposed
	return pv, nil
}

// PreviewPatch previews a patch that is not stored (the wizard's review step).
func (s *Service) PreviewPatch(ctx context.Context, patch Patch, acks []string) (*Preview, error) {
	cur, err := s.Company.Get(ctx)
	if err != nil {
		return nil, err
	}
	return s.build(ctx, cur, patch, acks)
}

// Run states.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

// ModuleOutcome is a module's recorded state during or after a run.
type ModuleOutcome struct {
	ID, Action, State, LastError string
}

// Run is one UI-started reconcile.
type Run struct {
	ID, ChangeID, Actor, State, Error string
	StartedAt                         time.Time
	FinishedAt                        *time.Time
	Actions                           []reconcile.Action
	Failed                            []string
	Modules                           []ModuleOutcome
}

// Run loads a run with the live state of the modules it touches.
func (s *Service) Run(ctx context.Context, id string) (Run, error) {
	var r Run
	var started int64
	var finished sql.NullInt64
	var aj, fj string
	err := s.DB.QueryRowContext(ctx, `SELECT id, change_id, actor, state, started_at, finished_at, actions_json, failed_json, error FROM apply_runs WHERE id=?`, id).
		Scan(&r.ID, &r.ChangeID, &r.Actor, &r.State, &started, &finished, &aj, &fj, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	r.StartedAt = time.Unix(started, 0).UTC()
	if finished.Valid {
		t := time.Unix(finished.Int64, 0).UTC()
		r.FinishedAt = &t
	}
	_ = json.Unmarshal([]byte(aj), &r.Actions)
	_ = json.Unmarshal([]byte(fj), &r.Failed)
	for _, a := range r.Actions {
		mo := ModuleOutcome{ID: a.Module, Action: string(a.Kind), State: "pending"}
		if m, err := s.DB.GetModule(ctx, a.Module); err == nil {
			mo.State, mo.LastError = m.State, Sanitise(m.LastError)
		}
		r.Modules = append(r.Modules, mo)
	}
	return r, nil
}

// LatestRun returns the most recent run, or ErrNotFound.
func (s *Service) LatestRun(ctx context.Context) (Run, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM apply_runs ORDER BY started_at DESC, rowid DESC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	return s.Run(ctx, id)
}

func (s *Service) finishRun(ctx context.Context, id, state, errText string, actions []reconcile.Action, failed []string) {
	aj, _ := json.Marshal(actions)
	fj, _ := json.Marshal(failed)
	_, _ = s.DB.ExecContext(ctx, `UPDATE apply_runs SET state=?, finished_at=?, actions_json=?, failed_json=?, error=? WHERE id=?`,
		state, s.now().Unix(), string(aj), string(fj), Sanitise(errText), id)
}

// Apply writes a proposed change to company.yaml and reconciles. It refuses a stale or expired
// change, runs at most one apply at a time, records the intent in the audit log before the write
// (and refuses to write when it cannot) and the result after it. Applying an applied change returns
// its run. confirmed carries the confirmations given with this very request: BackupAck for every
// module in the preview's BackupStops (an acknowledgement stored with the proposal does not count).
func (s *Service) Apply(ctx context.Context, actor, id string, confirmed ...string) (Run, error) {
	ch, err := s.GetChange(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if ch.State == ChangeApplied {
		return s.Run(ctx, ch.RunID)
	}
	if s.now().Sub(ch.CreatedAt) > ChangeTTL {
		return Run{}, ErrExpired
	}
	cur, err := s.Company.Get(ctx)
	if err != nil {
		return Run{}, err
	}
	if cur.Version != ch.BaseVersion {
		return Run{}, ErrStale
	}
	pv, err := s.build(ctx, cur, ch.Patch, ch.Acks)
	if err != nil {
		return Run{}, err
	}
	if err := pv.Err(); err != nil {
		return Run{}, err
	}
	if err := missingBackupAcks(pv.BackupStops, confirmed); err != nil {
		return Run{}, err
	}
	runID, err := s.claimRun(ctx, actor, ch.ID)
	if err != nil {
		return Run{}, err
	}
	fail := func(e error) (Run, error) {
		s.finishRun(ctx, runID, RunFailed, e.Error(), nil, nil)
		return Run{}, e
	}
	if _, err := s.audit(ctx, actor, "company.change.begin", ch.ID, map[string]any{"paths": patchPaths(ch.Patch), "base": cur.Version, "run": runID}); err != nil {
		return fail(err)
	}
	newVersion, err := s.Company.Put(ctx, pv.newData, cur.Version)
	if errors.Is(err, ErrConflict) {
		return fail(ErrStale)
	}
	if err != nil {
		return fail(err)
	}
	_, _ = s.audit(ctx, actor, "company.change.saved", ch.ID, map[string]any{"version": newVersion, "run": runID})
	_, _ = s.DB.ExecContext(ctx, `UPDATE company_changes SET state='applied', applied_at=?, run_id=? WHERE id=?`, s.now().Unix(), runID, ch.ID)
	if err := s.approveConfirmed(ctx, actor, pv.Held, confirmed); err != nil {
		return fail(err)
	}
	s.start(runID, actor, ch.ID, pv.desired)
	return s.Run(ctx, runID)
}

// claimRun takes the single apply slot (a unique partial index makes this atomic).
func (s *Service) claimRun(ctx context.Context, actor, changeID string) (string, error) {
	// A run that never finished (crash) must not block applies forever.
	_, _ = s.DB.ExecContext(ctx, `UPDATE apply_runs SET state='failed', finished_at=?, error='interrupted' WHERE state='running' AND started_at < ?`,
		s.now().Unix(), s.now().Add(-ApplyTimeout-time.Minute).Unix())
	id := newID()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO apply_runs(id, change_id, actor, state, started_at) VALUES (?,?,?,?,?)`, id, changeID, actor, RunRunning, s.now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", ErrBusy
		}
		return "", err
	}
	return id, nil
}

// Recover marks runs that were interrupted by a restart as failed. can-core calls it at start.
func (s *Service) Recover(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE apply_runs SET state='failed', finished_at=?, error='interrupted by a restart' WHERE state='running'`, s.now().Unix())
	return err
}

func (s *Service) start(runID, actor, changeID string, d reconcile.Desired) {
	work := func() {
		ctx, cancel := context.WithTimeout(context.Background(), ApplyTimeout)
		defer cancel()
		if s.Runner == nil {
			s.finishRun(ctx, runID, RunFailed, "no reconciler is configured", nil, nil)
			return
		}
		res, err := s.Runner.Apply(ctx, d)
		state, msg := RunSucceeded, ""
		if err != nil || len(res.Failed) > 0 {
			state = RunFailed
		}
		if err != nil {
			msg = err.Error()
		}
		// A run that leaves out what was asked for must not read as a success.
		if left := notAppliedSummary(res.Held); left != "" {
			state = RunFailed
			msg = strings.TrimSpace(left + " " + msg)
		}
		s.finishRun(ctx, runID, state, msg, res.Actions, res.Failed)
		s.RecordReconcile(ctx, res, err)
		_, _ = s.audit(ctx, actor, "company.apply.done", changeID, map[string]any{"run": runID, "state": state, "failed": res.Failed})
	}
	if s.Async != nil {
		s.Async(work)
		return
	}
	go work()
}

// Purge destroys a disabled module's data. It is separate from disable on purpose: the operator must
// type the exact confirmation `purge <id>` (the page also re-checks their password and a distinct
// permission), the module must be disabled in the store and in company.yaml, and the intent is
// audited before anything is touched.
func (s *Service) Purge(ctx context.Context, actor, id, confirm string) error {
	if s.Purger == nil {
		return ErrNoPurger
	}
	if !moduleIDRe.MatchString(id) {
		return ErrNotPurgeable
	}
	if confirm != "purge "+id {
		return ErrConfirmation
	}
	m, err := s.DB.GetModule(ctx, id)
	if err != nil || m.State != store.StateDisabled {
		return ErrNotPurgeable
	}
	cur, err := s.Company.Get(ctx)
	if err != nil {
		return err
	}
	if c, _ := decodeCompany("company.yaml", cur.Data); c == nil || enabledSet(c)[id] {
		return ErrNotPurgeable
	}
	if _, err := s.audit(ctx, actor, "module.purge.begin", id, nil); err != nil {
		return err
	}
	if err := s.Purger.Purge(ctx, id); err != nil {
		_, _ = s.audit(ctx, actor, "module.purge.failed", id, map[string]any{"error": Sanitise(err.Error())})
		return fmt.Errorf("purge failed: %s", Sanitise(err.Error()))
	}
	if err := s.DB.DeleteDisabledModule(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if s.OnPurged != nil {
		if err := s.OnPurged(id); err != nil {
			_, _ = s.audit(ctx, actor, "module.purge.failed", id, map[string]any{"error": Sanitise(err.Error())})
			return fmt.Errorf("the module was purged but its backup dumps were not removed: %s", Sanitise(err.Error()))
		}
	}
	_, err = s.audit(ctx, actor, "module.purge.done", id, nil)
	return err
}
