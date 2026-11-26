package moduleapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
)

// The admin UI session cookie is the user's login, not a credential for modules: whatever its shape,
// introspection never treats it as one (it is just an unknown token).
func TestUISessionTokenIsNeverIntrospected(t *testing.T) {
	h := newHarness(t)
	tok := "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCdE" // 43 characters, the shape of a can_session value
	for _, tk := range []string{tok, strings.Repeat("a", 43)} {
		resp, b := h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": tk})
		if resp.StatusCode != 200 || strings.TrimSpace(string(b)) != `{"active":false}` {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
	}
}

func TestLiveSessionCookieValueIsInactiveAtModules(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	au := auth.NewService(h.db, audit.New(h.db), auth.Options{})
	u, err := h.db.CreateAdmin(ctx, "root", "h", "t")
	if err != nil {
		t.Fatal(err)
	}
	lr, err := au.CreateSSOSession(ctx, u.ID, "203.0.113.1", 0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := au.Authenticate(ctx, lr.Token); err != nil {
		t.Fatalf("fixture: the session must be live: %v", err)
	}
	_, b := h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": lr.Token})
	if strings.TrimSpace(string(b)) != `{"active":false}` {
		t.Fatalf("a live UI session value must never validate at a module: %s", b)
	}
}
