package adminapi

import "testing"

func TestOnlyOwnersChangeOwnersOverTheAPI(t *testing.T) {
	h := newPHarness(t)
	root, alice := h.login("root"), h.login("alice")
	// alice becomes an operator, then is also trusted with role management through a custom route:
	// rbac.manage is not part of operator, so she gets a 403 from the permission gate first.
	if code, _ := h.call(root, "POST", "/api/v1/users/alice/roles", map[string]string{"role": "operator"}); code != 204 {
		t.Fatal("setup")
	}
	if code, _ := h.call(alice, "POST", "/api/v1/users/alice/roles", map[string]string{"role": "owner"}); code != 403 {
		t.Fatalf("self-grant of owner = %d", code)
	}
	// a non-owner who does hold rbac.manage (an owner-granted override) still cannot touch owners
	if code, body := h.call(root, "POST", "/api/v1/roles/operator/overrides", map[string]string{"permission": "rbac.manage", "effect": "grant"}); code != 204 {
		t.Fatalf("override = %d %s", code, body)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/users/carol/roles", map[string]string{"role": "owner"}); code != 403 {
		t.Fatalf("non-owner granting owner = %d, want 403", code)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/users/root/roles/revoke", map[string]string{"role": "owner"}); code != 403 {
		t.Fatalf("non-owner stripping an owner = %d, want 403", code)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/users/alice/roles", map[string]string{"role": "auditor"}); code != 403 {
		t.Fatalf("self-grant of any role = %d, want 403", code)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/roles/operator/overrides", map[string]string{"permission": "platform.view", "effect": "revoke"}); code != 403 {
		t.Fatalf("a non-owner editing role grants = %d, want 403", code)
	}
	if code, _ := h.call(alice, "POST", "/api/v1/users/carol/roles", map[string]string{"role": "auditor"}); code != 204 {
		t.Fatalf("ordinary assignment by a role manager = %d", code)
	}
	if code, _ := h.call(root, "POST", "/api/v1/roles/nope/overrides", map[string]string{"permission": "x.y", "effect": "grant"}); code != 400 {
		t.Fatalf("override of an unknown role = %d, want 400", code)
	}
}
