// Package reconcile implements `can apply`: it compares the desired state (company.yaml plus the
// manifests of its enabled modules) with the state recorded in can-core's store and drives a
// ModuleInstaller to close the gap. All Kubernetes and Helm work is behind ModuleInstaller; the
// real Helm installer is a follow-up (specs/005-can-core-mvp/research.md R7).
package reconcile

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/validate"
)

// Spec is everything an installer needs to run one module.
type Spec struct {
	ID             string
	Version        string
	Namespace      string
	ServiceAccount string
	Settings       map[string]any
	Manifest       *config.Module
	// HMACSecret signs can-core's webhooks to this module; the installer injects it into the
	// module's environment (SPEC §3.1).
	HMACSecret string
	// RecordSecret is the signing secret the installed release record was written under, when a
	// rotation makes it differ from HMACSecret.
	RecordSecret string
	// Domain is the company domain from company.yaml (a module's public host is <subdomain>.<domain>).
	Domain string
	// CompanyName is company.yaml's name, for modules that show it (see companyRevision).
	CompanyName string
	// Email is the email block of the company.yaml being applied (nil without one). It is the one
	// being applied, not the mounted copy, which can lag behind.
	Email *config.Email
	// Rotate lists the keys of the module's `can-module` Secret to regenerate in this run (operator
	// requests, see store.RequestRotation). CAN_WEBHOOK_SECRET is rotated by the reconciler itself
	// through HMACSecret; the installer only validates the name.
	Rotate []string
	// Expose is company.yaml's modules.<id>.expose (specs/040-module-routing).
	Expose config.Expose
	// Values are chart value overrides the installer derives itself; the reconciler leaves it empty.
	Values map[string]any
}

// Routing is what a RouteAware installer would route for a Spec.
type Routing struct {
	// Fingerprint changes whenever the routes would: it is part of the recorded settings hash.
	Fingerprint string
	// UIHost is the host of the module's web UI when it is routed.
	UIHost string
}

// RouteAware is implemented by installers that route modules.
type RouteAware interface {
	Routing(ctx context.Context, s Spec) (Routing, error)
}

// RelayAware is implemented by installers that hand a module the SMTP relay.
type RelayAware interface {
	// RelayFingerprint changes whenever what the module would be handed does: it is part of the
	// recorded settings hash. It is "" when the module is given nothing.
	RelayFingerprint(ctx context.Context, s Spec) (string, error)
}

// ModuleInstaller applies and removes module workloads. Implementations must be idempotent.
type ModuleInstaller interface {
	// Enable installs or upgrades the module and leaves it running
	// (`helm upgrade --install --atomic` plus namespace, Secrets and NetworkPolicies).
	Enable(ctx context.Context, s Spec) error
	// Disable scales the module to zero, keeping its data.
	Disable(ctx context.Context, id string) error
}

// Kind of reconciliation step.
type Kind string

// Action kinds.
const (
	Install Kind = "install" // never recorded, or previously failed
	Upgrade Kind = "upgrade" // version or settings changed
	Enable  Kind = "enable"  // recorded as disabled, wanted enabled
	Disable Kind = "disable" // enabled but no longer wanted
)

// Action is one planned step.
type Action struct {
	Kind   Kind   `json:"kind"`
	Module string `json:"module"`
	Reason string `json:"reason"`
	// Widens is what the module may newly do after this step (see Widening).
	Widens []string `json:"widens,omitempty"`
	// Held, when set, says why Apply will not run this step now: HeldApproval or HeldRetry.
	Held string `json:"held,omitempty"`
	// Approval is what approves a step held for HeldApproval.
	Approval string `json:"approval,omitempty"`

	// NotApplied is what was asked of a module whose upgrade is held for approval and does not
	// reach it: a held module gets no change at all. Urgent says that this includes taking away
	// exposure or access (or that it cannot be ruled out).
	NotApplied []string `json:"not_applied,omitempty"`
	Urgent     bool     `json:"urgent,omitempty"`

	target   string // identifies what is wanted, for the retry delay
	baseline string // what the running release has, learnt from an old record (store.Module.Applied)
}

