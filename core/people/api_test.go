package people

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
	"testing"
	"time"
)

type apiEnv struct {
	*env
	srv *httptest.Server
	st  *StaticTokens
}

func newAPIEnv(t *testing.T) *apiEnv {
	e := newEnv(t)
	st := &StaticTokens{}
	st.Add("tok-hr-service-account-0001", Principal{Subject: "svc-hr", Roles: []string{RoleHR, RoleAdmin}})
	api := &API{Svc: e.s, Auth: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &apiEnv{env: e, srv: srv, st: st}
}

// token registers a user token for the subject of actor a.
func (e *apiEnv) token(name string, a Actor) string {
	tok := "tok-" + name + "-padding-padding"
	e.st.Add(tok, Principal{Subject: a.Subject, Roles: a.Roles})
	return tok
}

func (e *apiEnv) do(tok, method, path string, body any) (int, map[string]any, string) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m, string(b)
}

const hrTok = "tok-hr-service-account-0001"

func TestAPIAuthenticationAndHeaders(t *testing.T) {
	e := newAPIEnv(t)
	for name, tok := range map[string]string{"none": "", "wrong": "nope", "empty-ish": " "} {
		if code, _, _ := e.do(tok, "GET", "/v1/people", nil); code != 401 {
			t.Errorf("%s token: %d", name, code)
		}
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/people", nil)
	req.Header.Set("Authorization", "Basic "+hrTok)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Errorf("basic scheme accepted: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	// healthz needs no credentials and carries the hardening headers.
	resp, err := http.Get(e.srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	for h, want := range map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"} {
		if resp.Header.Get(h) != want {
			t.Errorf("%s = %q", h, resp.Header.Get(h))
		}
	}
	// A valid token with no recognised role gets nothing.
	e.st.Add("tok-norole-padding-padding", Principal{Subject: "x", Roles: []string{"superuser"}})
	if code, _, _ := e.do("tok-norole-padding-padding", "GET", "/v1/people", nil); code != 200 {
		t.Errorf("list for roleless caller should succeed with an empty page, got %d", code)
	}
	if _, m, _ := e.do("tok-norole-padding-padding", "GET", "/v1/people", nil); len(m["items"].([]any)) != 0 {
		t.Error("roleless caller saw people")
	}
}

func TestAPIEndToEndFlow(t *testing.T) {
	e := newAPIEnv(t)
	// hr creates the org and two people
	code, dep, _ := e.do(hrTok, "POST", "/v1/departments", map[string]any{"name": "Engineering"})
	if code != 201 {
		t.Fatal(code, dep)
	}
	code, boss, _ := e.do(hrTok, "POST", "/v1/people", map[string]any{"kind": "employee", "status": "active", "given_name": "Boss", "family_name": "Example", "sso_subject": "sso-boss", "department_id": dep["id"]})
	if code != 201 {
		t.Fatal(code, boss)
	}
	code, rae, _ := e.do(hrTok, "POST", "/v1/people", map[string]any{"kind": "employee", "status": "active", "given_name": "Rae", "family_name": "Example", "sso_subject": "sso-rae", "manager_id": boss["id"]})
	if code != 201 {
		t.Fatal(code, rae)
	}
	bossTok := e.token("boss", Actor{Subject: "sso-boss", Roles: []string{RoleEmployee, RoleManager}})
	raeTok := e.token("rae", Actor{Subject: "sso-rae", Roles: []string{RoleEmployee}})

	// sensitive: hr writes, rae reads, boss gets 403, unknown id 404
	if code, _, b := e.do(hrTok, "PUT", "/v1/people/"+rae["id"].(string)+"/sensitive", map[string]any{"salary": map[string]any{"amount": 7000000, "currency": "USD", "period": "year"}, "date_of_birth": "1993-03-03"}); code != 204 {
		t.Fatal(code, b)
	}
	if code, m, _ := e.do(raeTok, "GET", "/v1/people/"+rae["id"].(string)+"/sensitive?fields=salary", nil); code != 200 || m["salary"].(map[string]any)["currency"] != "USD" {
		t.Errorf("self sensitive: %d %v", code, m)
	}
	if code, _, b := e.do(bossTok, "GET", "/v1/people/"+rae["id"].(string)+"/sensitive", nil); code != 403 || strings.Contains(b, "USD") {
		t.Errorf("manager sensitive: %d %s", code, b)
	}
	if code, _, _ := e.do(raeTok, "GET", "/v1/people/"+boss["id"].(string)+"/sensitive", nil); code != 403 {
		t.Errorf("peer-ish sensitive: %d", code)
	}
	if code, _, _ := e.do(raeTok, "GET", "/v1/people/pers_aaaaaaaaaaaaaaaaaaaaaaaa", nil); code != 404 {
		t.Errorf("unknown id: %d", code)
	}
	// hr-only fields are not echoed to non-hr callers
	_, m, _ := e.do(raeTok, "GET", "/v1/people/"+boss["id"].(string), nil)
	if _, ok := m["sso_subject"]; ok {
		t.Error("sso_subject leaked")
	}
	// strict decoding and status codes
	if code, _, _ := e.do(hrTok, "POST", "/v1/people", map[string]any{"kind": "employee", "given_name": "A", "family_name": "B", "is_admin": true}); code != 400 {
		t.Errorf("unknown field: %d", code)
	}
	if code, _, _ := e.do(hrTok, "PUT", "/v1/people/"+boss["id"].(string)+"/manager", map[string]any{"manager_id": rae["id"]}); code != 400 {
		t.Errorf("cycle: %d", code)
	}
	big := strings.Repeat("x", maxBody+10)
	if code, _, _ := e.do(hrTok, "POST", "/v1/departments", map[string]any{"name": big}); code != 400 {
		t.Errorf("oversized body: %d", code)
	}
	if code, _, _ := e.do(raeTok, "POST", "/v1/people", map[string]any{"kind": "employee", "given_name": "A", "family_name": "B"}); code != 403 {
		t.Errorf("employee create: %d", code)
	}

	// 1:1 over HTTP: private notes never reach the report
	code, ser, _ := e.do(bossTok, "POST", "/v1/oneonones", map[string]any{"report_id": rae["id"], "cadence": "weekly"})
	if code != 201 {
		t.Fatal(code, ser)
	}
	code, mtg, _ := e.do(raeTok, "POST", "/v1/oneonones/"+ser["id"].(string)+"/meetings", map[string]any{"scheduled_at": e.clock.Add(24 * time.Hour).Format(time.RFC3339)})
	if code != 201 {
		t.Fatal(code, mtg)
	}
	mid := mtg["id"].(string)
	if code, _, _ := e.do(bossTok, "PUT", "/v1/meetings/"+mid+"/private-notes", map[string]any{"text": "performance concern"}); code != 204 {
		t.Fatal(code)
	}
	if code, _, _ := e.do(raeTok, "PUT", "/v1/meetings/"+mid+"/private-notes", map[string]any{"text": "x"}); code != 404 {
		t.Errorf("report private write: %d", code)
	}
	if code, _, b := e.do(raeTok, "GET", "/v1/meetings/"+mid, nil); code != 200 || strings.Contains(b, "concern") {
		t.Errorf("report view: %d %s", code, b)
	}
	if code, _, b := e.do(bossTok, "GET", "/v1/meetings/"+mid, nil); code != 200 || !strings.Contains(b, "performance concern") {
		t.Errorf("manager view: %d %s", code, b)
	}
	if code, _, _ := e.do(hrTok, "GET", "/v1/meetings/"+mid, nil); code != 404 {
		t.Errorf("service admin 1:1 read: %d", code)
	}
	// retention and erasure endpoints are hr/admin only
	if code, _, _ := e.do(bossTok, "POST", "/v1/retention/run", nil); code != 403 {
		t.Errorf("manager retention: %d", code)
	}
	if code, _, _ := e.do(hrTok, "POST", "/v1/retention/run", nil); code != 200 {
		t.Errorf("hr retention: %d", code)
	}
	if code, _, _ := e.do(bossTok, "GET", "/v1/erasures", nil); code != 403 {
		t.Errorf("manager erasures: %d", code)
	}
	if code, _, _ := e.do(hrTok, "POST", "/v1/people/"+rae["id"].(string)+"/erasure", nil); code != 409 {
		t.Errorf("erasing an active person: %d", code)
	}
	// goals over HTTP
	code, g, _ := e.do(raeTok, "POST", "/v1/goals", map[string]any{"scope": "person", "title": "Learn Go"})
	if code != 201 || g["visibility"] != "manager" {
		t.Fatal(code, g)
	}
	if code, _, _ := e.do(bossTok, "GET", "/v1/goals/"+g["id"].(string), nil); code != 200 {
		t.Errorf("manager reads goal: %d", code)
	}
	if code, _, _ := e.do(bossTok, "POST", "/v1/goals", map[string]any{"scope": "person", "owner_id": rae["id"], "title": "x", "visibility": "bogus"}); code != 400 {
		t.Errorf("bad visibility: %d", code)
	}
}

func TestIntrospectAuthenticator(t *testing.T) {
	calls := 0
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer svc-credential" {
			w.WriteHeader(401)
			return
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch in["token"] {
		case "good":
			_, _ = w.Write([]byte(`{"active":true,"subject":"sso-rae","roles":["employee","root","manager"]}`))
		case "inactive":
			_, _ = w.Write([]byte(`{"active":false,"subject":"sso-rae","roles":["admin"]}`))
		case "nosubject":
			_, _ = w.Write([]byte(`{"active":true,"roles":["admin"]}`))
		default:
			_, _ = w.Write([]byte(`garbage`))
		}
	}))
	defer idp.Close()
	now := time.Now()
	a := &IntrospectAuthenticator{URL: idp.URL, Token: "svc-credential", TTL: time.Minute, Now: func() time.Time { return now }}
	mk := func(tok string) *http.Request {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return r
	}
	p, err := a.Authenticate(mk("good"))
	if err != nil || p.Subject != "sso-rae" || strings.Join(p.Roles, ",") != "employee,manager" {
		t.Fatalf("good: %+v %v", p, err)
	}
	if _, err := a.Authenticate(mk("good")); err != nil || calls != 1 {
		t.Errorf("not cached: calls=%d", calls)
	}
	now = now.Add(2 * time.Minute)
	if _, _ = a.Authenticate(mk("good")); calls != 2 {
		t.Error("cache did not expire")
	}
	for _, tok := range []string{"inactive", "nosubject", "other"} {
		if _, err := a.Authenticate(mk(tok)); err == nil {
			t.Errorf("%s accepted", tok)
		}
	}
	// IdP down: fail closed, even for a previously valid token whose cache expired.
	idp.Close()
	now = now.Add(2 * time.Minute)
	if _, err := a.Authenticate(mk("good")); err == nil {
		t.Error("accepted while the introspection endpoint is down")
	}
}

