package work

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const bootstrap = "bootstrap-token-0123456789abcdefXYZ"

type harness struct {
	t   *testing.T
	app *App
	clk *clock
	h   http.Handler
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "work.db")
	if mutate != nil {
		mutate(cfg)
	}
	clk := &clock{t: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}
	app, err := NewApp(cfg, Secrets{AdminToken: bootstrap, WebhookSecret: "whsec-0123456789abcdef"}, Options{Now: clk.Now, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return &harness{t: t, app: app, clk: clk, h: app.Handler()}
}

type resp struct {
	Code   int
	Header http.Header
	Body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.Code, r.Body)
	}
	return m
}

func (h *harness) do(method, path, token string, body any, hdr ...string) resp {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "192.0.2.10:5555"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
}

func (h *harness) mustDo(want int, method, path, token string, body any, hdr ...string) map[string]any {
	h.t.Helper()
	r := h.do(method, path, token, body, hdr...)
	if r.Code != want {
		h.t.Fatalf("%s %s = %d, want %d: %s", method, path, r.Code, want, r.Body)
	}
	if len(r.Body) == 0 {
		return nil
	}
	return r.json(h.t)
}

// newToken creates a stored token through the API and returns its secret.
func (h *harness) newToken(kind, subject string, scopes []string, projects ...string) string {
	h.t.Helper()
	m := h.mustDo(201, "POST", "/v1/tokens", bootstrap, map[string]any{"kind": kind, "subject": subject, "scopes": scopes, "projects": projects})
	return m["token"].(string)
}

func (h *harness) setup() (writer string) {
	h.t.Helper()
	h.mustDo(201, "POST", "/v1/projects", bootstrap, map[string]any{"key": "ACME", "name": "Acme"})
	h.mustDo(201, "POST", "/v1/projects", bootstrap, map[string]any{"key": "SEC", "name": "Security"})
	return h.newToken("service", "planner-bot", []string{PermWrite})
}

func TestAuthRequired(t *testing.T) {
	h := newHarness(t, nil)
	for _, tok := range []string{"", "nope", bootstrap + "x", "cwt_0123456789abcdef_" + strings.Repeat("a", 43)} {
		r := h.do("GET", "/v1/projects", tok, nil)
		if r.Code != 401 || r.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("token %q: %d", tok, r.Code)
		}
	}
	if r := h.do("GET", "/healthz", "", nil); r.Code != 200 {
		t.Fatalf("healthz needs no auth: %d", r.Code)
	}
	if h.mustDo(200, "GET", "/v1/me", bootstrap, nil)["subject"] != "bootstrap-admin" {
		t.Fatal("me")
	}
}

func TestSecurityHeadersAndErrorsAreJSON(t *testing.T) {
	h := newHarness(t, nil)
	r := h.do("GET", "/v1/projects", "", nil)
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store", "Content-Type": "application/json"} {
		if r.Header.Get(k) != v {
			t.Errorf("%s = %q", k, r.Header.Get(k))
		}
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Error("CSP missing")
	}
}

func TestTokenScopesEnforced(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	reader := h.newToken("service", "dash", []string{PermRead})
	writer := h.newToken("service", "bot", []string{PermWrite})
	adminTok := h.newToken("service", "ops", []string{PermAdmin})

	issue := map[string]any{"type": "task", "title": "x"}
	h.mustDo(403, "POST", "/v1/projects/ACME/issues", reader, issue)
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, issue)
	h.mustDo(200, "GET", "/v1/issues", reader, nil)
	h.mustDo(200, "GET", "/v1/issues", writer, nil) // write implies read

	// Admin-only surfaces.
	for _, tok := range []string{reader, writer} {
		h.mustDo(403, "GET", "/v1/tokens", tok, nil)
		h.mustDo(403, "POST", "/v1/tokens", tok, map[string]any{"kind": "service", "subject": "x", "scopes": []string{PermRead}})
		h.mustDo(403, "POST", "/v1/projects", tok, map[string]any{"key": "NEW", "name": "n"})
		h.mustDo(403, "PATCH", "/v1/projects/ACME", tok, map[string]any{"name": "n"})
		h.mustDo(403, "GET", "/v1/audit", tok, nil)
	}
	h.mustDo(200, "GET", "/v1/tokens", adminTok, nil)
	h.mustDo(201, "POST", "/v1/issues/ACME-1/comments", adminTok, map[string]any{"body": "admin implies write"})
}

func TestTokenLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	m := h.mustDo(201, "POST", "/v1/tokens", bootstrap, map[string]any{"kind": "personal", "subject": "alice@example.com", "name": "laptop", "scopes": []string{PermWrite}, "projects": []string{"ACME"}})
	secret, info := m["token"].(string), m["info"].(map[string]any)
	id := info["id"].(string)
	if !strings.HasPrefix(secret, "cwt_"+id+"_") {
		t.Fatalf("token format: %s", secret)
	}
	if info["expires_at"] == nil {
		t.Fatal("personal tokens always expire")
	}
	// Only a hash is stored.
	var stored []byte
	_ = h.app.store.db.QueryRow(`SELECT secret_hash FROM api_tokens WHERE id = ?`, id).Scan(&stored)
	if len(stored) != 32 || bytes.Contains(stored, []byte(secret)) {
		t.Fatal("secret must be stored hashed")
	}
	rows, _ := h.app.store.db.Query(`SELECT * FROM api_tokens`)
	_ = rows.Close()
	list := h.do("GET", "/v1/tokens", bootstrap, nil)
	if bytes.Contains(list.Body, []byte(secret)) || strings.Contains(string(list.Body), "secret") {
		t.Fatalf("listing leaks the secret: %s", list.Body)
	}

	me := h.mustDo(200, "GET", "/v1/me", secret, nil)
	if me["subject"] != "alice@example.com" || me["kind"] != "personal" {
		t.Fatalf("me: %v", me)
	}
	got, _ := h.app.store.GetToken(context.Background(), id)
	if got.LastUsedAt == nil {
		t.Fatal("last_used_at not recorded")
	}
	// The reporter of what alice creates is alice, from the token, not from the body.
	is := h.mustDo(201, "POST", "/v1/projects/ACME/issues", secret, map[string]any{"type": "task", "title": "mine"})
	if is["reporter"] != "alice@example.com" {
		t.Fatalf("reporter %v", is["reporter"])
	}

	// Revocation is immediate and audited.
	h.mustDo(204, "DELETE", "/v1/tokens/"+id, bootstrap, nil)
	h.mustDo(401, "GET", "/v1/me", secret, nil)
	h.mustDo(404, "DELETE", "/v1/tokens/ffffffffffffffff", bootstrap, nil)

	// Expiry.
	m = h.mustDo(201, "POST", "/v1/tokens", bootstrap, map[string]any{"kind": "service", "subject": "bot", "scopes": []string{PermRead}, "expires_at": h.clk.Now().Add(time.Hour).Format(time.RFC3339)})
	short := m["token"].(string)
	h.mustDo(200, "GET", "/v1/me", short, nil)
	h.clk.Advance(2 * time.Hour)
	h.mustDo(401, "GET", "/v1/me", short, nil)

	audit := h.mustDo(200, "GET", "/v1/audit", bootstrap, nil)["items"].([]any)
	var actions []string
	for _, e := range audit {
		actions = append(actions, e.(map[string]any)["action"].(string))
	}
	for _, want := range []string{"token.create", "token.revoke", "project.create"} {
		if !strings.Contains(strings.Join(actions, " "), want) {
			t.Errorf("audit lacks %s: %v", want, actions)
		}
	}
	if strings.Contains(string(h.do("GET", "/v1/audit", bootstrap, nil).Body), secret) {
		t.Fatal("audit must never hold secrets")
	}
}

func TestTokenCreationRules(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	bad := []map[string]any{
		{"kind": "robot", "subject": "x", "scopes": []string{PermRead}},
		{"kind": "service", "subject": "", "scopes": []string{PermRead}},
		{"kind": "service", "subject": "x", "scopes": []string{}},
		{"kind": "service", "subject": "x", "scopes": []string{"work.root"}},
		{"kind": "service", "subject": "x", "scopes": []string{PermRead}, "projects": []string{"NOPE"}},
		{"kind": "personal", "subject": "x", "scopes": []string{PermRead}, "expires_at": h.clk.Now().Add(800 * 24 * time.Hour).Format(time.RFC3339)},
		{"kind": "service", "subject": "x", "scopes": []string{PermRead}, "expires_at": h.clk.Now().Add(-time.Hour).Format(time.RFC3339)},
		{"kind": "service", "subject": "x", "scopes": []string{PermRead}, "is_admin": true},
	}
	for i, b := range bad {
		if r := h.do("POST", "/v1/tokens", bootstrap, b); r.Code != 422 && r.Code != 400 {
			t.Errorf("case %d accepted: %d %s", i, r.Code, r.Body)
		}
	}
	// A project-scoped admin cannot mint broader tokens or reach other projects.
	scopedAdmin := h.newToken("service", "acme-admin", []string{PermAdmin}, "ACME")
	h.mustDo(422, "POST", "/v1/tokens", scopedAdmin, map[string]any{"kind": "service", "subject": "x", "scopes": []string{PermRead}})
	h.mustDo(422, "POST", "/v1/tokens", scopedAdmin, map[string]any{"kind": "service", "subject": "x", "scopes": []string{PermRead}, "projects": []string{"SEC"}})
	h.mustDo(201, "POST", "/v1/tokens", scopedAdmin, map[string]any{"kind": "service", "subject": "x", "scopes": []string{PermRead}, "projects": []string{"ACME"}})
	h.mustDo(403, "GET", "/v1/tokens", scopedAdmin, nil)
	h.mustDo(403, "GET", "/v1/audit", scopedAdmin, nil)
	h.mustDo(403, "POST", "/v1/projects", scopedAdmin, map[string]any{"key": "NEW", "name": "n"})
	// A writer cannot mint an admin token.
	writer := h.newToken("service", "w", []string{PermWrite})
	h.mustDo(403, "POST", "/v1/tokens", writer, map[string]any{"kind": "service", "subject": "x", "scopes": []string{PermAdmin}})
}

