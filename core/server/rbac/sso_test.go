package rbac

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestSyncSSORolesOnlyTouchesItsOwnAssignments(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.module("expenses", []string{"expenses.read"}, map[string][]string{"viewer": {"expenses.read"}}, store.StateEnabled)
	sub := "user:sso-abcd1234-0123456789abcdef"
	if err := e.s.Grant(ctx, CLIActor, sub, "auditor", ""); err != nil { // by hand
		t.Fatal(err)
	}
	res, err := e.s.SyncSSORoles(ctx, sub, []string{"operator", "expenses:viewer", "ghost:role", "owner"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Added, ",") != "expenses:viewer,operator" || strings.Join(res.Skipped, ",") != "ghost:role,owner" {
		t.Fatalf("%+v", res)
	}
	if !e.check(sub, "expenses.read", "") || e.check(sub, "rbac.manage", "") {
		t.Fatal("permissions follow the synced roles")
	}
	res, _ = e.s.SyncSSORoles(ctx, sub, nil, false)
	if len(res.Removed) != 2 {
		t.Fatalf("%+v", res)
	}
	if ok, _ := e.s.HasRole(ctx, sub, "auditor"); !ok {
		t.Fatal("the role granted by hand must survive")
	}
}

func TestSyncSSORolesOwnerNeedsConfirmationAndLastOwnerStays(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sub := "user:sso-abcd1234-0123456789abcdef"
	if _, err := e.db.CreateAdmin(ctx, "root", "x", "y"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.CreateSSOUser(ctx, strings.TrimPrefix(sub, "user:"), store.SSOIdentity{IssuerHash: "abcd1234", Subject: "s"}, nil); err != nil {
		t.Fatal(err)
	}
	res, _ := e.s.SyncSSORoles(ctx, sub, []string{"owner"}, false)
	if len(res.Added) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("an unconfirmed owner mapping assigns nothing: %+v", res)
	}
	res, _ = e.s.SyncSSORoles(ctx, sub, []string{"owner"}, true)
	if len(res.Added) != 1 {
		t.Fatalf("%+v", res)
	}
	// root loses owner; the SSO subject is then the last owner and is kept when its group goes away.
	if err := e.s.Revoke(ctx, CLIActor, "user:root", "owner", ""); err != nil {
		t.Fatal(err)
	}
	res, _ = e.s.SyncSSORoles(ctx, sub, nil, true)
	if len(res.Removed) != 0 || len(res.Kept) != 1 {
		t.Fatalf("the last owner must stay: %+v", res)
	}
	if ok, _ := e.s.HasRole(ctx, sub, Owner); !ok {
		t.Fatal("still owner")
	}
	if _, err := e.s.SyncSSORoles(ctx, "not a subject", nil, true); err == nil {
		t.Fatal("invalid subject")
	}
}
