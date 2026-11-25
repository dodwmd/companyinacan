package adminapi

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
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func init() { auth.TestingParams() }

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	db     *store.DB
	secret string
	pw     string
	now    *time.Time
	logs   *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Unix(1_700_000_000, 0)
	db.Now = func() time.Time { return now }
	h := &harness{t: t, db: db, pw: "correct horse battery staple", secret: auth.NewTOTPSecret(), now: &now, logs: &bytes.Buffer{}}
	hash, _ := auth.HashPassword(h.pw)
	if _, err := db.CreateAdmin(ctx, "root", hash, h.secret); err != nil {
		t.Fatal(err)
	}
	al := audit.New(db)
	s := &Server{
		Auth: auth.NewService(db, al, auth.Options{Now: func() time.Time { return now }}),
		DB:   db, Audit: al, Registry: registry.New(db, "http://can-core"),
		Log: slog.New(slog.NewJSONHandler(h.logs, nil)), Secure: true, Now: func() time.Time { return now },
	}
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) code() string {
	k, _ := auth.DecodeTOTPSecret(h.secret)
	return auth.Code(k, auth.SHA1, 6, 30, *h.now)
}

func (h *harness) do(method, path string, body any, mut ...func(*http.Request)) (*http.Response, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, m := range mut {
		m(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

type loggedIn struct {
	cookie *http.Cookie
	csrf   string
}

func (h *harness) login() loggedIn {
	h.t.Helper()
	*h.now = h.now.Add(31 * time.Second) // fresh TOTP step each time
	resp, b := h.do("POST", "/api/v1/login", map[string]string{"username": "root", "password": h.pw, "totp": h.code()})
	if resp.StatusCode != 200 {
		h.t.Fatalf("login: %d %s", resp.StatusCode, b)
	}
	var v sessionView
	_ = json.Unmarshal(b, &v)
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieName {
			return loggedIn{c, v.CSRFToken}
		}
	}
	h.t.Fatal("no session cookie")
	return loggedIn{}
}

func (l loggedIn) with(csrf bool) func(*http.Request) {
	return func(r *http.Request) {
		r.AddCookie(l.cookie)
		if csrf {
			r.Header.Set("X-CSRF-Token", l.csrf)
		}
	}
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	h := newHarness(t)
	resp, b := h.do("GET", "/healthz", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "ok") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("%v", resp.Header)
	}
}

func TestEveryProtectedRouteRejectsAnonymous(t *testing.T) {
	h := newHarness(t)
	for _, p := range Routes() {
		method, path, _ := strings.Cut(p, " ")
		if path == "/healthz" || path == "/api/v1/login" {
			continue
		}
		path = strings.ReplaceAll(strings.ReplaceAll(path, "{id}", "1"), "{name}", "x")
		resp, _ := h.do(method, path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s -> %d, want 401", p, resp.StatusCode)
		}
	}
}

func TestLoginCookieAttributesAndSession(t *testing.T) {
	h := newHarness(t)
	l := h.login()
	if !l.cookie.HttpOnly || !l.cookie.Secure || l.cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", l.cookie)
	}
	resp, b := h.do("GET", "/api/v1/session", nil, l.with(false))
	var v sessionView
	_ = json.Unmarshal(b, &v)
	if resp.StatusCode != 200 || v.Username != "root" || v.CSRFToken != l.csrf {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestLoginFailuresAreUniform(t *testing.T) {
	h := newHarness(t)
	for _, body := range []map[string]string{
		{"username": "root", "password": "wrong-password-xx", "totp": h.code()},
		{"username": "nobody", "password": h.pw, "totp": h.code()},
		{"username": "root", "password": h.pw, "totp": "000000"},
	} {
		resp, b := h.do("POST", "/api/v1/login", body)
		if resp.StatusCode != 401 || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
		if strings.Contains(string(b), "password is") || strings.Contains(string(b), "no such user") {
			t.Fatalf("response distinguishes the failure: %s", b)
		}
	}
}

func TestLoginLockoutReturns429WithRetryAfter(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < auth.DefaultPolicy.MaxUserFailures; i++ {
		h.do("POST", "/api/v1/login", map[string]string{"username": "root", "password": "wrong-password-xx", "totp": "000000"})
	}
	resp, _ := h.do("POST", "/api/v1/login", map[string]string{"username": "root", "password": h.pw, "totp": h.code()})
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
}

func TestLoginRequiresJSONAndRejectsUnknownFields(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/login", strings.NewReader("username=root&password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("form post accepted: %d", resp.StatusCode)
	}
	resp2, _ := h.do("POST", "/api/v1/login", map[string]string{"username": "root", "password": "x", "totp": "1", "admin": "true"})
	if resp2.StatusCode != 400 {
		t.Fatalf("unknown field accepted: %d", resp2.StatusCode)
	}
}

func TestCSRFRequiredOnUnsafeMethods(t *testing.T) {
	h := newHarness(t)
	l := h.login()
	if resp, _ := h.do("POST", "/api/v1/logout", nil, l.with(false)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without CSRF: %d", resp.StatusCode)
	}
	if resp, _ := h.do("POST", "/api/v1/logout", nil, l.with(false), func(r *http.Request) { r.Header.Set("X-CSRF-Token", "wrong") }); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logout with wrong CSRF: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/api/v1/session", nil, l.with(false)); resp.StatusCode != 200 {
		t.Fatal("session destroyed by a rejected request")
	}
	resp, _ := h.do("POST", "/api/v1/logout", nil, l.with(true))
	if resp.StatusCode != 204 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/api/v1/session", nil, l.with(false)); resp.StatusCode != 401 {
		t.Fatalf("session survived logout: %d", resp.StatusCode)
	}
}

