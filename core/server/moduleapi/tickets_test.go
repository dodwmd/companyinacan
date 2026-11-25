package moduleapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/modauth"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/server/ticket"
	"github.com/dodwmd/companyinacan/core/server/tokenreview"
)

type ticketEnv struct {
	t   *testing.T
	srv *httptest.Server
	svc *ticket.Service
	db  *store.DB
}

func newTicketEnv(t *testing.T) *ticketEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { return now }
	tr := tokenreview.NewFake()
	if _, err := db.CreateAdmin(ctx, "alice", "x", "y"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"work", "people"} {
		m := config.Module{ID: id, Name: id, Version: "1.0.0", Permissions: []string{id + ".read", id + ".write"}}
		m.Runtime.Subdomain = id
		m.Runtime.WebLogin = &config.WebLogin{ViaSubdomain: true}
		if err := db.UpsertModule(ctx, store.Module{ID: id, Version: "1.0.0", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, HMACSecret: "s", Manifest: m}, nil, nil); err != nil {
			t.Fatal(err)
		}
		tr.Add("tok-"+id, "mod-"+id, id)
	}
	svc := &ticket.Service{DB: db, Audit: audit.New(db), Now: func() time.Time { return now }, Launcher: ticket.Launcher{Domain: "example.org"},
		Holds: func(_ context.Context, subject, perm string) (bool, error) {
			return subject == "user:alice" && strings.HasSuffix(perm, ".read"), nil
		}}
	s := &Server{Auth: modauth.New(tr, db), Registry: registry.New(db, "http://can-core"), Bus: events.NewBus(db), Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), Tickets: svc}
	e := &ticketEnv{t: t, db: db, svc: svc, srv: httptest.NewServer(s.Handler())}
	t.Cleanup(e.srv.Close)
	return e
}

func (e *ticketEnv) call(path, token string, body any) (int, string) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(out))
}

func (e *ticketEnv) mint(module string) string {
	e.t.Helper()
	res, err := e.svc.Launch(context.Background(), "user:alice", module, "/x")
	if err != nil {
		e.t.Fatal(err)
	}
	return res.URL[strings.Index(res.URL, "t=")+2:]
}

func TestRedeemOverHTTP(t *testing.T) {
	e := newTicketEnv(t)
	tk := e.mint("work")
	code, body := e.call("/v1/tickets/redeem", "tok-work", map[string]string{"ticket": tk})
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var out struct {
		Active      bool     `json:"active"`
		Subject     string   `json:"subject"`
		Name        string   `json:"name"`
		Permissions []string `json:"permissions"`
		ReturnPath  string   `json:"return_path"`
		Session     string   `json:"session"`
		ExpiresAt   string   `json:"session_expires_at"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Active || out.Subject != "user:alice" || out.Name == "" || out.ReturnPath != "/x" || !ticket.LooksLikeHandle(out.Session) || out.ExpiresAt == "" || strings.Join(out.Permissions, ",") != "work.read" {
		t.Fatalf("%+v", out)
	}
	for _, leak := range []string{"email", "groups", "token_hash", tk} {
		if strings.Contains(body, leak) {
			t.Fatalf("answer leaks %q: %s", leak, body)
		}
	}
	// Revalidate with the handle: the same module only.
	code, body = e.call("/v1/tickets/revalidate", "tok-work", map[string]string{"session": out.Session})
	if code != 200 || !strings.Contains(body, `"active":true`) || !strings.Contains(body, `"user:alice"`) {
		t.Fatalf("%d %s", code, body)
	}
	code, body = e.call("/v1/tickets/revalidate", "tok-people", map[string]string{"session": out.Session})
	if code != 200 || body != `{"active":false}` {
		t.Fatalf("another module revalidated: %d %s", code, body)
	}
}

func TestInactiveAnswersAreByteIdentical(t *testing.T) {
	e := newTicketEnv(t)
	used := e.mint("work")
	e.call("/v1/tickets/redeem", "tok-work", map[string]string{"ticket": used})
	foreign := e.mint("work")
	cases := map[string]struct{ tok, ticket string }{
		"replayed":         {"tok-work", used},
		"another audience": {"tok-people", foreign},
		"unknown":          {"tok-work", "cant_" + strings.Repeat("Z", 43)},
		"malformed":        {"tok-work", "junk"},
		"empty":            {"tok-work", ""},
		// The admin UI session cookie value, or a personal access token, is never a ticket.
		"session cookie value":  {"tok-work", strings.Repeat("a", 43)},
		"personal access token": {"tok-work", "canpat_" + strings.Repeat("a", 43)},
	}
	for name, c := range cases {
		code, body := e.call("/v1/tickets/redeem", c.tok, map[string]string{"ticket": c.ticket})
		if code != 200 || body != `{"active":false}` {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}

func TestTicketsNeedAModuleTokenAndTheRoutesAreNotIntrospection(t *testing.T) {
	e := newTicketEnv(t)
	tk := e.mint("work")
	if code, _ := e.call("/v1/tickets/redeem", "", map[string]string{"ticket": tk}); code != 401 {
		t.Fatalf("anonymous redeem -> %d", code)
	}
	if code, _ := e.call("/v1/tickets/revalidate", "forged", map[string]string{"session": "x"}); code != 401 {
		t.Fatalf("forged revalidate -> %d", code)
	}
	// A ticket presented to token introspection is just an unknown token.
	code, body := e.call("/v1/tokens/introspect", "tok-work", map[string]string{"token": tk})
	if code != 200 || body != `{"active":false}` {
		t.Fatalf("a ticket introspected: %d %s", code, body)
	}
	// ... and it is still redeemable afterwards: introspection did not consume it.
	if code, body := e.call("/v1/tickets/redeem", "tok-work", map[string]string{"ticket": tk}); code != 200 || !strings.Contains(body, `"active":true`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestTicketRoutesAnswer503WithoutTheService(t *testing.T) {
	e := newTicketEnv(t)
	tr := tokenreview.NewFake()
	tr.Add("tok-work", "mod-work", "work")
	s := &Server{Auth: modauth.New(tr, e.db), Log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	e.srv = srv
	for _, p := range []string{"/v1/tickets/redeem", "/v1/tickets/revalidate"} {
		if code, _ := e.call(p, "tok-work", map[string]string{"ticket": "x"}); code != 503 {
			t.Errorf("%s -> %d", p, code)
		}
	}
}