// Why a planned step is held.
const (
	HeldApproval = "approval" // it widens what the module may do and nobody approved that
	HeldRetry    = "retry"    // the same upgrade failed; it is tried again later
)

// SplitHeld separates the steps that run from those that are held.
func SplitHeld(actions []Action) (run, held []Action) {
	for _, a := range actions {
		if a.Held == "" {
			run = append(run, a)
		} else {
			held = append(held, a)
		}
	}
	return run, held
}

// Desired is the validated desired state.
type Desired struct {
	Company   *config.Company
	Manifests map[string]*config.Module // enabled modules only
	// Content is what each module is installed from; a module missing here is compared by its
	// manifest alone.
	Content map[string]Content
}

func (d Desired) content(id string) Content {
	if c, ok := d.Content[id]; ok {
		return c
	}
	return manifestContent(d.Manifests[id])
}

// enabled returns the manifests of modules that company.yaml actually enables.
func (d Desired) enabled() map[string]*config.Module {
	out := map[string]*config.Module{}
	for id, m := range d.Manifests {
		if e, ok := d.Company.Modules[id]; ok && e.IsEnabled() {
			out[id] = m
		}
	}
	return out
}

// Options for Apply.
type Options struct{ DryRun bool }

// Result of Apply.
type Result struct {
	Actions []Action `json:"actions"`
	// Held are the planned steps that were not run.
	Held   []Action `json:"held,omitempty"`
	Failed []string `json:"failed,omitempty"`
}

// Reconciler reconciles desired with recorded state.
type Reconciler struct {
	db        *store.DB
	reg       *registry.Registry
	audit     Appender
	Installer ModuleInstaller
	Actor     string
	// FixedWebhookSecret, when set, is recorded for every module instead of a generated one. Only
	// can-core's development stack sets it: there the modules are started by Docker Compose and are
	// given the secret by their environment, not by an installer.
	FixedWebhookSecret string
	// Keep, when set, names recorded modules that are not company.yaml's to disable. Only can-core's
	// development stack sets it, for the identities CAN_DEV_MODULE_TOKENS registers.
	Keep func(store.Module) bool
	// RequireApproval holds an upgrade of a running module that widens what it may do until it is
	// approved (store.ApproveUpgrade). The running release and its record stay as they are.
	RequireApproval bool
	// Backoff delays the retry of an upgrade that failed, so that a release that cannot start does
	// not restart the module on every reconcile.
	Backoff bool
	mu      sync.Mutex
	failed  map[string]failure
	// getModule reads the recorded module (db.GetModule; a seam for tests).
	getModule func(ctx context.Context, id string) (store.Module, error)
}

type failure struct {
	target string
	count  int
	next   time.Time
}

// Appender is the audit write interface.
type Appender interface {
	Append(ctx context.Context, e audit.Entry) (audit.Record, error)
}

// New returns a Reconciler.
func New(db *store.DB, reg *registry.Registry, a Appender, inst ModuleInstaller) *Reconciler {
	return &Reconciler{db: db, reg: reg, audit: a, Installer: inst, Actor: "can-apply", getModule: db.GetModule}
}

// LoadDesired validates company.yaml with the manifests of its enabled modules (the same checks as
// `can validate`) and returns the desired state. Any error-severity problem aborts.
func LoadDesired(companyFile, modulesDir string) (Desired, []problem.Problem) {
	data, err := os.ReadFile(companyFile) //nolint:gosec // operator-configured path
	if err != nil {
		return Desired{}, []problem.Problem{{File: companyFile, Rule: "parse.read", Severity: problem.Error, Message: err.Error()}}
	}
	return LoadDesiredBytes(companyFile, data, modulesDir)
}