func TestIDORAcrossProjectsOverHTTP(t *testing.T) {
	h := newHarness(t, nil)
	h.setup()
	secretIssue := h.mustDo(201, "POST", "/v1/projects/SEC/issues", bootstrap, map[string]any{"type": "bug", "title": "vuln in prod"})["key"].(string)
	acme := h.newToken("service", "acme-bot", []string{PermWrite}, "ACME")

	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/issues/" + secretIssue}, {"PATCH", "/v1/issues/" + secretIssue},
		{"GET", "/v1/issues/" + secretIssue + "/comments"}, {"POST", "/v1/issues/" + secretIssue + "/comments"},
		{"GET", "/v1/issues/" + secretIssue + "/activity"}, {"POST", "/v1/issues/" + secretIssue + "/transitions"},
		{"POST", "/v1/issues/" + secretIssue + "/links"}, {"PUT", "/v1/issues/" + secretIssue + "/watchers/x"},
		{"POST", "/v1/issues/" + secretIssue + "/attachments"}, {"POST", "/v1/issues/" + secretIssue + "/rank"},
		{"GET", "/v1/projects/SEC"}, {"GET", "/v1/projects/SEC/board"}, {"GET", "/v1/projects/SEC/sprints"},
		{"POST", "/v1/projects/SEC/issues"}, {"POST", "/v1/projects/SEC/sprints"},
	} {
		// Valid bodies, so the answer comes from the scope check and not from validation.
		body := map[string]any{}
		for suffix, b := range map[string]map[string]any{
			"/comments": {"body": "x"}, "/transitions": {"to": "done"}, "/links": {"type": "blocks", "target": "ACME-1"},
			"/attachments": {"filename": "a.txt"}, "/rank": {"position": "first"}, "/SEC/issues": {"type": "task", "title": "t"},
			"/SEC/sprints": {"name": "s"},
		} {
			if strings.HasSuffix(c.path, suffix) {
				body = b
			}
		}
		r := h.do(c.method, c.path, acme, body)
		if r.Code != 404 {
			t.Errorf("%s %s = %d, want 404 (not 403: no existence oracle): %s", c.method, c.path, r.Code, r.Body)
		}
	}
	// A missing key in an allowed project looks exactly the same as a forbidden one.
	missing := h.do("GET", "/v1/issues/ACME-999", acme, nil)
	forbidden := h.do("GET", "/v1/issues/"+secretIssue, acme, nil)
	if missing.Code != forbidden.Code || !bytes.Equal(missing.Body, forbidden.Body) {
		t.Fatalf("responses differ: %d %s vs %d %s", missing.Code, missing.Body, forbidden.Code, forbidden.Body)
	}
	// Search and listings never include it.
	for _, p := range []string{"/v1/issues", "/v1/issues?q=vuln", "/v1/issues?project=SEC", "/v1/issues?q=key:" + secretIssue} {
		if body := h.do("GET", p, acme, nil).Body; strings.Contains(string(body), "vuln in prod") {
			t.Errorf("%s leaked: %s", p, body)
		}
	}
	if body := h.do("GET", "/v1/projects", acme, nil).Body; strings.Contains(string(body), "SEC") {
		t.Errorf("project list leaked: %s", body)
	}
	// Bulk update reports out-of-scope keys as not found and leaves them untouched.
	r := h.mustDo(200, "POST", "/v1/issues/bulk-update", acme, map[string]any{"keys": []string{secretIssue}, "set": map[string]any{"priority": "P0"}})
	if res := r["results"].([]any)[0].(map[string]any); res["ok"] != false || res["code"] != "not_found" {
		t.Fatalf("bulk: %v", res)
	}
	if got := h.mustDo(200, "GET", "/v1/issues/"+secretIssue, bootstrap, nil); got["priority"] != "P2" {
		t.Fatal("out-of-scope issue was modified")
	}
	// Idempotency keys are not shared across callers.
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", acme, map[string]any{"type": "task", "title": "a"}, "Idempotency-Key", "shared-key-123")
	other := h.newToken("service", "other-bot", []string{PermWrite}, "ACME")
	if r := h.do("POST", "/v1/projects/ACME/issues", other, map[string]any{"type": "task", "title": "b"}, "Idempotency-Key", "shared-key-123"); r.Code != 201 {
		t.Fatalf("another caller's idempotency key interfered: %d %s", r.Code, r.Body)
	}
}

func TestMassAssignmentRejected(t *testing.T) {
	h := newHarness(t, nil)
	writer := h.setup()
	for _, extra := range []string{"reporter", "key", "id", "status", "version", "created_at", "rank", "project", "number", "resolved_at"} {
		body := map[string]any{"type": "task", "title": "x", extra: "mallory"}
		if r := h.do("POST", "/v1/projects/ACME/issues", writer, body); r.Code != 400 {
			t.Errorf("create accepted %q: %d %s", extra, r.Code, r.Body)
		}
	}
	is := h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "task", "title": "x"})
	key := is["key"].(string)
	for _, extra := range []string{"reporter", "key", "id", "status", "type", "created_at", "rank", "project", "resolved_at"} {
		if r := h.do("PATCH", "/v1/issues/"+key, writer, map[string]any{extra: "done"}); r.Code != 400 {
			t.Errorf("patch accepted %q: %d %s", extra, r.Code, r.Body)
		}
	}
	// Status can only move through the workflow.
	if r := h.do("PATCH", "/v1/issues/"+key, writer, map[string]any{"status": "done"}); r.Code != 400 {
		t.Fatal("status via PATCH")
	}
	// Token, project and sprint bodies too.
	h.mustDo(400, "POST", "/v1/projects", bootstrap, map[string]any{"key": "ZZ", "name": "z", "next_number": 500})
	h.mustDo(400, "POST", "/v1/tokens", bootstrap, map[string]any{"kind": "service", "subject": "x", "scopes": []string{PermRead}, "id": "abc"})
	h.mustDo(400, "POST", "/v1/projects/ACME/sprints", writer, map[string]any{"name": "s", "state": "active"})
	if got := h.mustDo(200, "GET", "/v1/issues/"+key, writer, nil); got["reporter"] != "planner-bot" || got["status"] != "todo" {
		t.Fatalf("issue changed: %v", got)
	}
}

