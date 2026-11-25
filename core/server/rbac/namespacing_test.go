package rbac

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// QA round 2: module a defines role viewer; hostile module evil declares permission work.admin and
// its own role viewer granting it. Holders of a's viewer must not gain work.admin, even when the
// registry was bypassed and both modules are enabled.
func TestSameNamedRolesOfTwoModulesStayApart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.module("work", []string{"work.read", "work.admin"}, map[string][]string{"viewer": {"work.read"}}, store.StateEnabled)
	e.module("evil", []string{"work.admin"}, map[string][]string{"viewer": {"work.admin"}}, store.StateEnabled)
	_ = e.s.Grant(ctx, CLIActor, "user:v", "work:viewer", "")
	if !e.check("user:v", "work.read", "") {
		t.Fatal("work:viewer keeps its own grant")
	}
	if e.check("user:v", "work.admin", "") {
		t.Fatal("holders of work:viewer gained work.admin from another module's role of the same name")
	}
	// work.admin is declared by two enabled modules: it belongs to neither, so no module role grants it.
	_ = e.s.Grant(ctx, CLIActor, "user:e", "evil:viewer", "")
	if e.check("user:e", "work.admin", "") {
		t.Fatal("a contested permission must grant nothing")
	}
	if err := e.s.Grant(ctx, CLIActor, "user:v", "viewer", ""); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("the bare name is not a role any more: %v", err)
	}
}

func TestQualifyRole(t *testing.T) {
	for _, c := range [][3]string{{"x", "viewer", "x:viewer"}, {"x", "work:viewer", "work:viewer"}, {"x", "owner", "owner"}, {"x", "operator", "operator"}, {"x", "auditor", "auditor"}} {
		if got := QualifyRole(c[0], c[1]); got != c[2] {
			t.Errorf("QualifyRole(%q,%q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestCLIActorPrivilegeIsNotAUsernameAndNeedsBreakGlass(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// An admin who is literally named "cli" gets nothing special.
	if err := e.s.Grant(ctx, "cli", "user:cli", Owner, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Grant(cli -> user:cli, owner) = %v, want ErrForbidden", err)
	}
	if err := e.s.Grant(ctx, "cli", "user:other", Owner, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner named cli granting owner: %v", err)
	}
	// The sentinel is not a possible username, and only a break-glass Service honours it.
	if ValidSubject("user:" + CLIActor) {
		t.Fatalf("%q must not be a valid username", CLIActor)
	}
	web := New(e.db, audit.New(e.db), Options{Now: func() time.Time { return e.now }})
	if err := web.Grant(ctx, CLIActor, "user:x", Owner, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a web-side Service must not honour the CLI sentinel: %v", err)
	}
}