// LoadDesiredBytes is LoadDesired for company.yaml content held in memory (the admin UI applies the
// stored document directly, without waiting for the mounted ConfigMap to refresh).
func LoadDesiredBytes(name string, data []byte, modulesDir string) (Desired, []problem.Problem) {
	ps := validate.CompanyBytes(name, data, modulesDir)
	if problem.HasErrors(ps) {
		return Desired{}, ps
	}
	companyFile := name
	doc, lp := config.LoadBytes(name, data)
	if doc == nil {
		return Desired{}, lp
	}
	c, err := config.DecodeCompany(doc)
	if err != nil {
		return Desired{}, []problem.Problem{{File: companyFile, Rule: "parse.decode", Severity: problem.Error, Message: err.Error()}}
	}
	d := Desired{Company: c, Manifests: map[string]*config.Module{}, Content: map[string]Content{}}
	for id, e := range c.Modules {
		if !e.IsEnabled() {
			continue
		}
		path, ok := validate.ManifestPath(modulesDir, id)
		if !ok {
			continue // reported by validate; unreachable after the error check
		}
		md, mps := validate.LoadManifest(path)
		if md == nil {
			return Desired{}, mps
		}
		m, err := config.DecodeModule(md)
		if err != nil {
			return Desired{}, []problem.Problem{{File: path, Rule: "parse.decode", Severity: problem.Error, Message: err.Error()}}
		}
		d.Manifests[id] = m
		content, err := ContentOf(path, m)
		if err != nil {
			return Desired{}, []problem.Problem{{File: path, Rule: "parse.read", Severity: problem.Error, Message: err.Error()}}
		}
		d.Content[id] = content
	}
	return d, ps // ps may carry warnings
}

func settingsHash(m *config.Module, settings map[string]any, routes string) string {
	b, _ := json.Marshal(settings) // map keys sorted
	if routes != "" {
		b = append(append(b, 0), routes...)
	}
	if !m.Runtime.InternetEgress {
		// In the hash so that a module installed when every module could reach the internet is
		// applied once more, which tightens its network policy.
		b = append(b, "\x00no-internet"...)
	}
	h := sha256.Sum256(append([]byte(m.Version+"\x00"), b...))
	return hex.EncodeToString(h[:])
}

// companyRevision is what, beyond its own settings, a module's installation depends on: the facts
// of company.yaml the installer writes into it (jobs, files), and the revision of the credentials
// can-core wires between modules (people, provisioning). A change is an upgrade, so that a renamed
// company reaches the module and an installation made by an older release gets the new values.
func companyRevision(c *config.Company, id string) string {
	switch id {
	case "jobs", "files":
		return "1\x00" + c.Name + "\x00" + c.Domain
	case "people", "provisioning":
		return "1"
	}
	return ""
}

// RelayOptIn reports whether the company lets this module have the SMTP relay's credentials:
// email.modules.<id>.enabled must be written as true. Absent means no, unlike for email.send,
// because the credentials leave can-core for a third-party application.
func RelayOptIn(e *config.Email, module string) bool {
	if e == nil {
		return false
	}
	on := e.Modules[module].Enabled
	return on != nil && *on
}

// specHash is settingsHash of the module as company c has it. For a module that speaks SMTP itself
// and is opted in it also covers email.relay and the relay can-core can read (its Secret reaches the
// mounted volume a minute or two after it is written), so that opting in, opting out (which empties
// the module's Secret) or a change of either is an upgrade.
func specHash(c *config.Company, id string, man *config.Module, routes, relay string) string {
	h := settingsHash(man, c.Modules[id].Settings, routes)
	if rev := companyRevision(c, id); rev != "" {
		sum := sha256.Sum256([]byte(h + "\x00company\x00" + rev))
		h = hex.EncodeToString(sum[:])
	}
	if !modsecrets.WantsRelay(id) || !RelayOptIn(c.Email, id) {
		if relay != "" { // a credential wired in from another module (WiringAware)
			sum := sha256.Sum256([]byte(h + "\x00wired\x00" + relay))
			return hex.EncodeToString(sum[:])
		}
		return h
	}
	b, _ := json.Marshal(c.Email.Relay)
	sum := sha256.Sum256(append([]byte(h+"\x00relay\x00"), b...))
	h = hex.EncodeToString(sum[:])
	if relay == "" {
		return h
	}
	sum = sha256.Sum256([]byte(h + "\x00" + relay))
	return hex.EncodeToString(sum[:])
}