func TestBodyLimitsAndMalformedJSON(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxBodyBytes = 2048 })
	writer := h.setup()
	big := map[string]any{"type": "task", "title": "x", "description": strings.Repeat("a", 5000)}
	if r := h.do("POST", "/v1/projects/ACME/issues", writer, big); r.Code != 413 {
		t.Fatalf("oversize body: %d %s", r.Code, r.Body)
	}
	for _, body := range []string{"{", "[]", `{"type":"task","title":"x"} {"a":1}`, `"str"`, ""} {
		if r := h.do("POST", "/v1/projects/ACME/issues", writer, body); r.Code != 400 {
			t.Errorf("%q: %d", body, r.Code)
		}
	}
	// Field-level limits come back as 422 with the offending field named.
	r := h.do("POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "task", "title": strings.Repeat("t", 300)})
	if r.Code != 422 || !strings.Contains(string(r.Body), `"field":"title"`) {
		t.Fatalf("long title: %d %s", r.Code, r.Body)
	}
}

func TestRateLimits(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RateLimit.PerMinute = 5; c.RateLimit.AuthFailuresPerMin = 3 })
	for i := 0; i < 5; i++ {
		h.mustDo(200, "GET", "/v1/me", bootstrap, nil)
	}
	r := h.do("GET", "/v1/me", bootstrap, nil)
	if r.Code != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("per-token limit: %d", r.Code)
	}
	h.clk.Advance(61 * time.Second)
	h.mustDo(200, "GET", "/v1/me", bootstrap, nil)

	// Token guessing is throttled per client address, but only the failing path: a valid
	// token from the same (shared) address keeps working, so nobody can lock others out.
	h.clk.Advance(61 * time.Second)
	for i := 0; i < 3; i++ {
		h.mustDo(401, "GET", "/v1/me", "guess-guess-guess-guess-guess", nil)
	}
	if r := h.do("GET", "/v1/me", "guess-guess-guess-guess-guess", nil); r.Code != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("auth failure limit: %d", r.Code)
	}
	h.mustDo(200, "GET", "/v1/me", bootstrap, nil)
	req := httptest.NewRequest("GET", "/v1/me", nil)
	req.RemoteAddr = "198.51.100.7:1"
	req.Header.Set("Authorization", "Bearer "+bootstrap)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("other address affected: %d", rec.Code)
	}
}

