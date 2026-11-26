package adminapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/users"
)

// The API reaches the same service as the UI: one pass over its routes, the statuses of its
// refusals and the step-up.
func TestUserAdministrationAPI(t *testing.T) {
	h := newPHarness(t)
	root := h.login("root")
	alice := h.login("alice")

	if code, _ := h.call(alice, "POST", "/api/v1/users", map[string]any{"username": "bob"}); code != 403 {
		t.Fatalf("without rbac.manage = %d", code)
	}
	code, body := h.call(root, "POST", "/api/v1/users", map[string]any{"username": "bob", "email": "bob@acme.example"})
	var link users.Link
	if code != 201 || json.Unmarshal([]byte(body), &link) != nil || !strings.HasPrefix(link.Path, users.SetupPath+"?token=") || link.Emailed {
		t.Fatalf("create = %d %s", code, body)
	}
	if code, _ := h.call(root, "POST", "/api/v1/users", map[string]any{"username": "bob"}); code != 409 {
		t.Fatalf("duplicate = %d", code)
	}
	code, body = h.call(root, "GET", "/api/v1/users/bob", nil)
	token := strings.TrimPrefix(link.Path, users.SetupPath+"?token=")
	if code != 200 || !strings.Contains(body, `"status":"invited"`) || strings.Contains(body, token) {
		t.Fatalf("detail = %d %s", code, body)
	}
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"POST", "/api/v1/users/bob/link", map[string]any{"purpose": "invite"}, 201},
		{"POST", "/api/v1/users/bob/disable", nil, 204},
		{"POST", "/api/v1/users/bob/enable", nil, 204},
		{"DELETE", "/api/v1/users/bob/sessions", nil, 204},
		{"DELETE", "/api/v1/users/bob/sessions/0123456789abcdef", nil, 404},
		{"DELETE", "/api/v1/users/bob/tokens/pat_0000", nil, 404},
		{"POST", "/api/v1/users/root/disable", nil, 400},
		{"DELETE", "/api/v1/users/root", nil, 400},
		{"DELETE", "/api/v1/users/bob", nil, 204},
		{"GET", "/api/v1/users/bob", nil, 404},
	} {
		if code, body := h.call(root, c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s = %d %s, want %d", c.method, c.path, code, body, c.want)
		}
	}

	// An old sign-in is refused until the step-up.
	*h.now = h.now.Add(users.StepUpWindow + time.Second)
	code, body = h.call(root, "POST", "/api/v1/users/alice/roles", map[string]string{"role": "operator"})
	if code != 403 || !strings.Contains(body, "recent authentication required") {
		t.Fatalf("stale = %d %s", code, body)
	}
	if code, _ := h.call(root, "POST", "/api/v1/reauth", map[string]string{"password": "nope", "totp": "000000"}); code != 403 {
		t.Fatalf("bad step-up = %d", code)
	}
	if code, body := h.call(root, "POST", "/api/v1/reauth", stepUp(h, "root")); code != 204 {
		t.Fatalf("step-up = %d %s", code, body)
	}
	if code, body := h.call(root, "POST", "/api/v1/users/alice/roles", map[string]string{"role": "operator"}); code != 204 {
		t.Fatalf("after step-up = %d %s", code, body)
	}
}

func stepUp(h *pHarness, user string) map[string]string {
	key, _ := auth.DecodeTOTPSecret(h.secrets[user])
	return map[string]string{"password": h.pw, "totp": auth.Code(key, auth.SHA1, 6, 30, *h.now)}
}
