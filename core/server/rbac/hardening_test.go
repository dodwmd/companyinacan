package rbac

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestHostileManifestCannotEscalate(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Inserted straight into the store, as if validate and the registry had been bypassed.
	e.module("evil", []string{"rbac.manage", "platform.view", "evil.read"},
		map[string][]string{"auditor": {"rbac.manage"}, "owner": {"evil.read"}, "operator": {"rbac.manage"}, "helper": {"rbac.manage", "evil.read", "undeclared.perm"}}, store.StateEnabled)
	for _, r := range []string{"auditor", "operator", "helper"} {
		if err := e.s.Grant(ctx, CLIActor, "user:"+r, QualifyRole("evil", r), ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []string{"auditor", "operator", "helper"} {
		if e.check("user:"+r, PermManage, "") {
			t.Errorf("role %s obtained rbac.manage from a module manifest", r)
		}
	}
	if !e.check("user:auditor", PermAudit, "") || e.check("user:auditor", "evil.read", "") {
		t.Fatal("auditor must stay exactly the built-in role")
	}
	if !e.check("user:helper", "evil.read", "") || e.check("user:helper", "undeclared.perm", "") {
		t.Fatal("a module role grants its own declared permissions only")
	}
	if e.check("user:helper", "platform.view", "") {
		t.Fatal("helper has no platform permission")
	}
	rs, _ := e.s.Roles(ctx)
	for _, r := range rs {
		if r.Name == Owner && (len(r.Permissions) != 1 || r.Permissions[0] != Wildcard) {
			t.Fatalf("owner role changed: %v", r.Permissions)
		}
	}
}

func TestOnlyOwnersChangeOwnersAndNobodySelfGrants(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.module("expenses", []string{"expenses.read"}, map[string][]string{"viewer": {"expenses.read"}}, store.StateEnabled)
	_ = e.s.Grant(ctx, CLIActor, "user:boss", Owner, "")
	_ = e.s.Grant(ctx, CLIActor, "user:mgr", "operator", "")
	if err := e.s.Grant(ctx, "mgr", "user:mgr", Owner, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("self-grant of owner: %v", err)
	}
	if err := e.s.Grant(ctx, "mgr", "user:mgr", "expenses:viewer", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("self-grant of any role: %v", err)
	}
	if err := e.s.Grant(ctx, "mgr", "user:other", Owner, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner granting owner: %v", err)
	}
	if err := e.s.Revoke(ctx, "mgr", "user:boss", Owner, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner revoking owner: %v", err)
	}
	if err := e.s.Override(ctx, "mgr", "expenses:viewer", "expenses.write", EffectGrant); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner overriding a role: %v", err)
	}
	if err := e.s.Grant(ctx, "mgr", "user:other", "expenses:viewer", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-owner granted a permission they do not hold: %v", err)
	}
	if err := e.s.Grant(ctx, "mgr", "user:other", "auditor", ""); err != nil {
		t.Fatalf("a non-owner may assign what they hold to others: %v", err)
	}
	if err := e.s.Grant(ctx, "mgr", "user:boss", "auditor", ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a non-owner changed an owner: %v", err)
	}
	if err := e.s.Grant(ctx, "boss", "user:two", Owner, ""); err != nil {
		t.Fatalf("owner granting owner: %v", err)
	}
	if err := e.s.Grant(ctx, CLIActor, "user:x", Owner, "dept/*"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("scoped owner: %v", err)
	}
	if err := e.s.Override(ctx, CLIActor, "no-such-role", "expenses.read", EffectGrant); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("override must not create roles: %v", err)
	}
}

func TestLastOwnerHoldsUnderConcurrency(t *testing.T) {
	for i := 0; i < 25; i++ {
		e := newEnv(t)
		ctx := context.Background()
		_ = e.s.Grant(ctx, CLIActor, "user:a", Owner, "")
		_ = e.s.Grant(ctx, CLIActor, "user:b", Owner, "")
		var wg sync.WaitGroup
		for _, c := range [][2]string{{"a", "user:b"}, {"b", "user:a"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = e.s.Revoke(ctx, c[0], c[1], Owner, "")
			}()
		}
		wg.Wait()
		owners, _ := e.s.SubjectsWithRole(ctx, Owner, 10)
		if len(owners) < 1 {
			t.Fatalf("iteration %d: the installation lost its last owner", i)
		}
	}
}

func TestChangesFromAnotherProcessAreSeenAtOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db1, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db1.Close() }()
	db2, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	now := time.Unix(1_700_000_000, 0)
	s1 := New(db1, audit.New(db1), Options{Now: func() time.Time { return now }, BreakGlass: true})
	s2 := New(db2, audit.New(db2), Options{Now: func() time.Time { return now }, BreakGlass: true})
	m := store.Module{ID: "x", Version: "1", Namespace: "mod-x", ServiceAccount: "x", State: store.StateEnabled, HMACSecret: "s", Manifest: config.Module{ID: "x", Permissions: []string{"x.read"}, Roles: map[string][]string{"viewer": {"x.read"}}}}
	if err := db1.UpsertModule(ctx, m, nil, nil); err != nil {
		t.Fatal(err)
	}
	_ = s1.Grant(ctx, CLIActor, "user:a", "x:viewer", "")
	if ok, _ := s1.Check(ctx, "user:a", "x.read", ""); !ok {
		t.Fatal("setup")
	}
	// The CLI (a second connection) revokes: no TTL wait, no Invalidate on s1.
	if err := s2.Revoke(ctx, CLIActor, "user:a", "x:viewer", ""); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s1.Check(ctx, "user:a", "x.read", ""); ok {
		t.Fatal("a revocation made by another process must apply at once")
	}
}

func TestScopedAssignmentIsNotRoleMembership(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.module("x", []string{"x.read"}, map[string][]string{"viewer": {"x.read"}}, store.StateEnabled)
	_ = e.s.Grant(ctx, CLIActor, "user:scoped", "x:viewer", "dept/a/*")
	_ = e.s.Grant(ctx, CLIActor, "user:global", "x:viewer", "")
	if ok, _ := e.s.HasRole(ctx, "user:scoped", "x:viewer"); ok {
		t.Fatal("a scoped assignment must not count as membership for approver resolution")
	}
	if ok, _ := e.s.HasRole(ctx, "user:global", "x:viewer"); !ok {
		t.Fatal("unscoped membership")
	}
	got, _ := e.s.SubjectsWithRole(ctx, "x:viewer", 10)
	if len(got) != 1 || got[0] != "user:global" {
		t.Fatalf("role fan-out = %v, want only the unscoped holder", got)
	}
}
