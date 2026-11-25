package runtime

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func rig(t *testing.T) (*reconcile.Reconciler, *reconcile.FakeInstaller) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fk := reconcile.NewFakeInstaller()
	return reconcile.New(db, registry.New(db, ""), audit.New(db), fk), fk
}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func writeCompany(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const company = "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nmodules:\n  example: {}\n"

func TestReconcileOnceAppliesTheMountedCompanyFile(t *testing.T) {
	r, fk := rig(t)
	cfg := Config{CompanyFile: writeCompany(t, company), ModulesDir: "../../../modules"}
	if err := reconcileOnce(context.Background(), cfg, r, quiet()); err != nil {
		t.Fatal(err)
	}
	if len(fk.Enabled) != 1 || fk.Enabled["example"].ID != "example" {
		t.Fatalf("%v", fk.Calls)
	}
	n := len(fk.Calls)
	if err := reconcileOnce(context.Background(), cfg, r, quiet()); err != nil || len(fk.Calls) != n {
		t.Fatalf("second pass must be a no-op: %v %v", err, fk.Calls)
	}
}

func TestReconcileOnceSkipsMissingAndRefusesInvalidCompany(t *testing.T) {
	r, fk := rig(t)
	if err := reconcileOnce(context.Background(), Config{CompanyFile: filepath.Join(t.TempDir(), "nope.yaml"), ModulesDir: "../../../modules"}, r, quiet()); err != nil {
		t.Fatalf("a company.yaml that is not there yet is not an error: %v", err)
	}
	cfg := Config{CompanyFile: writeCompany(t, "schema: 1\nname: A\nprovider: {type: local}\nmodules:\n  ghost: {}\n"), ModulesDir: "../../../modules"}
	if err := reconcileOnce(context.Background(), cfg, r, quiet()); err == nil {
		t.Fatal("invalid company.yaml applied")
	}
	if len(fk.Calls) != 0 {
		t.Fatalf("installer called: %v", fk.Calls)
	}
}

func TestReconcileLoopRunsUntilCancelled(t *testing.T) {
	r, fk := rig(t)
	cfg := Config{CompanyFile: writeCompany(t, company), ModulesDir: "../../../modules", ReconcileInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { reconcileLoop(ctx, cfg, r, quiet()); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fk.EnabledCount() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if fk.EnabledCount() != 1 {
		t.Fatal("loop never applied")
	}
}

func TestInstallerEnvConfig(t *testing.T) {
	env := map[string]string{"CAN_INSTALLER": "kubernetes", "CAN_COMPANY_FILE": "/c.yaml", "CAN_MODULES_DIR": "/m", "CAN_NODE_IP": "10.0.0.1", "CAN_RECONCILE_INTERVAL_SECONDS": "7", "CAN_POSTGRES_ADMIN_URL": "postgres://x"}
	c, err := FromEnv(func(k string) string { return env[k] })
	if err != nil || c.Installer != "kubernetes" || c.CompanyFile != "/c.yaml" || c.ModulesDir != "/m" || c.NodeIP != "10.0.0.1" || c.ReconcileInterval != 7*time.Second || c.PostgresAdminURL != "postgres://x" {
		t.Fatalf("%+v %v", c, err)
	}
	c, _ = FromEnv(func(string) string { return "" })
	if c.Installer != "none" || c.ModulesDir != "/modules" || c.ReconcileInterval != 30*time.Second {
		t.Fatalf("defaults: %+v", c)
	}
	if _, err := FromEnv(func(k string) string { return map[string]string{"CAN_INSTALLER": "helm"}[k] }); err == nil {
		t.Fatal("unknown installer accepted")
	}
}

// The admin UI writes company.yaml to the store; the periodic loop must read the store, not a
// mounted file that the kubelet refreshes up to a minute later, or it would revert the UI's change.
func TestReconcileStoredAppliesTheStoreAndIgnoresAStaleMountedFile(t *testing.T) {
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fk := reconcile.NewFakeInstaller()
	al := audit.New(db)
	rec := reconcile.New(db, registry.New(db, ""), al, fk)
	run := modmgr.NewRunner(rec)
	svc := &modmgr.Service{DB: db, Audit: al, Runner: run}
	stale := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\n") // no modules: a stale mount
	cs := &modmgr.FileStore{Path: writeCompany(t, company)}
	cfg := Config{CompanyFile: stale, ModulesDir: "../../../modules"}
	if err := reconcileStored(context.Background(), cfg, cs, run, svc, quiet()); err != nil {
		t.Fatal(err)
	}
	if fk.EnabledCount() != 1 {
		t.Fatalf("the store was not applied: %v", fk.Calls)
	}
	h, err := svc.Status(context.Background())
	if err != nil || h.Reconcile == nil || !h.Reconcile.OK {
		t.Fatalf("the result was not recorded: %+v %v", h.Reconcile, err)
	}
	// no document yet: not an error; an invalid one: refused whole
	if err := reconcileStored(context.Background(), cfg, &modmgr.FileStore{Path: filepath.Join(t.TempDir(), "none.yaml")}, run, svc, quiet()); err != nil {
		t.Fatalf("%v", err)
	}
	bad := &modmgr.FileStore{Path: writeCompany(t, "schema: 1\nname: A\nprovider: {type: local}\nmodules:\n  ghost: {}\n")}
	if err := reconcileStored(context.Background(), cfg, bad, run, svc, quiet()); err == nil {
		t.Fatal("invalid company.yaml applied")
	}
	if fk.EnabledCount() != 1 {
		t.Fatal("an invalid document disabled modules")
	}
}

func TestPublicIPsEnv(t *testing.T) {
	c, err := FromEnv(func(k string) string {
		if k == "CAN_PUBLIC_IPS" {
			return "203.0.113.7, 2001:db8::7"
		}
		return ""
	})
	if err != nil || len(c.PublicIPs) != 2 || c.PublicIPs[1] != "2001:db8::7" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := FromEnv(func(k string) string {
		if k == "CAN_PUBLIC_IPS" {
			return "not-an-ip"
		}
		return ""
	}); err == nil {
		t.Fatal("a bad address was accepted")
	}
}
