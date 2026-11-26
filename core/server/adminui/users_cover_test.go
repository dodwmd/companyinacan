package adminui

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// A role whose module is disabled or gone, but which still has a grant override, is not a role with
// only the override's permissions: its holder cannot be taken over and it cannot be handed out by
// somebody who holds just those.
func TestRoleOfAMissingModuleIsNotKnownThroughAnOverride(t *testing.T) {
	u := newUUI(t)
	ctx := context.Background()
	billing := store.Module{ID: "billing", Version: "1", Namespace: "mod-billing", ServiceAccount: "billing", State: store.StateEnabled, HMACSecret: "s",
		Manifest: config.Module{ID: "billing", Permissions: []string{"billing.pay"}, Roles: map[string][]string{"payer": {"billing.pay"}}}}
	must(t, u.db.UpsertModule(ctx, billing, nil, nil))
	u.manager()
	must(t, u.rb.Override(ctx, "root", "billing:payer", rbac.PermView, rbac.EffectGrant))
	must(t, u.rb.Override(ctx, "root", "work:viewer", rbac.PermView, rbac.EffectGrant)) // alice covers the override's permission
	must(t, u.rb.Grant(ctx, "root", "user:carol", "billing:payer", ""))
	alice := u.loginAs("alice")

	check := func(state string) {
		t.Helper()
		u.rb.Invalidate()
		if r := u.post(alice, "/users/carol/link", "purpose", "mfa"); r.StatusCode != 403 {
			t.Errorf("%s: reset link for a holder = %d, want 403", state, r.StatusCode)
		}
		if r := u.post(alice, "/users/carol/link", "purpose", "reset"); r.StatusCode != 403 {
			t.Errorf("%s: full reset for a holder = %d, want 403", state, r.StatusCode)
		}
		if r := u.post(alice, "/users/dave/roles/grant", "role", "billing:payer"); r.StatusCode == 303 {
			t.Errorf("%s: the role was granted to an accomplice", state)
		}
		if as, _ := u.rb.SubjectAssignments(ctx, "user:dave"); len(as) != 0 {
			t.Errorf("%s: dave holds %v", state, as)
		}
	}
	check("enabled")
	billing.State = store.StateDisabled
	must(t, u.db.UpsertModule(ctx, billing, nil, nil))
	check("disabled")
	_, err := u.db.Exec(`DELETE FROM modules WHERE id='billing'`)
	must(t, err)
	check("removed")
}

func TestOnlyAChangeThatTakesAPermissionAwayEndsAccess(t *testing.T) {
	u := newUUI(t)
	dave, token := u.holdings() // work:editor
	root := u.loginAs("root")
	for _, c := range [][2]string{
		{"work.read", "grant"},      // already held
		{"work.read", "reset"},      // back to the manifest, which has it
		{"platform.view", "revoke"}, // never held
		{"platform.view", "revoke"},
		{"platform.view", "reset"},
	} {
		u.want(u.post(root, "/access/roles/work:editor/override", "permission", c[0], "effect", c[1]), 303)
	}
	if r := u.do("GET", "/settings", nil, dave.with()...); r.StatusCode != 200 {
		t.Fatalf("a change that took nothing ended the session: %d", r.StatusCode)
	}
	if _, ok := u.pats.Introspect(context.Background(), "work", token); !ok {
		t.Fatal("a change that took nothing revoked the token")
	}
	u.want(u.post(root, "/access/roles/work:editor/override", "permission", "platform.view", "effect", "grant"), 303)
	u.want(u.post(root, "/access/roles/work:editor/override", "permission", "platform.view", "effect", "reset"), 303)
	u.assertCut(dave, token)
}

// Approvals name roles: holding the same permissions is not holding that authority.
func TestRolesNamedByApprovalsNeedTheSameRole(t *testing.T) {
	for name, approver := range map[string]string{"full name": "work:editor", "bare name": "editor"} {
		t.Run(name, func(t *testing.T) {
			u := newUUI(t)
			ctx := context.Background()
			u.manager()
			must(t, u.rb.Override(ctx, "root", "work:viewer", "work.write", rbac.EffectGrant)) // all that work:editor grants
			alice := u.loginAs("alice")
			u.want(u.post(alice, "/users/dave/roles/grant", "role", "work:editor"), 303)
			u.want(u.post(alice, "/users/dave/link", "purpose", "mfa"), 200)
			root := u.loginAs("root")
			u.want(u.post(root, "/users/dave/roles/revoke", "role", "work:editor"), 303)
			must(t, u.eng.SyncPolicies(ctx, map[string]config.ApprovalPolicy{
				"spend": {Steps: []config.ApprovalStep{{Name: "s", Approvers: []config.Approver{{Role: approver}}}}}}))
			must(t, u.rb.Grant(ctx, "root", "user:carol", "work:editor", ""))
			alice = u.loginAs("alice")
			u.want(u.post(alice, "/users/dave/roles/grant", "role", "work:editor"), 403)
			u.want(u.post(alice, "/users/carol/link", "purpose", "mfa"), 403)
			// A scoped assignment gives no approval authority, and holding the role covers it.
			u.want(u.post(alice, "/users/dave/roles/grant", "role", "work:editor", "scope", "p/*"), 303)
			must(t, u.rb.Grant(ctx, "root", "user:alice", "work:editor", ""))
			alice = u.loginAs("alice")
			u.want(u.post(alice, "/users/carol/link", "purpose", "mfa"), 200)
		})
	}
}

// An address one administrator stored is shown, its change is recorded, and it does not follow the
// account into access another administrator gives.
func TestPlantedAddressDoesNotReceiveLaterLinks(t *testing.T) {
	u := newUUI(t)
	u.mail.ready = true
	alice := u.manager()
	u.want(u.post(alice, "/users", "username", "bob", "email", "planted@acme.example"), 200)
	root := u.loginAs("root")
	page := u.want(u.do("GET", "/users/bob", nil, root.with()...), 200).body
	for _, want := range []string{"planted@acme.example", "stored by alice", "Send the link by email to planted@acme.example"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	u.want(u.post(root, "/users/bob/roles/grant", "role", "operator"), 303)
	r := u.want(u.post(root, "/users/bob/link", "purpose", "invite", "send", "1"), 200)
	if len(u.mail.sent) != 0 || !linkRe.MatchString(r.body) {
		t.Fatalf("mailed to a forgotten address: %v", u.mail.sent)
	}
	// An address the granting administrator stored stays.
	u.want(u.post(root, "/users/bob/link", "purpose", "invite", "email", "bob@acme.example"), 200)
	u.want(u.post(root, "/users/bob/roles/grant", "role", "auditor"), 303)
	u.want(u.post(root, "/users/bob/link", "purpose", "invite", "send", "1"), 200)
	if len(u.mail.sent) != 1 || !strings.HasPrefix(u.mail.sent[0], "bob@acme.example|") {
		t.Fatalf("sent: %v", u.mail.sent)
	}
	got := strings.Join(u.actions(), " ")
	for _, a := range []string{"users.email.change", "users.email.cleared"} {
		if !strings.Contains(got, a) {
			t.Errorf("no %s audit entry", a)
		}
	}
	recs, err := u.audit.List(context.Background(), 0, 500)
	must(t, err)
	for _, rec := range recs {
		if strings.Contains(string(rec.Detail), "@acme.example") {
			t.Errorf("an address is in the audit log: %s", rec.Detail)
		}
	}
}
