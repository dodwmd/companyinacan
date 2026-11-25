package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func workMod() config.Module {
	return config.Module{Permissions: []string{"work.read", "work.admin"}, Roles: map[string][]string{"viewer": {"work.read"}, "boss": {"work.admin"}}}
}

// The QA scenario: module a owns role viewer; a hostile module declares permission work.admin and
// a role viewer granting it, hoping every viewer inherits it.
func TestHostileModuleCannotShareAnotherModulesRoleOrPermission(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	if err := r.Register(ctx, mod("work", workMod())); err != nil {
		t.Fatal(err)
	}
	evil := config.Module{Permissions: []string{"work.admin"}, Roles: map[string][]string{"viewer": {"work.admin"}}}
	if err := r.Register(ctx, mod("evil", evil)); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("err = %v, want ErrNameTaken", err)
	}
	onlyPerm := config.Module{Permissions: []string{"work.admin"}}
	if err := r.Register(ctx, mod("evil", onlyPerm)); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a permission owned by another module: %v", err)
	}
	if _, err := r.db.GetModule(ctx, "evil"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("refused modules are not recorded")
	}
	// Same-named roles are fine: they are different roles ("work:viewer", "evil:viewer").
	own := config.Module{Permissions: []string{"evil.read"}, Roles: map[string][]string{"viewer": {"evil.read"}}}
	if err := r.Register(ctx, mod("evil", own)); err != nil {
		t.Fatalf("a module's own role named like another module's: %v", err)
	}
}

func TestOwnershipDoesNotDependOnRegistrationOrder(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	evil := config.Module{Permissions: []string{"work.admin"}, Roles: map[string][]string{"viewer": {"work.admin"}}}
	if err := r.Register(ctx, mod("evil", evil)); err != nil {
		t.Fatal(err) // first registrant owns the names
	}
	if err := r.Register(ctx, mod("work", workMod())); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("the later registrant is refused whoever it is: %v", err)
	}
}

// Disabling the owner frees its names; re-enabling it is then refused while the other module holds
// them. Ownership is never inherited silently, and never taken back silently either.
func TestDisablingTheOwnerFreesNamesButReenablingIsRefusedWhileTaken(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	if err := r.Register(ctx, mod("work", workMod())); err != nil {
		t.Fatal(err)
	}
	if err := r.db.SetModuleState(ctx, "work", store.StateDisabled, ""); err != nil {
		t.Fatal(err)
	}
	evil := config.Module{Permissions: []string{"work.admin"}, Roles: map[string][]string{"viewer": {"work.admin"}}}
	if err := r.Register(ctx, mod("evil", evil)); err != nil {
		t.Fatalf("names of a disabled module are free: %v", err)
	}
	if err := r.Register(ctx, mod("work", workMod())); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("re-enabling the original owner while evil holds its names: %v", err)
	}
	if err := r.PreCheck(ctx, mod("work", workMod())); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("PreCheck must agree with Register: %v", err)
	}
	// A module registered as disabled or failed claims nothing.
	dis := mod("dis", config.Module{Permissions: []string{"work.admin"}})
	dis.State = store.StateDisabled
	if err := r.Register(ctx, dis); err != nil {
		t.Fatal(err)
	}
}

// Every manifest shipped in modules/ must register together, or the checks would break installs.
func TestShippedManifestsRegisterTogether(t *testing.T) {
	files, _ := filepath.Glob("../../../modules/*/module.yaml")
	if len(files) < 5 {
		t.Fatalf("found %d manifests", len(files))
	}
	r := newReg(t)
	ctx := context.Background()
	for _, f := range files {
		doc, ps := config.Load(f)
		if doc == nil {
			t.Fatalf("%s: %v", f, ps)
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
		if err := r.Register(ctx, store.Module{ID: m.ID, Version: m.Version, Namespace: "mod-" + m.ID, ServiceAccount: m.ID, State: store.StateEnabled, HMACSecret: "s", Manifest: *m}); err != nil {
			t.Errorf("%s: %v", m.ID, err)
		}
	}
}