// WiringAware is implemented by installers that copy a credential of one module into another: the
// fingerprint changes when the source does (the people module installed afresh), so that the copy
// is made again.
type WiringAware interface {
	WiringFingerprint(ctx context.Context, s Spec) (string, error)
}

// relay is the fingerprint of what is written into the module from outside its own settings: the
// SMTP relay, or another module's credential.
func (r *Reconciler) relay(ctx context.Context, d Desired, id, hmacSecret string) (string, error) {
	if wa, ok := r.Installer.(WiringAware); ok && id == "provisioning" {
		return wa.WiringFingerprint(ctx, Spec{ID: id, Manifest: d.Manifests[id], HMACSecret: hmacSecret})
	}
	ra, ok := r.Installer.(RelayAware)
	if !ok || !modsecrets.WantsRelay(id) || !RelayOptIn(d.Company.Email, id) {
		return "", nil
	}
	return ra.RelayFingerprint(ctx, Spec{ID: id, Manifest: d.Manifests[id], Domain: d.Company.Domain, Email: d.Company.Email, HMACSecret: hmacSecret})
}

func (r *Reconciler) routing(ctx context.Context, d Desired, id string) (Routing, error) {
	ra, ok := r.Installer.(RouteAware)
	if !ok {
		return Routing{}, nil
	}
	return ra.Routing(ctx, Spec{ID: id, Manifest: d.Manifests[id], Domain: d.Company.Domain, Expose: d.Company.Modules[id].Exposure()})
}

func newSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// ErrNotDeployable is returned when a module whose manifest says status: not-deployable is
// asked to be enabled.
var ErrNotDeployable = errors.New("module is not-deployable (known not to run as shipped) and cannot be enabled")

