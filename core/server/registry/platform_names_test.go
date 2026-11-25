package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func newReg(t *testing.T) *Registry {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, "http://can-core")
}

func mod(id string, m config.Module) store.Module {
	m.ID = id
	return store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, HMACSecret: "s", Manifest: m}
}

func TestRegisterRefusesHostileManifests(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	for name, m := range map[string]config.Module{
		"platform permission":  {Permissions: []string{"rbac.manage"}, Roles: map[string][]string{"helper": {"rbac.manage"}}},
		"built-in role":        {Permissions: []string{"evil.read"}, Roles: map[string][]string{"auditor": {"evil.read"}}},
		"owner role":           {Roles: map[string][]string{"owner": {}}},
		"forged event":         {},
		"foreign notification": {Notifications: []config.NotifyRule{{Event: "lead.created.v1", Type: "approval.requested", Recipients: []string{"role:x"}}}},
	} {
		if name == "forged event" {
			m.Publishes = append(m.Publishes, struct {
				Event  string `yaml:"event"`
				Schema string `yaml:"schema"`
			}{Event: "approval.decided.v1"})
		}
		if err := r.Register(ctx, mod("evil", m)); !errors.Is(err, ErrReservedName) {
			t.Errorf("%s: err = %v, want ErrReservedName", name, err)
		}
	}
	if _, err := r.db.GetModule(ctx, "evil"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a refused manifest must not be recorded")
	}
	ok := config.Module{Permissions: []string{"evil.read"}, Roles: map[string][]string{"reader": {"evil.read"}}}
	if err := r.Register(ctx, mod("fine", ok)); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalTypesAreUniqueAcrossEnabledModules(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	m := config.Module{Approvals: []config.ManifestApproval{{Type: "expense"}}}
	if err := r.Register(ctx, mod("expenses", m)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ctx, mod("expenses", m)); err != nil {
		t.Fatalf("re-registering the same module: %v", err)
	}
	if err := r.Register(ctx, mod("vendors", m)); !errors.Is(err, ErrApprovalTypeTaken) {
		t.Fatalf("a second module declaring the type: %v", err)
	}
	if err := r.Register(ctx, mod("vendors", config.Module{Approvals: []config.ManifestApproval{{Type: "po"}}})); err != nil {
		t.Fatal(err)
	}
}
