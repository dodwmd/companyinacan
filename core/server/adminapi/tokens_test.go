package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type issuedBody struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

func TestTokenAPIIssueListRevoke(t *testing.T) {
	h := newPHarness(t)
	root := h.login("root")
	code, body := h.call(root, "POST", "/api/v1/tokens", map[string]any{"module": "expenses", "name": "ci", "permissions": []string{"expenses.approve"}, "ttl_hours": 24})
	if code != 201 {
		t.Fatalf("issue = %d %s", code, body)
	}
	var iss issuedBody
	_ = json.Unmarshal([]byte(body), &iss)
	if !strings.HasPrefix(iss.Token, "canpat_") || iss.ID == "" || iss.ExpiresAt == "" {
		t.Fatalf("%s", body)
	}
	// the list never carries the token or its hash
	code, body = h.call(root, "GET", "/api/v1/tokens", nil)
	if code != 200 || strings.Contains(body, iss.Token) || strings.Contains(body, "token_hash") || !strings.Contains(body, iss.ID) {
		t.Fatalf("list = %d %s", code, body)
	}
	if code, body = h.call(root, "DELETE", "/api/v1/tokens/"+iss.ID, nil); code != 204 {
		t.Fatalf("revoke = %d %s", code, body)
	}
	if _, ok := h.pats.Introspect(context.Background(), "expenses", iss.Token); ok {
		t.Fatal("token still valid after revoke")
	}
	if code, _ = h.call(root, "DELETE", "/api/v1/tokens/pat_missing", nil); code != 404 {
		t.Fatalf("revoking an unknown token = %d", code)
	}
}

func TestTokenIssueIsLimitedToWhatTheIssuerHolds(t *testing.T) {
	h := newPHarness(t)
	alice := h.login("alice") // no roles
	code, body := h.call(alice, "POST", "/api/v1/tokens", map[string]any{"module": "expenses", "name": "x", "permissions": []string{"expenses.approve"}, "ttl_hours": 24})
	if code != 403 {
		t.Fatalf("a user without the permission minted a token: %d %s", code, body)
	}
	root := h.login("root")
	if code, body := h.call(root, "POST", "/api/v1/users/alice/roles", map[string]string{"role": "expenses:finance"}); code != 204 {
		t.Fatalf("grant = %d %s", code, body)
	}
	h.rb.Invalidate()
	alice = h.login("alice")
	if code, body = h.call(alice, "POST", "/api/v1/tokens", map[string]any{"module": "expenses", "name": "x", "permissions": []string{"expenses.approve"}, "ttl_hours": 24}); code != 201 {
		t.Fatalf("issue after the grant = %d %s", code, body)
	}
	// holds approve, not pay
	if code, _ = h.call(alice, "POST", "/api/v1/tokens", map[string]any{"module": "expenses", "name": "x", "permissions": []string{"expenses.pay"}, "ttl_hours": 24}); code != 403 {
		t.Fatalf("a permission the issuer lacks was granted: %d", code)
	}
}

func TestTokenListAndRevokeAreOwnUnlessManager(t *testing.T) {
	h := newPHarness(t)
	root := h.login("root")
	_, body := h.call(root, "POST", "/api/v1/tokens", map[string]any{"module": "expenses", "name": "rootsci", "permissions": []string{"expenses.approve"}, "ttl_hours": 24})
	var iss issuedBody
	_ = json.Unmarshal([]byte(body), &iss)
	dave := h.login("dave")
	if code, body := h.call(dave, "GET", "/api/v1/tokens", nil); code != 200 || strings.Contains(body, iss.ID) {
		t.Fatalf("dave sees root's token: %d %s", code, body)
	}
	if code, _ := h.call(dave, "DELETE", "/api/v1/tokens/"+iss.ID, nil); code != 404 {
		t.Fatalf("dave revoking root's token = %d", code)
	}
}

func TestTokenIssueValidation(t *testing.T) {
	h := newPHarness(t)
	root := h.login("root")
	bad := []map[string]any{
		{"module": "nope", "name": "x", "permissions": []string{"expenses.approve"}, "ttl_hours": 24},
		{"module": "expenses", "name": "x", "permissions": []string{"expenses.root"}, "ttl_hours": 24},
		{"module": "expenses", "name": "x", "permissions": []string{}, "ttl_hours": 24},
		{"module": "expenses", "name": "x", "permissions": []string{"expenses.approve"}, "ttl_hours": 0},
		{"module": "expenses", "name": "x", "permissions": []string{"expenses.approve"}, "ttl_hours": 100000},
		{"module": "expenses", "name": "", "permissions": []string{"expenses.approve"}, "ttl_hours": 24},
	}
	for i, b := range bad {
		if code, body := h.call(root, "POST", "/api/v1/tokens", b); code != 400 {
			t.Errorf("case %d = %d %s", i, code, body)
		}
	}
}

func TestTokenRoutesNeedASession(t *testing.T) {
	h := newPHarness(t)
	for _, r := range []struct{ m, p string }{{"GET", "/api/v1/tokens"}, {"POST", "/api/v1/tokens"}, {"DELETE", "/api/v1/tokens/x"}} {
		req, _ := http.NewRequest(r.m, h.srv.URL+r.p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s %s without a session = %d", r.m, r.p, resp.StatusCode)
		}
	}
}
