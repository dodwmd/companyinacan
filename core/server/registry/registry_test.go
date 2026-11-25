package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func manifest(id string, provides ...string) *config.Module {
	m := &config.Module{ID: id, Version: "1.0.0"}
	for i, c := range provides {
		m.Provides = append(m.Provides, struct {
			Capability string `yaml:"capability"`
			Schema     string `yaml:"schema"`
			Service    string `yaml:"service"`
			Port       int    `yaml:"port"`
			Status     string `yaml:"status"`
		}{Capability: c, Service: id, Port: 8080 + i})
	}
	return m
}

func setup(t *testing.T) (*Registry, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, "http://can-core.can-system.svc:8081"), db
}

func install(t *testing.T, r *Registry, db *store.DB, m *config.Module, state string) {
	t.Helper()
	rec := store.Module{ID: m.ID, Version: m.Version, Namespace: "mod-" + m.ID, ServiceAccount: m.ID, State: state, HMACSecret: "s", Manifest: *m}
	if err := r.Register(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSingleExclusiveProvider(t *testing.T) {
	r, db := setup(t)
	install(t, r, db, manifest("crm", "contacts"), store.StateEnabled)
	res, err := r.Resolve(context.Background(), "contacts")
	if err != nil {
		t.Fatal(err)
	}
	if res.Aggregate || len(res.Providers) != 1 || res.Providers[0].URL != "http://crm.mod-crm.svc.cluster.local:8080" {
		t.Fatalf("%+v", res)
	}
}

func TestExclusiveConflictNeedsPrimary(t *testing.T) {
	ctx := context.Background()
	r, db := setup(t)
	install(t, r, db, manifest("crm", "contacts"), store.StateEnabled)
	install(t, r, db, manifest("espo", "contacts"), store.StateEnabled)
	if _, err := r.Resolve(ctx, "contacts"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("want ambiguous, got %v", err)
	}
	if err := r.SetPrimaries(ctx, map[string]string{"contacts": "espo"}); err != nil {
		t.Fatal(err)
	}
	res, err := r.Resolve(ctx, "contacts")
	if err != nil || len(res.Providers) != 1 || res.Providers[0].ModuleID != "espo" {
		t.Fatalf("%+v %v", res, err)
	}
	// primary that is no longer enabled: with one other provider left, ambiguity is not an issue
	if err := db.SetModuleState(ctx, "espo", store.StateDisabled, ""); err != nil {
		t.Fatal(err)
	}
	res, err = r.Resolve(ctx, "contacts")
	if err != nil || res.Providers[0].ModuleID != "crm" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestAggregateReturnsAllProviders(t *testing.T) {
	r, db := setup(t)
	install(t, r, db, manifest("a", "reports"), store.StateEnabled)
	install(t, r, db, manifest("b", "reports"), store.StateEnabled)
	res, err := r.Resolve(context.Background(), "reports")
	if err != nil || !res.Aggregate || len(res.Providers) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestNoProviderAndDisabledProvider(t *testing.T) {
	r, db := setup(t)
	if _, err := r.Resolve(context.Background(), "contacts"); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("%v", err)
	}
	install(t, r, db, manifest("crm", "contacts"), store.StateDisabled)
	if _, err := r.Resolve(context.Background(), "contacts"); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("disabled module resolved: %v", err)
	}
}

func TestPlatformCapabilitiesAreProvidedByCore(t *testing.T) {
	r, _ := setup(t)
	res, err := r.Resolve(context.Background(), "email.send")
	if err != nil || !res.Platform || res.Providers[0].ModuleID != "can-core" || res.Providers[0].URL != "http://can-core.can-system.svc:8081" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestReRegisterReplacesProvisions(t *testing.T) {
	ctx := context.Background()
	r, db := setup(t)
	install(t, r, db, manifest("crm", "contacts", "tickets"), store.StateEnabled)
	install(t, r, db, manifest("crm", "contacts"), store.StateEnabled)
	if _, err := r.Resolve(ctx, "tickets"); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("stale provision survived upgrade: %v", err)
	}
}

func TestFromManifestDefaults(t *testing.T) {
	m := manifest("crm", "contacts")
	m.Subscribes = append(m.Subscribes, struct {
		Event   string `yaml:"event"`
		Service string `yaml:"service"`
		Port    int    `yaml:"port"`
		Webhook string `yaml:"webhook"`
		Status  string `yaml:"status"`
	}{Event: "lead.created.v1", Webhook: "/hooks/lead"})
	_, subs, err := FromManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Service != "crm" || subs[0].Port != 8080 || subs[0].Webhook != "/hooks/lead" {
		t.Fatalf("%+v", subs)
	}
}

func TestDeclares(t *testing.T) {
	m := &config.Module{}
	m.Requires.Capabilities = []string{"email.send"}
	m.Consumes = []string{"contacts"}
	if !Declares(m, "email.send") || !Declares(m, "contacts") || Declares(m, "files") {
		t.Fatal("Declares wrong")
	}
}

func TestServiceURLRejectsSSRFVectors(t *testing.T) {
	for _, svc := range []string{"169.254.169.254#", "evil.com/", "x@evil.com", "a.b", "a b", "-a", "A", "", "evil.com:80#", "a?b", strings.Repeat("a", 64)} {
		if u, err := ServiceURL("mod-x", svc, 80); err == nil {
			t.Errorf("service %q accepted: %s", svc, u)
		}
	}
	for _, ns := range []string{"x.evil.com#", "", "a/b"} {
		if _, err := ServiceURL(ns, "ok", 80); err == nil {
			t.Errorf("namespace %q accepted", ns)
		}
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := ServiceURL("mod-x", "ok", port); err == nil {
			t.Errorf("port %d accepted", port)
		}
	}
	if u, err := ServiceURL("mod-crm", "adapter", 8081); err != nil || u != "http://adapter.mod-crm.svc.cluster.local:8081" {
		t.Fatalf("%s %v", u, err)
	}
}

func TestRegisterRejectsMaliciousServiceInManifest(t *testing.T) {
	r, db := setup(t)
	m := manifest("evil", "contacts")
	m.Provides[0].Service = "169.254.169.254#"
	rec := store.Module{ID: "evil", Version: "1", Namespace: "mod-evil", ServiceAccount: "evil", State: store.StateEnabled, Manifest: *m}
	if err := r.Register(context.Background(), rec); err == nil {
		t.Fatal("manifest with an SSRF service accepted")
	}
	if ms, _ := db.ListModules(context.Background()); len(ms) != 0 {
		t.Fatal("rejected module was still recorded")
	}
}

// A provision or subscription marked planned (declared for the contract, adapter not deployed)
// must not become a provider or a delivery target.
func TestFromManifestSkipsPlanned(t *testing.T) {
	m := manifest("support", "tickets")
	m.Provides[0].Status = "planned"
	m.Subscribes = append(m.Subscribes, struct {
		Event   string `yaml:"event"`
		Service string `yaml:"service"`
		Port    int    `yaml:"port"`
		Webhook string `yaml:"webhook"`
		Status  string `yaml:"status"`
	}{Event: "lead.created.v1", Webhook: "/hooks/lead", Status: "planned"})
	ps, subs, err := FromManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 || len(subs) != 0 {
		t.Fatalf("planned entries were registered: %+v %+v", ps, subs)
	}
}
