package equity

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type apiEnv struct {
	*env
	srv *httptest.Server
}

func newAPIEnv(t *testing.T) *apiEnv {
	t.Helper()
	e := newEnv(t).base()
	st := &StaticTokens{}
	st.Add("tok-hr-0000000000000000", Principal{Subject: "hr-alice", Roles: []string{RoleHR}})
	st.Add("tok-board-000000000000000", Principal{Subject: "board-bob", Roles: []string{RoleBoard}, PersonID: personBob})
	st.Add("tok-fin-00000000000000000", Principal{Subject: "fin-frank", Roles: []string{RoleFinance}})
	st.Add("tok-eve-00000000000000000", Principal{Subject: "sso-eve", Roles: []string{RoleEmployee}, PersonID: personEve})
	st.Add("tok-max-00000000000000000", Principal{Subject: "sso-max", Roles: []string{RoleEmployee, RoleManager}, PersonID: personMax})
	st.Add("tok-none-0000000000000000", Principal{Subject: "nobody", Roles: []string{"superuser"}}) // unknown roles are dropped
	api := &API{Svc: e.s, Auth: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &apiEnv{env: e, srv: srv}
}

func (a *apiEnv) do(tok, method, path string, body any, hdr ...string) (int, []byte, http.Header) {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
	}
	req, _ := http.NewRequest(method, a.srv.URL+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func TestAPIAuthAndHeaders(t *testing.T) {
	a := newAPIEnv(t)
	if code, _, _ := a.do("", "GET", "/healthz", nil); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
	for _, p := range []string{"/v1/entities", "/v1/grants/" + "grt_000000000000000000000000", "/v1/me/grants"} {
		if code, _, h := a.do("", "GET", p, nil); code != 401 || h.Get("WWW-Authenticate") == "" {
			t.Errorf("%s without a token = %d", p, code)
		}
		if code, _, _ := a.do("wrong", "GET", p, nil); code != 401 {
			t.Errorf("%s with a wrong token = %d", p, code)
		}
	}
	// a token whose roles are all unknown has no access at all
	if code, _, _ := a.do("tok-none-0000000000000000", "GET", "/v1/entities", nil); code != 403 {
		t.Errorf("unknown role = %d", code)
	}
	code, _, h := a.do("tok-hr-0000000000000000", "GET", "/v1/entities", nil)
	if code != 200 || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") == "" {
		t.Errorf("headers = %v code %d", h, code)
	}
	// strict bodies
	for _, body := range []string{`{"name":"X","currency":"USD","extra":1}`, `{"name":"X"} {"a":1}`, `not json`, ``} {
		if code, _, _ := a.do("tok-board-000000000000000", "POST", "/v1/entities", body); code != 400 {
			t.Errorf("body %q = %d", body, code)
		}
	}
	if code, _, _ := a.do("tok-board-000000000000000", "POST", "/v1/entities", strings.Repeat("x", 70<<10)); code != 400 {
		t.Errorf("oversized body = %d", code)
	}
}

func TestValidCredentialsAreNeverThrottledByBadOnes(t *testing.T) {
	a := newAPIEnv(t)
	limited := 0
	for i := 0; i < 60; i++ {
		if code, _, _ := a.do("bad-token", "GET", "/v1/entities", nil); code == 429 {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("a flood of failed authentications was never limited")
	}
	// the same client address, now with a valid token, is served
	for i := 0; i < 30; i++ {
		if code, _, _ := a.do("tok-hr-0000000000000000", "GET", "/v1/entities", nil); code != 200 {
			t.Fatalf("valid token refused after a flood of bad ones: %d", code)
		}
	}
}

func TestSelfServiceOverHTTP(t *testing.T) {
	a := newAPIEnv(t)
	gid := a.active(a.eveStk.ID, q(4800))
	a.advance("2027-01-15")
	// eve sees her own grant, vesting and identity; max (her manager) sees none of it
	if code, body, _ := a.do("tok-eve-00000000000000000", "GET", "/v1/me/grants", nil); code != 200 || !strings.Contains(string(body), gid) {
		t.Fatalf("my grants = %d %s", code, body)
	}
	if code, body, _ := a.do("tok-eve-00000000000000000", "GET", "/v1/grants/"+gid+"/vesting", nil); code != 200 || !strings.Contains(string(body), `"vested":"1200"`) {
		t.Fatalf("vesting = %d %s", code, body)
	}
	for _, p := range []string{"/v1/grants/" + gid, "/v1/grants/" + gid + "/vesting", "/v1/stakeholders/" + a.eveStk.ID, "/v1/stakeholders/" + a.eveStk.ID + "/identity"} {
		if code, _, _ := a.do("tok-max-00000000000000000", "GET", p, nil); code != 404 {
			t.Errorf("manager GET %s = %d, want 404", p, code)
		}
		if code, _, _ := a.do("tok-eve-00000000000000000", "GET", p, nil); code != 200 {
			t.Errorf("holder GET %s = %d", p, code)
		}
	}
	if code, body, _ := a.do("tok-max-00000000000000000", "GET", "/v1/me/grants", nil); code != 200 || strings.Contains(string(body), gid) {
		t.Errorf("manager's own list = %d %s", code, body)
	}
	// request an exercise, replay it with the same key, then finance approves
	req := map[string]any{"quantity": "100"}
	code, body, _ := a.do("tok-eve-00000000000000000", "POST", "/v1/grants/"+gid+"/exercises", req, "Idempotency-Key", "k-1")
	if code != 201 {
		t.Fatalf("exercise request = %d %s", code, body)
	}
	var first struct{ ID string }
	_ = json.Unmarshal(body, &first)
	code, body, _ = a.do("tok-eve-00000000000000000", "POST", "/v1/grants/"+gid+"/exercises", req, "Idempotency-Key", "k-1")
	var again struct{ ID string }
	_ = json.Unmarshal(body, &again)
	if code != 200 || again.ID != first.ID {
		t.Fatalf("replay = %d %s", code, body)
	}
	if code, _, _ := a.do("tok-eve-00000000000000000", "POST", "/v1/grants/"+gid+"/exercises", map[string]any{"quantity": "101"}, "Idempotency-Key", "k-1"); code != 409 {
		t.Fatalf("key reuse = %d", code)
	}
	if code, _, _ := a.do("tok-eve-00000000000000000", "POST", "/v1/exercises/"+first.ID+"/approve", nil); code != 403 {
		t.Fatalf("holder approving = %d", code)
	}
	if code, _, _ := a.do("tok-fin-00000000000000000", "POST", "/v1/exercises/"+first.ID+"/approve", nil); code != 200 {
		t.Fatalf("finance approving = %d", code)
	}
	if code, body, _ := a.do("tok-eve-00000000000000000", "GET", "/v1/me/exercises", nil); code != 200 || !strings.Contains(string(body), `"status":"approved"`) {
		t.Fatalf("my exercises = %d %s", code, body)
	}
	// board approval over HTTP, and SoD as a 403 with its own code
	id := a.draft(a.hr, a.eveStk.ID, q(10))
	if code, _, _ := a.do("tok-hr-0000000000000000", "POST", "/v1/grants/"+id+"/submit", nil); code != 200 {
		t.Fatal("submit")
	}
	if code, _, _ := a.do("tok-hr-0000000000000000", "POST", "/v1/grants/"+id+"/approve", nil); code != 403 {
		t.Fatalf("hr approving = %d", code)
	}
	if code, _, _ := a.do("tok-board-000000000000000", "POST", "/v1/grants/"+id+"/approve", nil); code != 200 {
		t.Fatal("board approve")
	}
	// CSV
	code, body, h := a.do("tok-fin-00000000000000000", "GET", "/v1/entities/"+a.ent.ID+"/exports/grants", nil)
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/csv") || !strings.HasPrefix(string(body), "grant_id,holder") {
		t.Fatalf("export = %d %s %s", code, h.Get("Content-Type"), body)
	}
	if code, _, _ := a.do("tok-eve-00000000000000000", "GET", "/v1/entities/"+a.ent.ID+"/exports/grants", nil); code != 403 {
		t.Fatalf("employee export = %d", code)
	}
	// malformed ids look like missing records
	if code, _, _ := a.do("tok-hr-0000000000000000", "GET", "/v1/grants/not-an-id", nil); code != 404 {
		t.Fatalf("bad id = %d", code)
	}
	// quantities are exact decimal strings, accepted as numbers too
	if code, _, _ := a.do("tok-fin-00000000000000000", "POST", "/v1/entities/"+a.ent.ID+"/ledger/issuances",
		`{"class_id":"`+a.class.ID+`","stakeholder_id":"`+a.founder.ID+`","quantity":1000.5,"effective_date":"2026-01-01"}`); code != 201 {
		t.Fatalf("issuance with a numeric quantity = %d", code)
	}
}

func TestIntrospectionCarriesPersonID(t *testing.T) {
	var calls atomic.Int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var in struct{ Token string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch in.Token {
		case "good":
			_, _ = w.Write([]byte(`{"active":true,"subject":"sso-eve","roles":["employee","wizard"],"person_id":"` + personEve + `"}`))
		case "badperson":
			_, _ = w.Write([]byte(`{"active":true,"subject":"sso-eve","roles":["employee"],"person_id":"../../etc"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer idp.Close()
	now := time.Now()
	au := &IntrospectAuthenticator{URL: idp.URL, TTL: time.Minute, Now: func() time.Time { return now }}
	req := func(tok string) *http.Request {
		r, _ := http.NewRequest("GET", "http://x/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return r
	}
	p, err := au.Authenticate(req("good"))
	if err != nil || p.PersonID != personEve || len(p.Roles) != 1 || p.Roles[0] != RoleEmployee {
		t.Fatalf("principal = %+v, %v", p, err)
	}
	if _, err := au.Authenticate(req("badperson")); err == nil {
		t.Fatal("a malformed person id was trusted")
	}
	if _, err := au.Authenticate(req("nope")); err == nil {
		t.Fatal("an unknown token authenticated")
	}
	before := calls.Load()
	_, _ = au.Authenticate(req("good"))
	_, _ = au.Authenticate(req("nope"))
	if calls.Load() != before {
		t.Fatal("answers were not cached")
	}
}

func TestSigningMatchesTheSharedScheme(t *testing.T) {
	got := Sign([]byte("secret"), 1700000000, []byte(`{"a":1}`))
	if !strings.HasPrefix(got, "sha256=") || len(got) != len("sha256=")+64 {
		t.Fatalf("signature = %q", got)
	}
	if Sign([]byte("secret"), 1700000001, []byte(`{"a":1}`)) == got {
		t.Fatal("timestamp not covered")
	}
}

func TestWebhookDelivery(t *testing.T) {
	var got atomic.Int32
	var lastSig, lastType atomic.Value
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		lastSig.Store(r.Header.Get("X-Can-Signature"))
		lastType.Store(r.Header.Get("X-Can-Event-Type"))
		w.WriteHeader(204)
	}))
	defer hook.Close()
	e := newEnv(t).base()
	e.approved(e.eveStk.ID, q(10))
	d := NewDispatcher(hook.URL, []byte("0123456789abcdef"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	n, err := e.s.DeliverPending(e.ctx, d)
	if err != nil || n != 1 || got.Load() != 1 || lastType.Load() != EventGrantApproved || !strings.HasPrefix(lastSig.Load().(string), "sha256=") {
		t.Fatalf("delivered %d, %v, hits %d", n, err, got.Load())
	}
	if n, _ := e.s.DeliverPending(e.ctx, d); n != 0 {
		t.Fatal("delivered twice")
	}
}

func TestMainCommands(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.WriteFile(keys, []byte("k1 "+hexKeyA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "equity.yaml")
	if err := os.WriteFile(cfg, []byte("db_path: "+filepath.Join(dir, "e.db")+"\nkeyring_file: "+keys+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(string) string { return "" }
	run := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		c := Main(append(args, "-config", cfg), env, &o, &e)
		return c, o.String(), e.String()
	}
	if c, o, _ := run("check-config"); c != 0 || !strings.Contains(o, "config ok") {
		t.Errorf("check-config: %d %s", c, o)
	}
	if c, o, _ := run("version"); c != 0 || !strings.Contains(o, "can-equity") {
		t.Errorf("version: %d %s", c, o)
	}
	if c, _, _ := run("bogus"); c != 2 {
		t.Errorf("unknown command: %d", c)
	}
	if c, o, _ := run("rotate-keys"); c != 0 || !strings.Contains(o, "re-sealed 0") {
		t.Errorf("rotate-keys: %d %s", c, o)
	}
	if c, o, _ := run("verify-audit"); c != 0 || !strings.Contains(o, "ok") {
		t.Errorf("verify-audit: %d %s", c, o)
	}
	if c, o, _ := run("retention"); c != 0 || !strings.Contains(o, "purged") {
		t.Errorf("retention: %d %s", c, o)
	}
	if c, _, _ := run("purge"); c != 2 {
		t.Errorf("purge without args: %d", c)
	}
	if c, _, e := run("purge", "-subject", "stk_000000000000000000000000", "-class", "all"); c != 0 {
		t.Errorf("purge of an unknown subject should be a no-op: %d %s", c, e)
	}
	if c, _, _ := run("purge", "-subject", "bad id", "-class", "all"); c != 1 {
		t.Errorf("purge bad subject: %d", c)
	}
	if err := os.Chmod(keys, 0o644); err != nil {
		t.Fatal(err)
	}
	if c, _, e := run("rotate-keys"); c != 2 || !strings.Contains(e, "keys:") {
		t.Errorf("open key permissions: %d %s", c, e)
	}
	if c, _, _ := run("serve"); c != 2 {
		t.Errorf("serve with open keys: %d", c)
	}
}

func TestConfigRules(t *testing.T) {
	for _, bad := range []string{
		"", "listen: :1\n", "keyring_file: /k\nbogus: 1\n", "keyring_file: /k\nwebhook:\n  url: ftp://x\n",
		"keyring_file: /k\nintrospect:\n  url: http://idp.example.test/x\n", "keyring_file: /k\nretention:\n  nonsense: 1y\n", "keyring_file: /k\nretention:\n  equity-grants: forever\n",
	} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("config accepted: %q", bad)
		}
	}
	if _, err := ParseConfig([]byte("keyring_file: /k\nintrospect:\n  url: http://127.0.0.1:9/x\n")); err != nil {
		t.Errorf("loopback http introspection: %v", err)
	}
	if _, err := SecretsFromEnv(func(string) string { return "short" }, &Config{}); err == nil {
		t.Error("a short API token was accepted")
	}
	if _, err := SecretsFromEnv(func(k string) string {
		if k == "CAN_EQUITY_API_TOKEN" {
			return strings.Repeat("a", 24)
		}
		return ""
	}, &Config{}); err != nil {
		t.Errorf("valid secrets: %v", err)
	}
}
