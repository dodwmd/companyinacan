package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/rbac"
)

// owners 20 (user:root) and 21 (user:deputy), an ordinary administrator 22 (user:ops).
func (e *env) seedOwners() {
	e.t.Helper()
	for _, q := range []string{
		`INSERT INTO admin_users(id,username,password_hash,totp_secret,created_at,auth_source) VALUES (20,'root','x','y',1,'local'),(21,'deputy','x','y',1,'local'),(22,'ops','x','y',1,'local')`,
		`INSERT INTO rbac_assignments(subject,role,scope,granted_by,granted_at) VALUES ('user:root','owner','','t',1),('user:deputy','owner','','t',1),('user:ops','platform-admin','','t',1)`,
	} {
		if _, err := e.db.ExecContext(context.Background(), q); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) revokeNowAudits() int {
	var n int
	_ = e.db.QueryRow(`SELECT count(*) FROM audit_log WHERE action='identity.revoke_now'`).Scan(&n)
	return n
}

func TestRevokeNowOwnerRules(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedOwners()
	// a non-owner with rbac.manage cannot disable an owner
	if _, err := e.svc.RevokeNow(ctx, "user:ops", "user:root"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner disabling an owner: %v", err)
	}
	if e.db.AdminDisabled(ctx, 20) || e.revokeNowAudits() != 0 {
		t.Fatal("a refused call changed something")
	}
	// an owner can disable another owner
	if _, err := e.svc.RevokeNow(ctx, "user:root", "user:deputy"); err != nil {
		t.Fatal(err)
	}
	if !e.db.AdminDisabled(ctx, 21) {
		t.Fatal("deputy not disabled")
	}
	// but never the last enabled owner, not even by themself or by the break-glass actor
	for _, actor := range []string{"user:root", rbac.CLIActor} {
		if _, err := e.svc.RevokeNow(ctx, actor, "user:root"); !errors.Is(err, ErrLastOwner) {
			t.Fatalf("%s disabling the last owner: %v", actor, err)
		}
	}
	if e.db.AdminDisabled(ctx, 20) {
		t.Fatal("the last owner was disabled")
	}
	// a non-owner can still be disabled by anyone with rbac.manage; an unknown account is refused and not audited
	before := e.revokeNowAudits()
	if _, err := e.svc.RevokeNow(ctx, "user:ops", "user:ghost"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown subject: %v", err)
	}
	if e.revokeNowAudits() != before {
		t.Fatal("a no-op was audited")
	}
	if _, err := e.svc.RevokeNow(ctx, "user:root", "user:ops"); err != nil {
		t.Fatal(err)
	}
}

func TestModuleRevocationAlsoKeepsTheLastOwner(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedOwners()
	e.svc.PerMinute = 100
	const ownerSubj = "user:root"
	e.offboard("people", party, "2026-06-01", ownerSubj)
	// root is the only enabled owner once deputy is disabled
	if _, err := e.svc.RevokeNow(ctx, "user:root", "user:deputy"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Revoke(ctx, provisioner(), Request{PartyID: party, Subject: ownerSubj}); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("a module disabled the last owner: %v", err)
	}
}

func TestEnableIsOwnerOnlyAndAudited(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedOwners()
	if _, err := e.svc.RevokeNow(ctx, "user:root", "user:ops"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Enable(ctx, "user:ops", "user:ops"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a disabled non-owner enabling themself: %v", err)
	}
	if err := e.svc.Enable(ctx, "user:deputy", "user:ghost"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown: %v", err)
	}
	if err := e.svc.Enable(ctx, "user:deputy", "user:ops"); err != nil {
		t.Fatal(err)
	}
	if e.db.AdminDisabled(ctx, 22) {
		t.Fatal("not enabled")
	}
	// the break-glass CLI actor can re-enable
	if _, err := e.svc.RevokeNow(ctx, "user:root", "user:ops"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Enable(ctx, rbac.CLIActor, "user:ops"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = e.db.QueryRow(`SELECT count(*) FROM audit_log WHERE action='identity.enable'`).Scan(&n)
	if n != 2 {
		t.Fatalf("enable audits %d", n)
	}
	if err := e.svc.Enable(ctx, "user:root", "admin"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad subject: %v", err)
	}
}
