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
	"sort"

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
	// Domain is the company domain from company.yaml (a module's public host is <subdomain>.<domain>).
	Domain string
	// Rotate lists the keys of the module's `can-module` Secret to regenerate in this run (operator
	// requests, see store.RequestRotation). CAN_WEBHOOK_SECRET is rotated by the reconciler itself
	// through HMACSecret; the installer only validates the name.
	Rotate []string
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
}

// Desired is the validated desired state.
type Desired struct {
	Company   *config.Company
	Manifests map[string]*config.Module // enabled modules only
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
	Failed  []string `json:"failed,omitempty"`
}

// Reconciler reconciles desired with recorded state.
type Reconciler struct {
	db        *store.DB
	reg       *registry.Registry
	audit     Appender
	Installer ModuleInstaller
	Actor     string
	// getModule reads the recorded module (db.GetModule; a seam for tests).
	getModule func(ctx context.Context, id string) (store.Module, error)
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
	d := Desired{Company: c, Manifests: map[string]*config.Module{}}
	for id, e := range c.Modules {
		if !e.IsEnabled() {
			continue
		}
		path, ok := validate.ManifestPath(modulesDir, id)
		if !ok {
			continue // reported by validate; unreachable after the error check
		}
		md, mps := config.Load(path)
		if md == nil {
			return Desired{}, mps
		}
		m, err := config.DecodeModule(md)
		if err != nil {
			return Desired{}, []problem.Problem{{File: path, Rule: "parse.decode", Severity: problem.Error, Message: err.Error()}}
		}
		d.Manifests[id] = m
	}
	return d, ps // ps may carry warnings
}

func settingsHash(m *config.Module, settings map[string]any) string {
	b, _ := json.Marshal(settings) // map keys sorted
	h := sha256.Sum256(append([]byte(m.Version+"\x00"), b...))
	return hex.EncodeToString(h[:])
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
		if _, wanted := want[m.ID]; !wanted && (m.State == store.StateEnabled || m.State == store.StateFailed) {
			reason := "no longer enabled in company.yaml"
			disables = append(disables, Action{Disable, m.ID, reason})
		}
	}
	for _, id := range installOrder(want) {
		man := want[id]
		rec, ok := have[id]
		hash := settingsHash(man, d.Company.Modules[id].Settings)
		switch {
		case !ok:
			enables = append(enables, Action{Install, id, "not installed"})
		case rec.State == store.StateFailed:
			enables = append(enables, Action{Install, id, "retrying after failure: " + rec.LastError})
		case rec.State == store.StateDisabled:
			enables = append(enables, Action{Enable, id, "enabled in company.yaml"})
		case rec.Version != man.Version:
			enables = append(enables, Action{Upgrade, id, fmt.Sprintf("version %s -> %s", rec.Version, man.Version)})
		case rec.SettingsHash != hash:
			enables = append(enables, Action{Upgrade, id, "settings changed"})
		default:
			pend, err := r.db.PendingRotations(ctx, id)
			if err != nil {
				return nil, err
			}
			if len(pend) > 0 {
				enables = append(enables, Action{Upgrade, id, "secret rotation requested"})
			}
		}
	}
	return append(disables, enables...), nil
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
	res := Result{Actions: actions}
	if opt.DryRun {
		return res, nil
	}
	var errs []error
	for _, a := range actions {
		var err error
		if a.Kind == Disable {
			err = r.disable(ctx, a)
		} else {
			err = r.enable(ctx, d, a)
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
	secret := prev.HMACSecret
	if perr != nil || secret == "" {
		secret = newSecret()
	}
	rotate, err := r.db.PendingRotations(ctx, a.Module)
	if err != nil {
		return err
	}
	for _, k := range rotate {
		if k == modsecrets.WebhookKey {
			secret = newSecret() // takes effect only if the install succeeds (the recorded one is kept otherwise)
		}
	}
	settings := d.Company.Modules[a.Module].Settings
	rec := store.Module{
		ID: a.Module, Version: man.Version, Namespace: "mod-" + a.Module, ServiceAccount: a.Module,
		State: store.StateEnabled, SettingsHash: settingsHash(man, settings), HMACSecret: secret, Manifest: *man,
	}
	ierr := r.Installer.Enable(ctx, Spec{ID: a.Module, Version: man.Version, Namespace: rec.Namespace, ServiceAccount: rec.ServiceAccount, Settings: settings, Manifest: man, HMACSecret: secret, Domain: d.Company.Domain, Rotate: rotate})
	if ierr != nil && a.Kind == Upgrade && perr == nil && prev.State == store.StateEnabled {
		// `helm upgrade --atomic` rolled back, so the previous release is still running: keep its
		// recorded state (token identity, capabilities, subscriptions) and only note the error.
		// The next apply sees the unchanged version/settings hash and retries the upgrade.
		_ = r.db.SetModuleState(ctx, a.Module, store.StateEnabled, ierr.Error())
		r.log(ctx, "module.upgrade-failed", a, map[string]any{"error": ierr.Error(), "kept_version": prev.Version})
		return ierr
	}
	if ierr != nil {
		rec.State, rec.LastError = store.StateFailed, ierr.Error()
	}
	if err := r.reg.Register(ctx, rec); err != nil {
		return err
	}
	if ierr != nil {
		r.log(ctx, "module.failed", a, map[string]any{"error": ierr.Error()})
		return ierr
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