func TestETagAndIfMatch(t *testing.T) {
	h := newHarness(t, nil)
	writer := h.setup()
	r := h.do("POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "story", "title": "x"})
	if r.Code != 201 || r.Header.Get("ETag") != `"1"` || r.Header.Get("Location") != "/v1/issues/ACME-1" {
		t.Fatalf("create: %d %v", r.Code, r.Header)
	}
	if et := h.do("GET", "/v1/issues/ACME-1", writer, nil).Header.Get("ETag"); et != `"1"` {
		t.Fatalf("get etag %q", et)
	}
	up := h.do("PATCH", "/v1/issues/ACME-1", writer, map[string]any{"title": "y"}, "If-Match", `"1"`)
	if up.Code != 200 || up.Header.Get("ETag") != `"2"` {
		t.Fatalf("patch: %d %v", up.Code, up.Header)
	}
	stale := h.do("PATCH", "/v1/issues/ACME-1", writer, map[string]any{"title": "z"}, "If-Match", `"1"`)
	if stale.Code != 412 || stale.json(t)["code"] != "version_mismatch" {
		t.Fatalf("stale: %d %s", stale.Code, stale.Body)
	}
	h.mustDo(412, "PATCH", "/v1/issues/ACME-1", writer, map[string]any{"title": "z", "version": 1})
	h.mustDo(200, "PATCH", "/v1/issues/ACME-1", writer, map[string]any{"title": "z", "version": 2})
	h.mustDo(400, "PATCH", "/v1/issues/ACME-1", writer, map[string]any{"title": "q"}, "If-Match", "banana")
	h.mustDo(412, "POST", "/v1/issues/ACME-1/transitions", writer, map[string]any{"to": "in_progress"}, "If-Match", `"1"`)
	h.mustDo(200, "POST", "/v1/issues/ACME-1/transitions", writer, map[string]any{"to": "in_progress"}, "If-Match", `"3"`)
}

func TestIdempotencyOverHTTP(t *testing.T) {
	h := newHarness(t, nil)
	writer := h.setup()
	body := map[string]any{"type": "story", "title": "Plan the quarter", "priority": "P1"}
	first := h.do("POST", "/v1/projects/ACME/issues", writer, body, "Idempotency-Key", "planner-run-42-1")
	again := h.do("POST", "/v1/projects/ACME/issues", writer, body, "Idempotency-Key", "planner-run-42-1")
	if first.Code != 201 || again.Code != 200 || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("codes %d %d %v", first.Code, again.Code, again.Header)
	}
	if first.json(t)["key"] != again.json(t)["key"] {
		t.Fatal("retry created a second issue")
	}
	body["title"] = "Different"
	h.mustDo(422, "POST", "/v1/projects/ACME/issues", writer, body, "Idempotency-Key", "planner-run-42-1")
	h.mustDo(400, "POST", "/v1/projects/ACME/issues", writer, body, "Idempotency-Key", "short")
	list := h.mustDo(200, "GET", "/v1/issues", writer, nil)["items"].([]any)
	if len(list) != 1 {
		t.Fatalf("%d issues", len(list))
	}
	// Bulk create with the request-level key.
	bulk := map[string]any{"issues": []any{map[string]any{"type": "epic", "title": "E"}, map[string]any{"type": "story", "title": "S"}}}
	b1 := h.mustDo(201, "POST", "/v1/projects/ACME/issues/bulk", writer, bulk, "Idempotency-Key", "bulk-run-0001")
	h.mustDo(200, "POST", "/v1/projects/ACME/issues/bulk", writer, bulk, "Idempotency-Key", "bulk-run-0001")
	if len(b1["items"].([]any)) != 2 {
		t.Fatal("bulk items")
	}
	if n := len(h.mustDo(200, "GET", "/v1/issues", writer, nil)["items"].([]any)); n != 3 {
		t.Fatalf("bulk retry duplicated: %d", n)
	}
}

func TestSearchOverHTTPIsSafe(t *testing.T) {
	h := newHarness(t, nil)
	writer := h.setup()
	for i := 0; i < 5; i++ {
		h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "task", "title": "item", "priority": "P1"})
	}
	for _, q := range []string{
		`status:todo'; DROP TABLE issues;--`, `label:"' OR '1'='1"`, `priority:P0) OR (1=1`, `"` + strings.Repeat("a", 600) + `"`, `title:x`,
	} {
		r := h.do("GET", "/v1/issues?q="+urlEscape(q), writer, nil)
		if r.Code != 200 && r.Code != 422 {
			t.Errorf("%q: %d", q, r.Code)
		}
		if r.Code == 200 && len(r.json(t)["items"].([]any)) == 5 && !strings.HasPrefix(q, "title") {
			t.Errorf("%q matched everything: injection?", q)
		}
	}
	if n := len(h.mustDo(200, "GET", "/v1/issues", writer, nil)["items"].([]any)); n != 5 {
		t.Fatalf("table damaged: %d", n)
	}
	h.mustDo(422, "GET", "/v1/issues?sort="+urlEscape("id;DROP TABLE issues"), writer, nil)
	h.mustDo(400, "GET", "/v1/issues?cursor=!!!", writer, nil)
	page := h.mustDo(200, "GET", "/v1/issues?limit=2&sort=created", writer, nil)
	if len(page["items"].([]any)) != 2 || page["next_cursor"] == nil {
		t.Fatalf("page: %v", page)
	}
	next := h.mustDo(200, "GET", "/v1/issues?limit=2&sort=created&cursor="+page["next_cursor"].(string), writer, nil)
	if next["items"].([]any)[0].(map[string]any)["key"] != "ACME-3" {
		t.Fatalf("next page: %v", next)
	}
	if n := len(h.mustDo(200, "GET", "/v1/issues?limit=100000", writer, nil)["items"].([]any)); n != 5 {
		t.Fatalf("limit clamp: %d", n)
	}
}

func urlEscape(s string) string {
	return strings.NewReplacer("%", "%25", " ", "%20", "'", "%27", `"`, "%22", ";", "%3B", "&", "%26", "#", "%23", "+", "%2B").Replace(s)
}