func TestDispatcherSignsAndRetries(t *testing.T) {
	e := newEnv(t)
	var got []string
	var sigs, tss []string
	fail := true
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if fail {
			w.WriteHeader(500)
			return
		}
		got = append(got, string(b))
		sigs = append(sigs, r.Header.Get("X-Can-Signature"))
		tss = append(tss, r.Header.Get("X-Can-Timestamp"))
	}))
	defer rcv.Close()
	_, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "contractor", GivenName: "Con", FamilyName: "Tractor"})
	e.must(err)
	d := NewDispatcher(rcv.URL, []byte("0123456789abcdef"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n, err := e.s.DeliverPending(e.ctx, d); err != nil || n != 0 {
		t.Fatalf("first attempt = %d, %v", n, err)
	}
	if n, _ := e.s.DeliverPending(e.ctx, d); n != 0 {
		t.Error("retried before the backoff elapsed")
	}
	*e.clock = e.clock.Add(time.Minute)
	fail = false
	n, err := e.s.DeliverPending(e.ctx, d)
	if err != nil || n != 1 || len(got) != 1 {
		t.Fatalf("retry = %d, %v", n, err)
	}
	if want := Sign([]byte("0123456789abcdef"), e.clock.Unix(), []byte(got[0])); sigs[0] != want || tss[0] == "" {
		t.Errorf("signature %q want %q", sigs[0], want)
	}
	if strings.Contains(got[0], "Tractor") {
		t.Error("payload carries a name")
	}
	if p, _ := e.s.PendingEvents(e.ctx); len(p) != 0 {
		t.Errorf("%d events still pending", len(p))
	}
}

func TestConfigValidation(t *testing.T) {
	ok := "keyring_file: /etc/keys\nretention:\n  people-sensitive: 5y\n  performance-goals: legal\n"
	c, err := ParseConfig([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8081" || c.DBPath != "/data/people.db" {
		t.Errorf("defaults: %+v", c)
	}
	rules, err := c.RetentionRules()
	if err != nil || rules[ClassSensitive].Days != 5*365 || !rules[ClassGoals].Legal || rules[ClassBasic].Days != 7*365 {
		t.Errorf("rules: %+v %v", rules, err)
	}
	for name, in := range map[string]string{
		"no keyring":     "listen: :1\n",
		"unknown key":    ok + "bogus: 1\n",
		"bad class":      "keyring_file: /k\nretention:\n  salaries: 1y\n",
		"bad retention":  "keyring_file: /k\nretention:\n  people-sensitive: forever\n",
		"webhook creds":  "keyring_file: /k\nwebhook:\n  url: http://u:p@host/x\n",
		"webhook scheme": "keyring_file: /k\nwebhook:\n  url: ftp://host/x\n",
	} {
		if _, err := ParseConfig([]byte(in)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := SecretsFromEnv(env(map[string]string{"CAN_PEOPLE_API_TOKEN": "short"}), c); err == nil {
		t.Error("short api token accepted")
	}
	c.Webhook.URL = "http://x/y"
	if _, err := SecretsFromEnv(env(map[string]string{"CAN_PEOPLE_API_TOKEN": strings.Repeat("a", 24)}), c); err == nil {
		t.Error("webhook without secret accepted")
	}
}

func TestMainCommands(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.WriteFile(keys, []byte("k1 "+hexKeyA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "people.yaml")
	if err := os.WriteFile(cfg, []byte("db_path: "+filepath.Join(dir, "p.db")+"\nkeyring_file: "+keys+"\n"), 0o600); err != nil {
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
	if c, o, _ := run("version"); c != 0 || !strings.Contains(o, "can-people") {
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
	if c, _, _ := run("purge"); c != 2 {
		t.Errorf("purge without args: %d", c)
	}
	if c, _, e := run("purge", "-subject", "pers_000000000000000000000000", "-class", "all"); c != 0 {
		t.Errorf("purge unknown subject should be a no-op: %d %s", c, e)
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
