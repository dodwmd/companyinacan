package rbac

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type env struct {
	t   *testing.T
	db  *store.DB
	s   *Service
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{t: t, db: db, now: time.Unix(1_700_000_000, 0)}
	db.Now = func() time.Time { return e.now }
	e.s = New(db, audit.New(db), Options{Now: func() time.Time { return e.now }})
	return e
}

func (e *env) module(id string, perms []string, roles map[string][]string, state string) {
	e.t.Helper()
	m := store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: state, HMACSecret: "x",
		Manifest: config.Module{ID: id, Permissions: perms, Roles: roles}}
	if err := e.db.UpsertModule(context.Background(), m, nil, nil); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) check(subject, perm, res string) bool {
	e.t.Helper()
	ok, err := e.s.Check(context.Background(), subject, perm, res)
	if err != nil {
		e.t.Fatal(err)
	}
	return ok
}

func TestLeastPrivilegeByDefault(t *testing.T) {
	e := newEnv(t)
	e.module("expenses", []string{"expenses.read", "expenses.approve"}, map[string][]string{"finance": {"expenses.read", "expenses.approve"}}, store.StateEnabled)
	if e.check("user:nobody", "expenses.read", "") {
		t.Fatal("a subject with no role must have no permission")
	}
	if e.check("", "expenses.read", "") {
		t.Fatal("empty subject must be denied")
	}
}

func TestManifestRolesGrantPermissions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.module("expenses", []string{"expenses.read", "expenses.approve"}, map[string][]string{"finance": {"expenses.read", "expenses.approve"}}, store.StateEnabled)
	if err := e.s.Grant(ctx, "root", "user:fin", "finance", ""); err != nil {
		t.Fatal(err)
	}
	if !e.check("user:fin", "expenses.approve", "") {
		t.Fatal("finance should approve expenses")
	}
	if e.check("user:fin", "expenses.admin", "") {
		t.Fatal("permission not in the role must be denied")
	}
}

func TestDisabledModuleRolesGiveNothing(t *testing.T) {
	e := newEnv(t)
	e.module("expenses", []string{"expenses.read"}, map[string][]string{"finance": {"expenses.read"}}, store.StateDisabled)
	if err := e.s.Grant(context.Background(), "root", "user:fin", "finance", ""); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("grant of a role only a disabled module defines: err = %v, want ErrUnknownRole", err)
	}
}

func TestOwnerBreakGlassHasEverything(t *testing.T) {
	e := newEnv(t)
	if err := e.s.Grant(context.Background(), "cli", "user:root", Owner, ""); err != nil {
		t.Fatal(err)
	}
	if !e.check("user:root", "anything.at-all", "any/resource") {
		t.Fatal("owner must be allowed everything")
	}
}

func TestScopedAssignment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.module("expenses", []string{"expenses.read"}, map[string][]string{"viewer": {"expenses.read"}}, store.StateEnabled)
	if err := e.s.Grant(ctx, "root", "user:a", "viewer", "dept/sales/*"); err != nil {
		t.Fatal(err)
	}
	if !e.check("user:a", "expenses.read", "dept/sales/42") {
		t.Fatal("resource under the scope must be allowed")
	}
	if e.check("user:a", "expenses.read", "dept/hr/1") {
		t.Fatal("resource outside the scope must be denied")
	}
	if e.check("user:a", "expenses.read", "") {
		t.Fatal("a scoped grant must not answer for an unspecified resource (fail closed)")
	}
	if e.check("user:a", "expenses.read", "dept/sales") {
		t.Fatal("prefix scope dept/sales/* must not match dept/sales itself")
	}
}

func TestOverridesRevokeAndGrant(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.module("expenses", []string{"expenses.read", "expenses.approve"}, map[string][]string{"finance": {"expenses.read", "expenses.approve"}}, store.StateEnabled)
	_ = e.s.Grant(ctx, "root", "user:fin", "finance", "")
	if err := e.s.Override(ctx, "root", "finance", "expenses.approve", EffectRevoke); err != nil {
		t.Fatal(err)
	}
	if e.check("user:fin", "expenses.approve", "") {
		t.Fatal("revoked default grant must no longer apply (cache must be invalidated)")
	}
	if err := e.s.Override(ctx, "root", "finance", "expenses.read", EffectReset); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Override(ctx, "root", "finance", "expenses.approve", EffectReset); err != nil {
		t.Fatal(err)
	}
	if !e.check("user:fin", "expenses.approve", "") {
		t.Fatal("reset restores the manifest default")
	}
	if err := e.s.Override(ctx, "root", Owner, "x.y", EffectRevoke); err == nil {
		t.Fatal("the owner role must not be editable")
	}
}

