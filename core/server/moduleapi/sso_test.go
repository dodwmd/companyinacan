package moduleapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/sso"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type fakeUsers struct {
	info sso.UserInfo
	err  error
	seen []string
}

func (f *fakeUsers) IntrospectSession(_ context.Context, tok string) (sso.UserInfo, error) {
	f.seen = append(f.seen, tok)
	return f.info, f.err
}

const sessionTok = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789-_AbCdE" // 43 characters, the shape of a session token

func usersHarness(t *testing.T, u UserIntrospector) *harness {
	h := newHarness(t)
	s := &Server{Auth: modauth.New(h.tr, h.db), Registry: h.reg, Bus: events.NewBus(h.db), Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Users: u}
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func TestSessionShapeIsExact(t *testing.T) {
	if len(sessionTok) != 43 || !looksLikeSession(sessionTok) {
		t.Fatal("fixture")
	}
	for _, bad := range []string{"", "tok-crm", sessionTok + "x", sessionTok[:42], strings.Replace(sessionTok, "A", ".", 1), "a.b.c", strings.Replace(sessionTok, "A", " ", 1)} {
		if looksLikeSession(bad) {
			t.Errorf("%q has the shape of a session token", bad)
		}
	}
}

func TestIntrospectEndUserSession(t *testing.T) {
	u := &fakeUsers{info: sso.UserInfo{Active: true, Sub: "oidc:abcd:u-1", Subject: "user:sso-x", Email: "a@acme.example", Name: "A", Groups: []string{"hr-team"},
		Roles: []string{"operator", "support:agent", "crm:sales", "support:lead"}, AMR: []string{"mfa"}}}
	h := usersHarness(t, u)
	resp, b := h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": sessionTok})
	body := string(b)
	for _, want := range []string{`"active":true`, `"kind":"user"`, `"subject":"user:sso-x"`, `"sub":"oidc:abcd:u-1"`, `"email":"a@acme.example"`, `"groups":["hr-team"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
	// The caller (support) sees its own roles without the module prefix and the built-ins; never crm's.
	if !strings.Contains(body, `"roles":["operator","agent","lead"]`) || strings.Contains(body, `"roles":[`+`"operator","support`) {
		t.Errorf("roles for the caller: %s", body)
	}
	if resp.StatusCode != 200 || len(u.seen) != 1 {
		t.Fatalf("%d %v", resp.StatusCode, u.seen)
	}
}

func TestIntrospectEndUserSessionInactiveOrBroken(t *testing.T) {
	h := usersHarness(t, &fakeUsers{})
	_, b := h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": sessionTok})
	if strings.TrimSpace(string(b)) != `{"active":false}` {
		t.Fatalf("%s", b)
	}
	h = usersHarness(t, &fakeUsers{err: errors.New("db down")})
	resp, _ := h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": sessionTok})
	if resp.StatusCode != 502 {
		t.Fatalf("a failing lookup is an error, never 'active': %d", resp.StatusCode)
	}
	// Without the service, session-shaped tokens are inactive (not sent to TokenReview).
	h = usersHarness(t, nil)
	_, b = h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": sessionTok})
	if strings.TrimSpace(string(b)) != `{"active":false}` {
		t.Fatalf("%s", b)
	}
}

func TestIntrospectNeedsAModuleCaller(t *testing.T) {
	h := usersHarness(t, &fakeUsers{info: sso.UserInfo{Active: true}})
	resp, _ := h.call("POST", "/v1/tokens/introspect", "", map[string]string{"token": sessionTok})
	if resp.StatusCode != 401 {
		t.Fatalf("an anonymous caller must not introspect sessions: %d", resp.StatusCode)
	}
}

func TestPATsAndSessionsStayDistinct(t *testing.T) {
	u := &fakeUsers{info: sso.UserInfo{Active: true}}
	h := usersHarness(t, u)
	pat := "canpat_" + strings.Repeat("a", 36) // 43 characters, but a personal access token
	_, b := h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": pat})
	if len(u.seen) != 0 || strings.TrimSpace(string(b)) != `{"active":false}` {
		t.Fatalf("a PAT-shaped token must never reach the session introspector: %v %s", u.seen, b)
	}
}

func TestSessionIntrospectionListsTheCallersPermissions(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	m := store.Module{ID: "support", Version: "1", Namespace: "mod-support", ServiceAccount: "support", State: store.StateEnabled, HMACSecret: "x",
		Manifest: config.Module{ID: "support", Permissions: []string{"support.read", "support.write"}, Roles: map[string][]string{"agent": {"support.read"}}}}
	if err := h.db.UpsertModule(ctx, m, nil, nil); err != nil {
		t.Fatal(err)
	}
	rb := rbac.New(h.db, audit.New(h.db), rbac.Options{BreakGlass: true})
	if err := rb.Grant(ctx, rbac.CLIActor, "user:sso-x", "support:agent", ""); err != nil {
		t.Fatal(err)
	}
	u := &fakeUsers{info: sso.UserInfo{Active: true, Sub: "oidc:a:b", Subject: "user:sso-x", Roles: []string{"support:agent"}}}
	s := &Server{Auth: modauth.New(h.tr, h.db), Registry: h.reg, Bus: events.NewBus(h.db), Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Users: u, RBAC: rb}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	h.srv = srv
	_, b := h.call("POST", "/v1/tokens/introspect", "tok-support", map[string]string{"token": sessionTok})
	if !strings.Contains(string(b), `"permissions":["support.read"]`) || !strings.Contains(string(b), `"subject":"user:sso-x"`) || !strings.Contains(string(b), `"roles":["agent"]`) {
		t.Fatalf("%s", b)
	}
}
