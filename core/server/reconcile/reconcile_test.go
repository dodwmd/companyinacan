package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type rig struct {
	r   *Reconciler
	db  *store.DB
	fk  *FakeInstaller
	reg *registry.Registry
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fk := NewFakeInstaller()
	reg := registry.New(db, "http://can-core")
	return &rig{r: New(db, reg, audit.New(db), fk), db: db, fk: fk, reg: reg}
}

func mf(id, version string, requires []string, provides ...string) *config.Module {
	m := &config.Module{ID: id, Version: version}
	m.Requires.Capabilities = requires
	for i, c := range provides {
		m.Provides = append(m.Provides, struct {
			Capability string `yaml:"capability"`
			Schema     string `yaml:"schema"`
			Service    string `yaml:"service"`
			Port       int    `yaml:"port"`
			Status     string `yaml:"status"`
		}{Capability: c, Service: id, Port: 9000 + i})
	}
	return m
}

func desired(primary map[string]string, enabled map[string]bool, ms ...*config.Module) Desired {
	d := Desired{Company: &config.Company{Name: "T", Modules: map[string]config.ModuleEntry{}, Primary: primary}, Manifests: map[string]*config.Module{}}
	for _, m := range ms {
		on := true
		if v, ok := enabled[m.ID]; ok {
			on = v
		}
		d.Company.Modules[m.ID] = config.ModuleEntry{Enabled: &on}
		d.Manifests[m.ID] = m
	}
	return d
}

func kinds(as []Action) string {
	var s []string
	for _, a := range as {
		s = append(s, string(a.Kind)+":"+a.Module)
	}
	return strings.Join(s, ",")
}