func TestCacheExpiresAndInvalidates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.module("expenses", []string{"expenses.read"}, map[string][]string{"viewer": {"expenses.read"}}, store.StateEnabled)
	_ = e.s.Grant(ctx, "root", "user:a", "viewer", "")
	if !e.check("user:a", "expenses.read", "") {
		t.Fatal("setup")
	}
	// A change made behind the service's back (e.g. a module re-registered with other roles)
	// becomes visible after the TTL, not before.
	e.module("expenses", []string{"expenses.read"}, map[string][]string{"viewer": {}}, store.StateEnabled)
	if !e.check("user:a", "expenses.read", "") {
		t.Fatal("cached answer expected within the TTL")
	}
	e.now = e.now.Add(DefaultTTL + time.Second)
	if e.check("user:a", "expenses.read", "") {
		t.Fatal("after the TTL the new manifest roles apply")
	}
	// Changes made through the service take effect immediately.
	e.module("expenses", []string{"expenses.read"}, map[string][]string{"viewer": {"expenses.read"}}, store.StateEnabled)
	e.now = e.now.Add(DefaultTTL + time.Second)
	if !e.check("user:a", "expenses.read", "") {
		t.Fatal("setup 2")
	}
	if err := e.s.Revoke(ctx, "root", "user:a", "viewer", ""); err != nil {
		t.Fatal(err)
	}
	if e.check("user:a", "expenses.read", "") {
		t.Fatal("revoke must invalidate the cache at once")
	}
}

func TestLastOwnerCannotBeRevoked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_ = e.s.Grant(ctx, "cli", "user:root", Owner, "")
	if err := e.s.Revoke(ctx, "user:root", "user:root", Owner, ""); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("err = %v, want ErrLastOwner", err)
	}
	_ = e.s.Grant(ctx, "user:root", "user:two", Owner, "")
	if err := e.s.Revoke(ctx, "user:root", "user:root", Owner, ""); err != nil {
		t.Fatalf("with a second owner the revoke must work: %v", err)
	}
}

func TestGrantAndRevokeAreAudited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.module("expenses", []string{"expenses.read"}, map[string][]string{"viewer": {"expenses.read"}}, store.StateEnabled)
	_ = e.s.Grant(ctx, "root", "user:a", "viewer", "")
	_ = e.s.Revoke(ctx, "root", "user:a", "viewer", "")
	recs, _ := audit.New(e.db).List(ctx, 0, 50)
	var actions []string
	for _, r := range recs {
		actions = append(actions, r.Action)
	}
	want := map[string]bool{"rbac.grant": false, "rbac.revoke": false}
	for _, a := range actions {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for a, seen := range want {
		if !seen {
			t.Errorf("no %s audit entry in %v", a, actions)
		}
	}
}

func TestInputValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, c := range []struct{ subject, role, scope string }{
		{"", "owner", ""}, {"alice", "owner", ""}, {"user:a b", "owner", ""}, {"user:a", "", ""}, {"user:a", "owner", "bad scope with space"},
	} {
		if err := e.s.Grant(ctx, "root", c.subject, c.role, c.scope); err == nil {
			t.Errorf("Grant(%q,%q,%q) accepted", c.subject, c.role, c.scope)
		}
	}
}

func TestFailClosedOnDatabaseError(t *testing.T) {
	e := newEnv(t)
	_ = e.db.Close()
	ok, err := e.s.Check(context.Background(), "user:a", "x.y", "")
	if ok || err == nil {
		t.Fatalf("Check = %v, %v; want denied with an error", ok, err)
	}
}

func TestSubjectsWithRoleIsBounded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		_ = e.s.Grant(ctx, "root", "user:u"+string(rune('a'+i%26))+string(rune('a'+i/26)), Owner, "")
	}
	got, err := e.s.SubjectsWithRole(ctx, Owner, 10)
	if err != nil || len(got) != 10 {
		t.Fatalf("got %d, %v; want exactly 10", len(got), err)
	}
}

func TestMigrationMakesExistingAdminOwner(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.db.CreateAdmin(ctx, "first", "h", "s"); err != nil {
		t.Fatal(err)
	}
	if !e.check("user:first", "whatever.perm", "") {
		t.Fatal("the first admin created must be an owner")
	}
	if _, err := e.db.CreateAdmin(ctx, "second", "h", "s"); err != nil {
		t.Fatal(err)
	}
	if e.check("user:second", "whatever.perm", "") {
		t.Fatal("later admins start with no role (least privilege)")
	}
}