func TestModulesViewNeverExposesSecrets(t *testing.T) {
	h := newHarness(t)
	m := store.Module{ID: "crm", Version: "1.0.0", Namespace: "mod-crm", ServiceAccount: "crm", State: store.StateEnabled, HMACSecret: "TOP-SECRET-HMAC", Manifest: config.Module{ID: "crm", Name: "CRM"}}
	if err := h.db.UpsertModule(context.Background(), m, []store.Provider{{Capability: "contacts", ModuleID: "crm", Service: "crm", Port: 80}}, nil); err != nil {
		t.Fatal(err)
	}
	l := h.login()
	resp, b := h.do("GET", "/api/v1/modules", nil, l.with(false))
	if resp.StatusCode != 200 || strings.Contains(string(b), "TOP-SECRET-HMAC") || !strings.Contains(string(b), `"id":"crm"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if resp, _ := h.do("GET", "/api/v1/modules/nope", nil, l.with(false)); resp.StatusCode != 404 {
		t.Fatalf("%d", resp.StatusCode)
	}
	resp, b = h.do("GET", "/api/v1/modules/crm", nil, l.with(false))
	if resp.StatusCode != 200 || strings.Contains(string(b), "TOP-SECRET") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	resp, b = h.do("GET", "/api/v1/capabilities", nil, l.with(false))
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"capability":"contacts"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestAuditListAndVerifyEndpoints(t *testing.T) {
	h := newHarness(t)
	l := h.login() // writes an admin.login audit entry
	resp, b := h.do("GET", "/api/v1/audit?limit=10", nil, l.with(false))
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"action":"admin.login"`) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	resp, b = h.do("GET", "/api/v1/audit/verify", nil, l.with(false))
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"entries":1`) || strings.Contains(string(b), "violation") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if _, err := h.db.Exec(`UPDATE audit_log SET actor='mallory' WHERE seq=1`); err != nil {
		t.Fatal(err)
	}
	_, b = h.do("GET", "/api/v1/audit/verify", nil, l.with(false))
	if !strings.Contains(string(b), `"kind":"hash-mismatch"`) {
		t.Fatalf("tampering not reported: %s", b)
	}
	if resp, _ := h.do("GET", "/api/v1/audit/verify?head=bogus", nil, l.with(false)); resp.StatusCode != 400 {
		t.Fatalf("bad head: %d", resp.StatusCode)
	}
}

func TestDeliveryLogEndpoints(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_ = h.db.UpsertModule(ctx, store.Module{ID: "crm", Version: "1", Namespace: "mod-crm", ServiceAccount: "crm", State: store.StateEnabled, HMACSecret: "s"}, nil, nil)
	_ = h.db.InsertEvent(ctx, store.Event{ID: "e1", Type: "lead.created.v1", Time: *h.now, Source: "forms", DataJSON: "{}"}, []store.DeliveryTarget{{ModuleID: "crm", URL: "http://x"}})
	due, _ := h.db.DueDeliveries(ctx, *h.now, 5)
	_ = h.db.RecordAttempt(ctx, due[0].ID, store.Attempt{At: *h.now, StatusCode: 502, Error: "bad gateway"}, store.StatusPending, h.now.Add(time.Minute))
	l := h.login()
	_, b := h.do("GET", "/api/v1/deliveries", nil, l.with(false))
	if !strings.Contains(string(b), `"event_type":"lead.created.v1"`) || !strings.Contains(string(b), `"attempts":1`) {
		t.Fatalf("%s", b)
	}
	_, b = h.do("GET", "/api/v1/deliveries/1/attempts", nil, l.with(false))
	if !strings.Contains(string(b), `"status_code":502`) {
		t.Fatalf("%s", b)
	}
	if resp, _ := h.do("GET", "/api/v1/deliveries/abc/attempts", nil, l.with(false)); resp.StatusCode != 400 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestLogsNeverContainCredentials(t *testing.T) {
	h := newHarness(t)
	l := h.login()
	h.do("GET", "/api/v1/session", nil, l.with(false))
	out := h.logs.String()
	for _, secret := range []string{l.cookie.Value, h.pw, l.csrf} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaks a credential: %s", out)
		}
	}
	if !strings.Contains(out, `"route":"GET /api/v1/session"`) || !strings.Contains(out, `"status":200`) {
		t.Fatalf("structured access log missing: %s", out)
	}
}

func TestUnknownRouteAndPanicRecovery(t *testing.T) {
	h := newHarness(t)
	if resp, _ := h.do("GET", "/nope", nil); resp.StatusCode != 404 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if resp, _ := h.do("DELETE", "/api/v1/modules", nil); resp.StatusCode != 405 {
		t.Fatalf("%d", resp.StatusCode)
	}
}
