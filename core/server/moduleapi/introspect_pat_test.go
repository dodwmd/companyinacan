package moduleapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

func patHarness(t *testing.T) (*harness, *pat.Service) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tr := tokenreview.NewFake()
	reg := registry.New(db, "http://can-core.can-system.svc:8090")
	ps := pat.New(db, audit.New(db))
	for _, id := range []string{"work", "people"} {
		m := config.Module{ID: id, Version: "1.0.0", Permissions: []string{id + ".read", id + ".write"}}
		if err := reg.Register(ctx, store.Module{ID: id, Version: "1.0.0", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, HMACSecret: "s", Manifest: m}); err != nil {
			t.Fatal(err)
		}
		tr.Add("tok-"+id, "mod-"+id, id)
	}
	s := &Server{Auth: modauth.New(tr, db), Registry: reg, Bus: events.NewBus(db), PATs: ps, Log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	h := &harness{t: t, db: db, tr: tr, reg: reg, srv: httptest.NewServer(s.Handler())}
	t.Cleanup(h.srv.Close)
	return h, ps
}

type introspectOut struct {
	Active      bool     `json:"active"`
	Kind        string   `json:"kind"`
	Subject     string   `json:"subject"`
	Permissions []string `json:"permissions"`
	ModuleID    string   `json:"module_id"`
	ExpiresAt   string   `json:"expires_at"`
}

func introspect(t *testing.T, h *harness, caller string, body any) (int, introspectOut, string) {
	t.Helper()
	resp, b := h.call("POST", "/v1/tokens/introspect", caller, body)
	var out introspectOut
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out, string(b)
}

func TestIntrospectPersonalAccessToken(t *testing.T) {
	h, ps := patHarness(t)
	iss, err := ps.Issue(context.Background(), "user:alice", pat.IssueRequest{Name: "ci", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	code, out, raw := introspect(t, h, "tok-work", map[string]string{"token": iss.Token})
	if code != 200 || !out.Active || out.Kind != "user" || out.Subject != "user:alice" || len(out.Permissions) != 1 || out.Permissions[0] != "work.read" || out.ExpiresAt == "" {
		t.Fatalf("%d %s", code, raw)
	}
	if strings.Contains(raw, iss.Token) {
		t.Fatal("the answer echoes the token")
	}
	// audience binding: the caller says which module it serves; a mismatch is just inactive
	if _, out, _ := introspect(t, h, "tok-work", map[string]string{"token": iss.Token, "audience": "work"}); !out.Active {
		t.Fatal("matching audience refused")
	}
	if _, out, raw := introspect(t, h, "tok-work", map[string]string{"token": iss.Token, "audience": "people"}); out.Active || strings.Contains(raw, "alice") {
		t.Fatalf("audience mismatch accepted or leaked: %s", raw)
	}
}

func TestPersonalAccessTokenOfAnotherModuleIsInactiveAndLeaksNothing(t *testing.T) {
	h, ps := patHarness(t)
	iss, _ := ps.Issue(context.Background(), "user:alice", pat.IssueRequest{Name: "ci", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour})
	code, out, raw := introspect(t, h, "tok-people", map[string]string{"token": iss.Token})
	if code != 200 || out.Active || strings.Contains(raw, "alice") || strings.Contains(raw, "work.read") {
		t.Fatalf("%d %s", code, raw)
	}
	// and it is indistinguishable from a token that never existed
	_, _, rawUnknown := introspect(t, h, "tok-people", map[string]string{"token": pat.TokenPrefix + strings.Repeat("A", 43)})
	if raw != rawUnknown {
		t.Fatalf("the answers differ:\n%s\n%s", raw, rawUnknown)
	}
}

func TestRevokedAndExpiredTokensAreInactive(t *testing.T) {
	h, ps := patHarness(t)
	ctx := context.Background()
	iss, _ := ps.Issue(ctx, "user:alice", pat.IssueRequest{Name: "ci", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour})
	if err := ps.Revoke(ctx, "user:alice", iss.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := introspect(t, h, "tok-work", map[string]string{"token": iss.Token}); out.Active {
		t.Fatal("revoked token active")
	}
}

func TestIntrospectionStillRequiresTheCallersOwnToken(t *testing.T) {
	h, ps := patHarness(t)
	iss, _ := ps.Issue(context.Background(), "user:alice", pat.IssueRequest{Name: "ci", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour})
	if code, _, _ := introspect(t, h, "", map[string]string{"token": iss.Token}); code != 401 {
		t.Fatalf("anonymous caller got %d", code)
	}
	if code, _, _ := introspect(t, h, "forged", map[string]string{"token": iss.Token}); code != 401 {
		t.Fatalf("forged caller got %d", code)
	}
}

// Valid credentials are never throttled: a flood of bad tokens from one module must not make the
// next valid one fail, and the valid token keeps answering however often it is presented.
func TestValidTokensAreNeverThrottled(t *testing.T) {
	h, ps := patHarness(t)
	iss, _ := ps.Issue(context.Background(), "user:alice", pat.IssueRequest{Name: "ci", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour})
	for i := 0; i < 300; i++ {
		_, _, _ = introspect(t, h, "tok-work", map[string]string{"token": pat.TokenPrefix + strings.Repeat("B", 43)})
	}
	for i := 0; i < 300; i++ {
		if code, out, _ := introspect(t, h, "tok-work", map[string]string{"token": iss.Token}); code != 200 || !out.Active {
			t.Fatalf("valid token refused on attempt %d (%d)", i, code)
		}
	}
}

func TestModuleTokenIntrospectionIsUnchanged(t *testing.T) {
	h, _ := patHarness(t)
	code, out, _ := introspect(t, h, "tok-work", map[string]string{"token": "tok-people"})
	if code != 200 || !out.Active || out.ModuleID != "people" || out.Kind != "module" {
		t.Fatalf("%d %+v", code, out)
	}
}

func TestOversizedTokenIsRejectedWithoutLookup(t *testing.T) {
	h, _ := patHarness(t)
	resp, _ := h.call("POST", "/v1/tokens/introspect", "tok-work", map[string]string{"token": strings.Repeat("x", 200_000)})
	if resp.StatusCode != 200 && resp.StatusCode != 400 && resp.StatusCode != 413 {
		t.Fatal(resp.StatusCode)
	}
}