func TestFullWorkflowOverHTTP(t *testing.T) {
	h := newHarness(t, nil)
	writer := h.setup()
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "epic", "title": "Launch"})
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "story", "title": "Signup", "parent": "ACME-1", "assignee": "bob@example.com", "labels": []string{"web"}, "priority": "P0"})
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "task", "title": "Pay"})
	h.mustDo(201, "POST", "/v1/issues/ACME-2/links", writer, map[string]any{"type": "blocks", "target": "ACME-3"})
	h.mustDo(409, "POST", "/v1/issues/ACME-3/links", writer, map[string]any{"type": "blocks", "target": "ACME-2"})
	h.mustDo(409, "POST", "/v1/issues/ACME-2/transitions", writer, map[string]any{"to": "done"})
	h.mustDo(200, "POST", "/v1/issues/ACME-2/transitions", writer, map[string]any{"to": "in_progress"})
	h.mustDo(201, "POST", "/v1/issues/ACME-2/comments", writer, map[string]any{"body": "started"})
	h.mustDo(201, "POST", "/v1/issues/ACME-2/attachments", writer, map[string]any{"filename": "mock.png", "size_bytes": 10})
	h.mustDo(403, "PUT", "/v1/issues/ACME-2/watchers/carol@example.com", writer, nil)
	h.mustDo(204, "PUT", "/v1/issues/ACME-2/watchers/planner-bot", writer, nil)
	h.mustDo(204, "PUT", "/v1/issues/ACME-2/watchers/carol@example.com", bootstrap, nil)
	sp := h.mustDo(201, "POST", "/v1/projects/ACME/sprints", writer, map[string]any{"name": "S1"})
	sid := int(sp["id"].(float64))
	h.mustDo(200, "PATCH", "/v1/issues/ACME-2", writer, map[string]any{"sprint_id": sid})
	h.mustDo(200, "POST", "/v1/sprints/"+itoa(sid)+"/start", writer, nil)
	board := h.mustDo(200, "GET", "/v1/projects/ACME/board", writer, nil)
	if cols := board["columns"].([]any); len(cols) != 5 || cols[1].(map[string]any)["count"].(float64) != 1 {
		t.Fatalf("board: %v", board)
	}
	h.mustDo(200, "POST", "/v1/issues/ACME-3/rank", writer, map[string]any{"position": "first"})
	d := h.mustDo(200, "GET", "/v1/issues/ACME-2", writer, nil)
	if len(d["links"].([]any)) != 1 || len(d["attachments"].([]any)) != 1 || len(d["watchers"].([]any)) < 2 {
		t.Fatalf("detail: %v", d)
	}
	acts := h.mustDo(200, "GET", "/v1/issues/ACME-2/activity", writer, nil)["items"].([]any)
	if len(acts) < 5 {
		t.Fatalf("activity: %d", len(acts))
	}
	res := h.mustDo(200, "GET", "/v1/issues?q="+urlEscape("sprint:active priority:P0 assignee:bob@example.com is:blocked"), writer, nil)
	if len(res["items"].([]any)) != 0 { // ACME-2 blocks ACME-3, it is not blocked itself
		t.Fatalf("is:blocked: %v", res)
	}
	res = h.mustDo(200, "GET", "/v1/issues?q="+urlEscape("is:blocked"), writer, nil)
	if items := res["items"].([]any); len(items) != 1 || items[0].(map[string]any)["key"] != "ACME-3" {
		t.Fatalf("blocked: %v", res)
	}
	h.mustDo(404, "GET", "/v1/issues/not-a-key", writer, nil)
	h.mustDo(404, "GET", "/v1/sprints/abc", writer, nil)
	h.mustDo(404, "DELETE", "/v1/issues/ACME-2/links/99", writer, nil)
	h.mustDo(204, "DELETE", "/v1/issues/ACME-2/links/"+itoa(int(d["links"].([]any)[0].(map[string]any)["id"].(float64))), writer, nil)
}

func itoa(n int) string { return strconv.Itoa(n) }

// ---- webhooks -------------------------------------------------------------------------

type hook struct {
	mu   sync.Mutex
	got  []hookCall
	fail int
}

type hookCall struct {
	body   []byte
	header http.Header
}

func (k *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.fail > 0 {
		k.fail--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	k.got = append(k.got, hookCall{b, r.Header.Clone()})
}

func (k *hook) calls() []hookCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]hookCall(nil), k.got...)
}

func TestWebhookEventsSignedAndRetried(t *testing.T) {
	recv := &hook{fail: 1}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	h := newHarness(t, func(c *Config) { c.Webhook.URL = srv.URL })
	writer := h.setup()
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "task", "title": "Ship it", "priority": "P1"})
	h.mustDo(200, "PATCH", "/v1/issues/ACME-1", writer, map[string]any{"priority": "P0", "labels": []string{"x"}})
	h.mustDo(200, "POST", "/v1/issues/ACME-1/transitions", writer, map[string]any{"to": "in_progress"})
	h.mustDo(201, "POST", "/v1/issues/ACME-1/comments", writer, map[string]any{"body": "secret comment text"})
	h.mustDo(200, "PATCH", "/v1/issues/ACME-1", writer, map[string]any{"title": "Ship it"}) // no change: no event

	d := h.app.dispatcher
	ctx := context.Background()
	if n, err := d.DeliverDue(ctx); err != nil || n != 3 { // the first call fails with 500, the next three succeed
		t.Fatalf("first pass: %d %v", n, err)
	}
	if n, _ := d.DeliverDue(ctx); n != 0 {
		t.Fatalf("retry before backoff elapsed delivered %d", n)
	}
	h.clk.Advance(time.Minute)
	if n, err := d.DeliverDue(ctx); err != nil || n != 1 {
		t.Fatalf("after backoff: %d %v", n, err)
	}
	calls := recv.calls()
	if len(calls) != 4 {
		t.Fatalf("%d deliveries", len(calls))
	}
	types := map[string]int{}
	for _, c := range calls {
		var ev map[string]any
		if err := json.Unmarshal(c.body, &ev); err != nil {
			t.Fatal(err)
		}
		types[ev["type"].(string)]++
		if ev["source"] != "work" || c.header.Get("X-Can-Event-Id") != ev["id"] || c.header.Get("X-Can-Event-Type") != ev["type"] {
			t.Errorf("headers/envelope: %v %v", ev, c.header)
		}
		stamp := c.header.Get("X-Can-Timestamp")
		var sec int64
		for _, ch := range stamp {
			sec = sec*10 + int64(ch-'0')
		}
		if want := Sign([]byte("whsec-0123456789abcdef"), sec, c.body); c.header.Get("X-Can-Signature") != want {
			t.Errorf("signature mismatch for %s", ev["type"])
		}
		if strings.Contains(string(c.body), "secret comment text") {
			t.Error("comment body must not travel in events")
		}
	}
	for _, want := range []string{EventIssueCreated, EventIssueUpdated, EventIssueTransition, EventCommentCreated} {
		if types[want] != 1 {
			t.Errorf("event %s delivered %d times: %v", want, types[want], types)
		}
	}
	// Delivered events are not sent again.
	h.clk.Advance(time.Hour)
	if n, _ := d.DeliverDue(ctx); n != 0 {
		t.Fatalf("redelivery of %d", n)
	}
}

