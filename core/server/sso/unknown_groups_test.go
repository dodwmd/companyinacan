package sso

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Groups that could not be read are not "no groups": the sign-in is refused and nothing the person
// holds changes.
func TestFailedUserinfoRemovesNoRole(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.Mutate = func(c map[string]any) { delete(c, "groups") } // only userinfo knows them
	r := e.mustLogin(alice())
	if len(r.Roles) != 1 || r.Roles[0] != "operator" {
		t.Fatalf("roles from userinfo: %v", r.Roles)
	}
	ids, err := e.db.ListSSOIdentities(context.Background(), 10)
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	name := ids[0].Username
	e.idp.UserinfoDown = true
	_, err = e.login(alice())
	wantDenied(t, err, "userinfo")
	if got := e.roles(name); len(got) != 1 {
		t.Fatalf("a failed call removed roles: %v", got)
	}
	if _, _, err := e.au.Authenticate(context.Background(), r.Login.Token); err != nil {
		t.Fatalf("a failed call ended access: %v", err)
	}
	if !strings.Contains(e.auditActions(), "sso.login.failed") {
		t.Fatal("not audited")
	}
	// A token that does say the person is in no group is believed.
	e.idp.Mutate = func(c map[string]any) { c["groups"] = []string{} }
	if r := e.mustLogin(alice()); len(r.Roles) != 0 {
		t.Fatalf("roles after an empty claim: %v", r.Roles)
	}
}

// A disabled single sign-on owner cannot sign in, so with local_login disabled the local owner must.
func TestDisabledSSOOwnerDoesNotKeepLocalLoginOff(t *testing.T) {
	e := newEnv(t, func(c *config.SSO) { c.LocalLogin = config.LocalLoginDisabled; c.ConfirmDisableLocalLogin = true })
	ctx := context.Background()
	u := alice()
	u.Groups = []string{"admins"}
	e.mustLogin(u)
	root, err := e.db.AdminByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	if e.svc.AllowLocalLogin(ctx, root) {
		t.Fatal("with an SSO owner, local login is off")
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE admin_users SET disabled_at=1 WHERE auth_source=?`, store.SourceSSO); err != nil {
		t.Fatal(err)
	}
	if !e.svc.AllowLocalLogin(ctx, root) {
		t.Fatal("the only SSO owner is disabled: the local owner is locked out")
	}
	st, err := e.svc.Status(ctx)
	if err != nil || st.LocalLoginEffective != config.LocalLoginOwnerOnly {
		t.Fatalf("status: %+v %v", st.LocalLoginEffective, err)
	}
}