// The epic's "done when": a stub module is enabled, receives injected config, and is disabled again.
func TestEnableInjectsConfigThenDisable(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	stub := mf("stub", "0.1.0", nil, "tickets")
	d := desired(nil, nil, stub)
	d.Company.Modules["stub"] = config.ModuleEntry{Settings: map[string]any{"greeting": "hi"}}

	res, err := g.r.Apply(ctx, d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if kinds(res.Actions) != "install:stub" {
		t.Fatalf("%s", kinds(res.Actions))
	}
	got, ok := g.fk.Enabled["stub"]
	if !ok || got.Settings["greeting"] != "hi" || got.Namespace != "mod-stub" || got.ServiceAccount != "stub" || len(got.HMACSecret) != 64 {
		t.Fatalf("installer received %+v", got)
	}
	rec, _ := g.db.GetModule(ctx, "stub")
	if rec.State != store.StateEnabled || rec.HMACSecret != got.HMACSecret {
		t.Fatalf("%+v", rec)
	}
	if res2, err := g.reg.Resolve(ctx, "tickets"); err != nil || res2.Providers[0].ModuleID != "stub" {
		t.Fatalf("capability not registered: %+v %v", res2, err)
	}

	// disable
	off := false
	d.Company.Modules["stub"] = config.ModuleEntry{Enabled: &off}
	res, err = g.r.Apply(ctx, d, Options{})
	if err != nil || kinds(res.Actions) != "disable:stub" {
		t.Fatalf("%v %v", kinds(res.Actions), err)
	}
	if _, still := g.fk.Enabled["stub"]; still {
		t.Fatal("installer still has the module enabled")
	}
	rec, _ = g.db.GetModule(ctx, "stub")
	if rec.State != store.StateDisabled {
		t.Fatalf("%+v", rec)
	}
	if _, err := g.reg.Resolve(ctx, "tickets"); !errors.Is(err, registry.ErrNoProvider) {
		t.Fatalf("disabled module still provides: %v", err)
	}
	// data kept: the record (and its signing secret) survive, so re-enable reuses them
	off2 := true
	d.Company.Modules["stub"] = config.ModuleEntry{Enabled: &off2}
	if res, err = g.r.Apply(ctx, d, Options{}); err != nil || kinds(res.Actions) != "enable:stub" {
		t.Fatalf("%v %v", kinds(res.Actions), err)
	}
	if g.fk.Enabled["stub"].HMACSecret != got.HMACSecret {
		t.Fatal("signing secret rotated on re-enable")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	d := desired(nil, nil, mf("a", "1.0.0", nil), mf("b", "1.0.0", nil))
	if _, err := g.r.Apply(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	n := len(g.fk.Calls)
	res, err := g.r.Apply(ctx, d, Options{})
	if err != nil || kinds(res.Actions) != "" || len(g.fk.Calls) != n {
		t.Fatalf("second apply did work: %q calls=%v", kinds(res.Actions), g.fk.Calls[n:])
	}
}

func TestUpgradeOnVersionOrSettingsChange(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	d := desired(nil, nil, mf("a", "1.0.0", nil))
	_, _ = g.r.Apply(ctx, d, Options{})
	d.Manifests["a"] = mf("a", "1.1.0", nil)
	res, _ := g.r.Apply(ctx, d, Options{})
	if kinds(res.Actions) != "upgrade:a" {
		t.Fatalf("%s", kinds(res.Actions))
	}
	d.Company.Modules["a"] = config.ModuleEntry{Settings: map[string]any{"k": 1}}
	res, _ = g.r.Apply(ctx, d, Options{})
	if kinds(res.Actions) != "upgrade:a" {
		t.Fatalf("settings change: %s", kinds(res.Actions))
	}
	if rec, _ := g.db.GetModule(ctx, "a"); rec.Version != "1.1.0" {
		t.Fatalf("%+v", rec)
	}
}

func TestFailedUpgradeKeepsTheWorkingModuleRegistered(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	d := desired(nil, nil, mf("a", "1.0.0", nil, "tickets"))
	if _, err := g.r.Apply(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	d.Manifests["a"] = mf("a", "1.1.0", nil, "tickets")
	g.fk.FailEnable["a"] = errors.New("helm: upgrade failed, rolled back")
	if _, err := g.r.Apply(ctx, d, Options{}); err == nil {
		t.Fatal("expected the upgrade error")
	}
	rec, _ := g.db.GetModule(ctx, "a")
	if rec.State != store.StateEnabled || rec.Version != "1.0.0" || !strings.Contains(rec.LastError, "rolled back") {
		t.Fatalf("working module's record was clobbered: %+v", rec)
	}
	if _, err := g.reg.Resolve(ctx, "tickets"); err != nil {
		t.Fatalf("capability lost after a failed upgrade: %v", err)
	}
	// the next apply retries and succeeds
	delete(g.fk.FailEnable, "a")
	res, err := g.r.Apply(ctx, d, Options{})
	if err != nil || kinds(res.Actions) != "upgrade:a" {
		t.Fatalf("%q %v", kinds(res.Actions), err)
	}
	if rec, _ := g.db.GetModule(ctx, "a"); rec.Version != "1.1.0" || rec.LastError != "" {
		t.Fatalf("%+v", rec)
	}
}

func TestRemovedFromCompanyIsDisabledNotUninstalled(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	_, _ = g.r.Apply(ctx, desired(nil, nil, mf("a", "1", nil), mf("b", "1", nil)), Options{})
	res, err := g.r.Apply(ctx, desired(nil, nil, mf("a", "1", nil)), Options{})
	if err != nil || kinds(res.Actions) != "disable:b" {
		t.Fatalf("%q %v", kinds(res.Actions), err)
	}
	if rec, err := g.db.GetModule(ctx, "b"); err != nil || rec.State != store.StateDisabled {
		t.Fatalf("data-bearing record must be kept: %+v %v", rec, err)
	}
}

func TestDependencyOrder(t *testing.T) {
	g := newRig(t)
	d := desired(nil, nil,
		mf("app", "1", []string{"auth.oidc"}),
		mf("sso", "1", nil, "auth.oidc"),
		mf("zeta", "1", nil))
	res, err := g.r.Apply(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if kinds(res.Actions) != "install:sso,install:app,install:zeta" && kinds(res.Actions) != "install:sso,install:zeta,install:app" {
		t.Fatalf("provider must be installed before its consumer: %s", kinds(res.Actions))
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	res, err := g.r.Apply(ctx, desired(nil, nil, mf("a", "1", nil)), Options{DryRun: true})
	if err != nil || kinds(res.Actions) != "install:a" {
		t.Fatalf("%q %v", kinds(res.Actions), err)
	}
	if len(g.fk.Calls) != 0 {
		t.Fatalf("dry run called installer: %v", g.fk.Calls)
	}
	if ms, _ := g.db.ListModules(ctx); len(ms) != 0 {
		t.Fatalf("dry run wrote state: %+v", ms)
	}
	if recs, _ := audit.New(g.db).List(ctx, 0, 5); len(recs) != 0 {
		t.Fatal("dry run wrote audit entries")
	}
}

func TestFailureIsRecordedAndOthersContinue(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	g.fk.FailEnable["bad"] = errors.New("helm: timed out waiting for the condition")
	d := desired(nil, nil, mf("bad", "1", nil), mf("good", "1", nil))
	res, err := g.r.Apply(ctx, d, Options{})
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("want error naming the module, got %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0] != "bad" {
		t.Fatalf("%+v", res)
	}
	if rec, _ := g.db.GetModule(ctx, "good"); rec.State != store.StateEnabled {
		t.Fatalf("good module blocked by bad: %+v", rec)
	}
	rec, _ := g.db.GetModule(ctx, "bad")
	if rec.State != store.StateFailed || !strings.Contains(rec.LastError, "timed out") {
		t.Fatalf("%+v", rec)
	}
	if _, err := g.reg.Resolve(ctx, "anything"); err == nil {
		t.Fatal("failed module must not provide capabilities")
	}
	// a later apply retries the failed module
	delete(g.fk.FailEnable, "bad")
	res, err = g.r.Apply(ctx, d, Options{})
	if err != nil || kinds(res.Actions) != "install:bad" {
		t.Fatalf("%q %v", kinds(res.Actions), err)
	}
}

func TestPrimariesAreStored(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	d := desired(map[string]string{"contacts": "espo"}, nil, mf("crm", "1", nil, "contacts"), mf("espo", "1", nil, "contacts"))
	if _, err := g.r.Apply(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	res, err := g.reg.Resolve(ctx, "contacts")
	if err != nil || res.Providers[0].ModuleID != "espo" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestApplyWritesAuditTrailThatVerifies(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	_, _ = g.r.Apply(ctx, desired(nil, nil, mf("a", "1", nil)), Options{})
	off := false
	d := desired(nil, map[string]bool{"a": off}, mf("a", "1", nil))
	_, _ = g.r.Apply(ctx, d, Options{})
	recs, _ := audit.New(g.db).List(ctx, 0, 10)
	if len(recs) != 2 || recs[0].Action != "module.install" || recs[1].Action != "module.disable" || recs[0].Actor != "can-apply" {
		t.Fatalf("%+v", recs)
	}
	if res, _ := audit.New(g.db).Verify(ctx, audit.VerifyOptions{}); !res.OK() {
		t.Fatal("chain broken")
	}
}

func TestLoadDesiredValidatesAndLoadsManifests(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		full := filepath.Join(dir, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("company.yaml", "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nmodules:\n  example: {}\n")
	d, ps := LoadDesired(filepath.Join(dir, "company.yaml"), "../../../modules")
	if len(ps) != 0 || d.Manifests["example"] == nil || d.Company.Name != "A" {
		t.Fatalf("%v %+v", ps, d)
	}
	write("bad/company.yaml", "schema: 1\nname: A\nprovider: {type: local}\nmodules:\n  ghost: {}\n")
	if _, ps := LoadDesired(filepath.Join(dir, "bad", "company.yaml"), "../../../modules"); len(ps) == 0 {
		t.Fatal("invalid company accepted")
	}
}

func TestRecordOnlyInstaller(t *testing.T) {
	g := newRig(t)
	g.r.Installer = RecordOnly{}
	if _, err := g.r.Apply(context.Background(), desired(nil, nil, mf("a", "1", nil)), Options{}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := g.db.GetModule(context.Background(), "a"); rec.State != store.StateEnabled {
		t.Fatalf("%+v", rec)
	}
}

func TestMaliciousServiceNameIsRefusedBeforeInstall(t *testing.T) {
	g := newRig(t)
	m := mf("evil", "1", nil, "contacts")
	m.Provides[0].Service = "evil.com/"
	if _, err := g.r.Apply(context.Background(), desired(nil, nil, m), Options{}); err == nil {
		t.Fatal("accepted")
	}
	if len(g.fk.Calls) != 0 {
		t.Fatalf("installer called: %v", g.fk.Calls)
	}
}