func TestNoOutboxWithoutWebhook(t *testing.T) {
	h := newHarness(t, nil)
	writer := h.setup()
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "task", "title": "x"})
	var n int
	_ = h.app.store.db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n)
	if n != 0 {
		t.Fatalf("outbox grew to %d with no webhook configured", n)
	}
}

func TestWebhookNeedsSecret(t *testing.T) {
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "w.db")
	cfg.Webhook.URL = "https://hooks.example.com/x"
	if _, err := NewApp(cfg, Secrets{AdminToken: bootstrap}, Options{}); err == nil {
		t.Fatal("webhook without secret must fail to start")
	}
}

func TestConfigStrict(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "w.yaml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := LoadConfig(write("listen: ':9000'\nrate_limit: {per_minute: 10}\n"), true); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"bogus: 1\n", "max_body_bytes: 5\n", "webhook: {url: 'ftp://x'}\n", "rate_limit: {max_tracked: 1}\n", "retention_days: -1\n"} {
		if _, err := LoadConfig(write(bad), true); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.yaml"), false); err != nil {
		t.Fatalf("missing default file must fall back to defaults: %v", err)
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.yaml"), true); err == nil {
		t.Fatal("explicit missing file must fail")
	}
	if _, err := SecretsFromEnv(func(string) string { return "" }); err == nil {
		t.Fatal("admin token required")
	}
}

func TestMainCommands(t *testing.T) {
	var out, errb bytes.Buffer
	env := func(k string) string {
		if k == "CAN_WORK_ADMIN_TOKEN" {
			return bootstrap
		}
		return ""
	}
	if code := Main([]string{"version"}, env, &out, &errb); code != 0 || !strings.Contains(out.String(), "can-work") {
		t.Fatalf("version: %d %s", code, out.String())
	}
	if code := Main([]string{"check-config"}, env, &out, &errb); code != 0 {
		t.Fatalf("check-config: %d %s", code, errb.String())
	}
	if code := Main([]string{"check-config"}, func(string) string { return "" }, &out, &errb); code != 2 {
		t.Fatalf("check-config without secret: %d", code)
	}
	if code := Main([]string{"nope"}, env, &out, &errb); code != 2 {
		t.Fatalf("unknown command: %d", code)
	}
	if code := Main([]string{"healthcheck", "-addr", "127.0.0.1:1"}, env, &out, &errb); code != 1 {
		t.Fatalf("healthcheck of nothing: %d", code)
	}
}

func TestUndeliveredEventsAreRetriedThenDeadLetteredAndReplayable(t *testing.T) {
	recv := &hook{fail: 1 << 30}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	h := newHarness(t, func(c *Config) { c.Webhook.URL = srv.URL })
	writer := h.setup()
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "task", "title": "x"})
	d := h.app.dispatcher
	ctx := context.Background()
	// Retries continue well beyond the old 8-attempt (about 1 hour) cut-off, with capped backoff.
	for i := 0; i < 60; i++ {
		_, _ = d.DeliverDue(ctx)
		h.clk.Advance(7 * time.Hour)
		if i == 20 {
			var attempts int
			_ = h.app.store.db.QueryRow(`SELECT attempts FROM outbox`).Scan(&attempts)
			if attempts < 15 {
				t.Fatalf("only %d attempts after 20 backoff periods", attempts)
			}
		}
		if h.clk.Now().Sub(time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)) > 6*24*time.Hour {
			break
		}
	}
	h.mustDo(200, "GET", "/v1/events/dead", bootstrap, nil)
	if n := len(h.mustDo(200, "GET", "/v1/events/dead", bootstrap, nil)["items"].([]any)); n != 0 {
		t.Fatalf("dead too early: %d", n)
	}
	h.clk.Advance(2 * 24 * time.Hour)
	_, _ = d.DeliverDue(ctx)
	dead := h.mustDo(200, "GET", "/v1/events/dead", bootstrap, nil)["items"].([]any)
	if len(dead) != 1 {
		t.Fatalf("want one dead event, got %v", dead)
	}
	id := dead[0].(map[string]any)["event_id"].(string)
	if n, _ := d.DeliverDue(ctx); n != 0 {
		t.Fatal("dead events are not retried")
	}
	// Receiver recovers; replay delivers the very same event id.
	recv.mu.Lock()
	recv.fail = 0
	recv.mu.Unlock()
	h.mustDo(403, "POST", "/v1/events/"+id+"/replay", writer, nil)
	h.mustDo(204, "POST", "/v1/events/"+id+"/replay", bootstrap, nil)
	h.mustDo(404, "POST", "/v1/events/"+id+"/replay", bootstrap, nil)
	if n, err := d.DeliverDue(ctx); err != nil || n != 1 {
		t.Fatalf("replayed delivery: %d %v", n, err)
	}
	if got := recv.calls(); len(got) != 1 || got[0].header.Get("X-Can-Event-Id") != id {
		t.Fatalf("replayed event: %+v", got)
	}
	h.mustDo(403, "GET", "/v1/events/dead", writer, nil)
}