func sortedIDs(ms map[string]*config.Module) []string {
	ids := make([]string, 0, len(ms))
	for id := range ms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Plan lists the actions Apply would take, in execution order.
func (r *Reconciler) Plan(ctx context.Context, d Desired) ([]Action, error) {
	recorded, err := r.db.ListModules(ctx)
	if err != nil {
		return nil, err
	}
	have := map[string]store.Module{}
	for _, m := range recorded {
		have[m.ID] = m
	}
	want := d.enabled()
	// Defence in depth: validation already refuses these, but a Desired built another way must
	// not install a module the manifest says cannot run.
	for _, id := range sortedIDs(want) {
		if want[id].Status == "not-deployable" {
			return nil, fmt.Errorf("%w: %s", ErrNotDeployable, id)
		}
	}
	var disables, enables []Action
	for _, m := range recorded { // ordered by id
		if _, wanted := want[m.ID]; !wanted && (m.State == store.StateEnabled || m.State == store.StateFailed) && (r.Keep == nil || !r.Keep(m)) {
			reason := "no longer enabled in company.yaml"
			disables = append(disables, Action{Kind: Disable, Module: m.ID, Reason: reason})
		}
	}
	for _, id := range installOrder(want) {
		man := want[id]
		rec, ok := have[id]
		// A routing read that fails must not stop the plan (disables above all): the hash is then
		// unknown and is not compared.
		rt, rerr := r.routing(ctx, d, id)
		relay, lerr := r.relay(ctx, d, id, rec.HMACSecret)
		hash := specHash(d.Company, id, man, rt.Fingerprint, relay)
		var a *Action
		switch {
		case !ok:
			a = &Action{Kind: Install, Reason: "not installed"}
		case rec.State == store.StateFailed:
			a = &Action{Kind: Install, Reason: "retrying after failure: " + rec.LastError}
		case rec.State == store.StateDisabled:
			a = &Action{Kind: Enable, Reason: "enabled in company.yaml"}
		default:
			var why []string
			if rec.Version != man.Version {
				why = append(why, fmt.Sprintf("version %s -> %s", rec.Version, man.Version))
			} else if rerr == nil && lerr == nil && rec.SettingsHash != hash {
				why = append(why, "settings changed")
			}
			var was Content
			if rec.Content == "" || json.Unmarshal([]byte(rec.Content), &was) != nil {
				// What an old record's hash was made of is not known, so "settings changed"
				// would be a guess.
				why = slices.DeleteFunc(why, func(s string) bool { return s == "settings changed" })
				why = append(why, "recording the installed manifest and chart (once)")
			} else {
				why = append(why, d.content(id).changes(was)...)
			}
			pend, err := r.db.PendingRotations(ctx, id)
			if err != nil {
				return nil, err
			}
			if len(why) == 0 && len(pend) > 0 {
				why = []string{"secret rotation requested"}
			}
			if len(why) > 0 {
				a = &Action{Kind: Upgrade, Reason: strings.Join(why, "; ")}
				a.target = digest([]byte(man.Version), []byte(hash), []byte(d.content(id).key()), []byte(strings.Join(pend, ",")))
			}
		}
		if a == nil {
			continue
		}
		a.Module = id
		if ok && a.Kind != Install {
			a.Widens = Widening(&rec.Manifest, man)
			if newInternetAccess(rec.Content, man) {
				a.Widens = append(a.Widens, "internet access")
			}
		}
		if a.Kind == Upgrade {
			r.hold(a, rec)
		}
		if a.Held == HeldApproval {
			if err := r.notApplied(ctx, d, a, rec, relay, rerr, lerr); err != nil {
				return nil, err
			}
		}
		enables = append(enables, *a)
	}
	return append(disables, enables...), nil
}

// hold marks an upgrade of a running module that must not run now.
func (r *Reconciler) hold(a *Action, rec store.Module) {
	if r.RequireApproval && len(a.Widens) > 0 {
		approval := approvalOf(a.Widens)
		if rec.ApprovedUpgrade != approval && rec.ApprovedUpgrade != "*" {
			a.Held, a.Approval = HeldApproval, approval
			return
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.failed[a.Module]; r.Backoff && ok && f.target == a.target && r.db.Now().Before(f.next) {
		a.Held = HeldRetry
		a.Reason += fmt.Sprintf("; failed %d times, next try after %s", f.count, f.next.UTC().Format("15:04:05 UTC"))
	}
}

// outcome remembers a failed upgrade: 1, 2, 4 ... minutes up to an hour until the next try of the
// same thing. It is kept in memory: a restarted can-core tries at once.
func (r *Reconciler) outcome(a Action, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil || a.Kind != Upgrade {
		delete(r.failed, a.Module)
		return
	}
	f := r.failed[a.Module]
	if f.target != a.target {
		f = failure{target: a.target}
	}
	f.count++
	f.next = r.db.Now().Add(min(time.Minute<<min(f.count-1, 6), time.Hour))
	if r.failed == nil {
		r.failed = map[string]failure{}
	}
	r.failed[a.Module] = f
}

// recordHeld keeps store.Module.Pending in step with the plan, so that the admin UI and `can status`
// can show what waits for approval.
func (r *Reconciler) recordHeld(ctx context.Context, held []Action) error {
	want := map[string]Action{}
	for _, a := range held {
		if a.Held == HeldApproval {
			want[a.Module] = a
		}
	}
	mods, err := r.db.ListModules(ctx)
	if err != nil {
		return err
	}
	for _, m := range mods {
		pending := ""
		a, ok := want[m.ID]
		if ok {
			b, _ := json.Marshal(Pending{Approval: a.Approval, Widens: a.Widens, NotApplied: a.NotApplied, Urgent: a.Urgent})
			if a.baseline != "" && m.Applied == "" {
				if err := r.db.SetApplied(ctx, m.ID, a.baseline); err != nil {
					return err
				}
			}
			pending = string(b)
		}
		if pending == m.Pending {
			continue
		}
		if err := r.db.SetPendingUpgrade(ctx, m.ID, pending); err != nil {
			return err
		}
		if ok {
			r.log(ctx, "module.upgrade-held", a, map[string]any{"widens": a.Widens, "not_applied": a.NotApplied})
		}
	}
	return nil
}

// installOrder sorts module ids so providers of a required capability come before their
// consumers; ties and cycles fall back to id order.
func installOrder(ms map[string]*config.Module) []string {
	ids := make([]string, 0, len(ms))
	for id := range ms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	provider := map[string][]string{}
	for _, id := range ids {
		for _, p := range ms[id].Provides {
			if !config.ProvidesServed(p.Status) {
				continue
			}
			provider[p.Capability] = append(provider[p.Capability], id)
		}
	}
	done, visiting := map[string]bool{}, map[string]bool{}
	var out []string
	var visit func(id string)
	visit = func(id string) {
		if done[id] || visiting[id] {
			return
		}
		visiting[id] = true
		for _, c := range ms[id].Requires.Capabilities {
			for _, p := range provider[c] {
				if p != id {
					visit(p)
				}
			}
		}
		visiting[id] = false
		done[id] = true
		out = append(out, id)
	}
	for _, id := range ids {
		visit(id)
	}
	return out
}

// Apply executes the plan. A failing module is recorded as failed and the rest continue; the
// returned error names every failure. Disabled modules keep their data and signing secret.
func (r *Reconciler) Apply(ctx context.Context, d Desired, opt Options) (Result, error) {
	actions, err := r.Plan(ctx, d)
	if err != nil {
		return Result{}, err
	}
	run, held := SplitHeld(actions)
	res := Result{Actions: run, Held: held}
	if opt.DryRun {
		return res, nil
	}
	var errs []error
	if err := r.recordHeld(ctx, held); err != nil {
		errs = append(errs, err)
	}
	for _, a := range run {
		var err error
		if a.Kind == Disable {
			err = r.disable(ctx, a)
		} else {
			err = r.enable(ctx, d, a)
			r.outcome(a, err)
		}
		if err != nil {
			res.Failed = append(res.Failed, a.Module)
			errs = append(errs, fmt.Errorf("%s %s: %w", a.Kind, a.Module, err))
		}
	}
	if err := r.reg.SetPrimaries(ctx, d.Company.Primary); err != nil {
		errs = append(errs, err)
	}
	return res, errors.Join(errs...)
}

func (r *Reconciler) enable(ctx context.Context, d Desired, a Action) error {
	man := d.Manifests[a.Module]
	if _, _, err := registry.FromManifest(man); err != nil { // refuse before touching the cluster
		r.log(ctx, "module.failed", a, map[string]any{"error": err.Error()})
		return err
	}
	// Refuse a manifest the registry would refuse (reserved or already-owned names) before anything
	// is installed: a failing Register after the Helm release would leave it running unrecorded.
	if err := r.reg.PreCheck(ctx, store.Module{ID: a.Module, Version: man.Version, State: store.StateEnabled, Manifest: *man}); err != nil {
		r.log(ctx, "module.failed", a, map[string]any{"error": err.Error()})
		return err
	}
	prev, perr := r.getModule(ctx, a.Module)
	if perr != nil && !errors.Is(perr, store.ErrNotFound) {
		// Any error but "no such module" leaves the recorded signing secret unknown: generating a
		// new one would make the running module reject every webhook. Fail the enable instead.
		err := fmt.Errorf("reading the recorded module: %w", perr)
		r.log(ctx, "module.failed", a, map[string]any{"error": err.Error()})
		return err
	}
	rotate, err := r.db.PendingRotations(ctx, a.Module)
	if err != nil {
		return err
	}
	secret := prev.HMACSecret
	switch {
	case r.FixedWebhookSecret != "":
		secret = r.FixedWebhookSecret
	case secret == "" || slices.Contains(rotate, modsecrets.WebhookKey):
		// Stored before the installer sees it and reused by every retry: whatever of it reaches
		// the cluster (the module's Secret, the release record's signature) is still known after
		// a failure or a crash. It becomes the recorded secret only when the install succeeds.
		if secret, err = r.db.PendingSecret(ctx, a.Module, newSecret()); err != nil {
			return err
		}
	}
	settings := d.Company.Modules[a.Module].Settings
	// On an error the installer decides (it fails a module that needs the routing facts); the
	// recorded hash then differs from the next good one, so the module is applied again.
	rt, rerr := r.routing(ctx, d, a.Module)
	if rerr != nil {
		rt = Routing{}
	}
	// Read before the installer does: a relay that changes in between differs from this and is
	// applied by the next run. An unreadable one fails in the installer.
	relay, lerr := r.relay(ctx, d, a.Module, secret)
	if lerr != nil {
		relay = ""
	}
	rec := store.Module{
		ID: a.Module, Version: man.Version, Namespace: "mod-" + a.Module, ServiceAccount: a.Module,
		State: store.StateEnabled, SettingsHash: specHash(d.Company, a.Module, man, rt.Fingerprint, relay), HMACSecret: secret, Manifest: *man,
		Content: d.content(a.Module).encode(), Applied: appliedOf(d, a.Module, relay).encode(),
	}
	ierr := r.Installer.Enable(ctx, Spec{ID: a.Module, Version: man.Version, Namespace: rec.Namespace, ServiceAccount: rec.ServiceAccount, Settings: settings, Manifest: man, HMACSecret: secret, RecordSecret: prev.HMACSecret, Domain: d.Company.Domain, CompanyName: d.Company.Name, Email: d.Company.Email, Rotate: rotate, Expose: d.Company.Modules[a.Module].Exposure()})
	if ierr != nil && a.Kind == Upgrade && perr == nil && prev.State == store.StateEnabled {
		// `helm upgrade --atomic` rolled back, so the previous release is still running: keep its
		// recorded state (token identity, capabilities, subscriptions) and only note the error.
		// The settings hash is forgotten, so the next apply retries whatever is wanted then. The
		// rollback does not cover everything a failed upgrade did (routes already removed, Secrets
		// rewritten), so reverting company.yaml to the recorded state must still be an upgrade.
		_ = r.db.SetModuleState(ctx, a.Module, store.StateEnabled, ierr.Error())
		if err := r.db.ForgetSettingsHash(ctx, a.Module); err != nil {
			r.log(ctx, "module.upgrade-failed", a, map[string]any{"error": "the upgrade is not marked for retry: " + err.Error()})
		}
		r.log(ctx, "module.upgrade-failed", a, map[string]any{"error": ierr.Error(), "kept_version": prev.Version})
		return ierr
	}
	if ierr != nil {
		rec.State, rec.LastError = store.StateFailed, ierr.Error()
		if prev.HMACSecret != "" {
			rec.HMACSecret = prev.HMACSecret
		}
	} else {
		rec.Manifest.Runtime.RoutedUIHost = rt.UIHost
	}
	if err := r.reg.Register(ctx, rec); err != nil {
		return err
	}
	if ierr != nil {
		r.log(ctx, "module.failed", a, map[string]any{"error": ierr.Error()})
		return ierr
	}
	// Before the rotation requests: a request left over after a crash rotates once more, whereas a
	// pending secret left over would make the next rotation reuse the current secret.
	if err := r.db.ClearPendingSecret(ctx, a.Module); err != nil {
		return err
	}
	if err := r.db.ClearRotations(ctx, a.Module, rotate); err != nil {
		return err
	}
	r.log(ctx, "module."+string(a.Kind), a, map[string]any{"version": man.Version})
	return nil
}

func (r *Reconciler) disable(ctx context.Context, a Action) error {
	if err := r.Installer.Disable(ctx, a.Module); err != nil {
		_ = r.db.SetModuleState(ctx, a.Module, store.StateFailed, err.Error())
		r.log(ctx, "module.failed", a, map[string]any{"error": err.Error()})
		return err
	}
	if err := r.db.SetModuleState(ctx, a.Module, store.StateDisabled, ""); err != nil {
		return err
	}
	r.log(ctx, "module.disable", a, nil)
	return nil
}

func (r *Reconciler) log(ctx context.Context, action string, a Action, extra map[string]any) {
	detail := map[string]any{"reason": a.Reason}
	for k, v := range extra {
		detail[k] = v
	}
	_, _ = r.audit.Append(ctx, audit.Entry{Actor: r.Actor, Action: action, Target: a.Module, Detail: detail})
}