// Every documented operation: 401 without a token, 403 with a token one scope too low, and
// never 401 or 403 with a token that holds the documented scope.
func TestEveryOperationEnforcesItsDocumentedScope(t *testing.T) {
	raw, err := os.ReadFile("../../specs/011-work-tracker/contracts/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *Config) { c.RateLimit.PerMinute = 0; c.RateLimit.AuthFailuresPerMin = 0 })
	h.setup()
	toks := map[string]string{
		PermRead:  h.newToken("service", "r", []string{PermRead}),
		PermWrite: h.newToken("service", "w", []string{PermWrite}),
		PermAdmin: bootstrap,
	}
	lower := map[string]string{PermWrite: PermRead, PermAdmin: PermWrite}
	n := 0
	for path, item := range doc.Paths {
		for method, raw := range item {
			opm, _ := raw.(map[string]any)
			if method == "parameters" || opm["security"] != nil { // security: [] marks the public health check
				continue
			}
			op := struct{ Scope string }{Scope: fmt.Sprint(opm["x-required-scope"])}
			if opm["x-required-scope"] == nil {
				t.Errorf("%s %s has no x-required-scope", method, path)
				continue
			}
			key := "ACME-1"
			if strings.HasPrefix(path, "/v1/projects/") {
				key = "ACME"
			}
			concrete := strings.NewReplacer("{key}", key, "{id}", "1", "{subject}", "x").Replace(path)
			verb := strings.ToUpper(method)
			n++
			if r := h.do(verb, concrete, "", nil); r.Code != 401 {
				t.Errorf("%s %s without a token = %d, want 401", method, path, r.Code)
			}
			if r := h.do(verb, concrete, "cwt_0123456789abcdef_"+strings.Repeat("z", 43), nil); r.Code != 401 {
				t.Errorf("%s %s with a bogus token = %d, want 401", method, path, r.Code)
			}
			if low, ok := lower[op.Scope]; ok {
				if r := h.do(verb, concrete, toks[low], nil); r.Code != 403 {
					t.Errorf("%s %s with %s = %d, want 403", method, path, low, r.Code)
				}
			}
			if r := h.do(verb, concrete, toks[op.Scope], map[string]any{}); r.Code == 401 || r.Code == 403 {
				t.Errorf("%s %s with %s = %d, must be allowed", method, path, op.Scope, r.Code)
			}
		}
	}
	if n < 35 {
		t.Fatalf("only %d operations walked", n)
	}
}

func TestDeadEventsArePaginated(t *testing.T) {
	srv := httptest.NewServer(&hook{})
	defer srv.Close()
	h := newHarness(t, func(c *Config) { c.Webhook.URL = srv.URL })
	writer := h.setup()
	for i := 0; i < 5; i++ {
		h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, map[string]any{"type": "task", "title": "x"})
	}
	if _, err := h.app.store.db.Exec(`UPDATE outbox SET dead_at = 1`); err != nil {
		t.Fatal(err)
	}
	var seen []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		page := h.mustDo(200, "GET", "/v1/events/dead?limit=2&cursor="+cursor, bootstrap, nil)
		for _, it := range page["items"].([]any) {
			seen = append(seen, it.(map[string]any)["event_id"].(string))
		}
		next, _ := page["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 5 {
		t.Fatalf("paging saw %d of 5 dead events: %v", len(seen), seen)
	}
	h.mustDo(400, "GET", "/v1/events/dead?cursor=!!", bootstrap, nil)
}

func TestFailuresFromAnAddressOverTheLimitAreSlowedNotValidTokens(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RateLimit.AuthFailuresPerMin = 2 })
	for i := 0; i < 2; i++ {
		h.mustDo(401, "GET", "/v1/me", "bad-token-bad-token-bad-token-bad", nil)
	}
	start := time.Now()
	h.mustDo(429, "GET", "/v1/me", "bad-token-bad-token-bad-token-bad", nil)
	if time.Since(start) < 200*time.Millisecond {
		t.Fatalf("a failure from an address over the limit must cost something: %v", time.Since(start))
	}
	start = time.Now()
	h.mustDo(200, "GET", "/v1/me", bootstrap, nil)
	if time.Since(start) > 150*time.Millisecond {
		t.Fatalf("a valid token must not be delayed: %v", time.Since(start))
	}
}
