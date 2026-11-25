package adminui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func concrete(p string) string {
	r := strings.NewReplacer("{$}", "", "{id}", "1", "{sid}", "aaaaaaaaaaaaaaaa", "{file}", "admin.css")
	return r.Replace(p)
}

func split(route string) (string, string) {
	m, p, _ := strings.Cut(route, " ")
	return m, concrete(p)
}

var publicRoutes = map[string]bool{"GET /auth/oidc/login": true, "GET /auth/oidc/callback": true, "GET /login": true, "POST /login": true, "GET /static/{file}": true, "GET /manifest.webmanifest": true, "GET /sw.js": true}

func TestEveryPageRequiresSession(t *testing.T) {
	h := newHarness(t)
	for _, route := range Routes() {
		if publicRoutes[route] {
			continue
		}
		method, path := split(route)
		var form url.Values
		if method == "POST" {
			form = url.Values{"csrf": {"anything"}}
		}
		r := h.do(method, path, form)
		if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
			t.Errorf("%s anonymous -> %d %q, want 303 /login", route, r.StatusCode, r.Header.Get("Location"))
		}
		if strings.Contains(r.body, "Acme") {
			t.Errorf("%s leaked content to an anonymous caller", route)
		}
	}
	// A forged cookie is as good as none.
	r := h.do("GET", "/modules", nil, &http.Cookie{Name: "can_session", Value: "forged"})
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("forged cookie -> %d", r.StatusCode)
	}
	if c := cookieNamed(r.Response, "can_session"); c == nil || c.MaxAge >= 0 {
		t.Fatalf("stale cookie not cleared: %+v", c)
	}
}

func TestEveryPostRequiresCSRFAndSameSite(t *testing.T) {
	h := newHarness(t)
	h.seed()
	dead := h.killDelivery()
	s := h.login()
	for _, route := range Routes() {
		method, path := split(route)
		if method != "POST" || publicRoutes[route] || route == "POST /logout" {
			continue
		}
		path = strings.ReplaceAll(path, "/1/", "/"+itoa(dead)+"/")
		for name, form := range map[string]url.Values{"missing": {}, "wrong": {"csrf": {"nope"}}} {
			if r := h.do(method, path, form, s.with()...); r.StatusCode != http.StatusForbidden {
				t.Errorf("%s with %s csrf -> %d, want 403", route, name, r.StatusCode)
			}
		}
		req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(url.Values{"csrf": {s.csrf}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.AddCookie(s.cookie)
		r, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("%s cross-site -> %d, want 403", route, r.StatusCode)
		}
	}
	// logout too
	if r := h.do("POST", "/logout", url.Values{}, s.with()...); r.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without csrf -> %d", r.StatusCode)
	}
	// The dead delivery was not requeued by any of the rejected posts.
	if d, _ := h.db.GetDelivery(context.Background(), dead); d.Status != "dead" {
		t.Fatalf("rejected posts changed state: %s", d.Status)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestLoginPageAndCSRF(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/login", nil)
	if r.StatusCode != 200 || !strings.Contains(r.body, `name="totp"`) || !strings.Contains(r.body, `type="password"`) {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	c := cookieNamed(r.Response, "__Host-can_login_csrf")
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("login csrf cookie: %+v", c)
	}
	good := url.Values{"username": {"root"}, "password": {h.pw}, "totp": {h.code()}}
	// no cookie, no field, mismatching field: all 403 and no credential check (no session issued).
	for name, tc := range map[string]struct {
		form   url.Values
		cookie *http.Cookie
	}{
		"no cookie":   {with(good, "csrf", csrfOf(t, r.body)), nil},
		"no field":    {good, c},
		"wrong field": {with(good, "csrf", "x"), c},
	} {
		var cs []*http.Cookie
		if tc.cookie != nil {
			cs = append(cs, tc.cookie)
		}
		p := h.do("POST", "/login", tc.form, cs...)
		if p.StatusCode != http.StatusForbidden || cookieNamed(p.Response, "can_session") != nil {
			t.Errorf("%s: %d", name, p.StatusCode)
		}
	}
	if n, _ := h.db.GetLoginAttempt(context.Background(), "user:root|127.0.0.1"); n.Failures != 0 {
		t.Fatal("CSRF-rejected posts must not count as credential failures")
	}
}

func with(v url.Values, k, val string) url.Values {
	out := url.Values{}
	for a, b := range v {
		out[a] = b
	}
	out.Set(k, val)
	return out
}

var tokenRe = regexp.MustCompile(`name="csrf" value="[^"]*"`)

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	h := newHarness(t)
	c, tok := h.loginForm()
	attempt := func(user, pw, code string) resp {
		return h.do("POST", "/login", url.Values{"username": {user}, "password": {pw}, "totp": {code}, "csrf": {tok}}, c)
	}
	a := attempt("root", "wrong password!!", h.code())
	b := attempt("nobody", h.pw, h.code())
	d := attempt("root", h.pw, "000000")
	for _, r := range []resp{a, b, d} {
		if r.StatusCode != http.StatusUnauthorized || !strings.Contains(r.body, "Invalid username, password or authentication code.") {
			t.Fatalf("%d %s", r.StatusCode, r.body)
		}
		if strings.Contains(r.body, h.pw) || strings.Contains(r.body, `value="wrong`) {
			t.Fatal("password echoed")
		}
	}
	norm := func(s string) string { return tokenRe.ReplaceAllString(s, "") }
	ok := norm(a.body)
	if strings.Replace(norm(b.body), "nobody", "root", 1) != ok || norm(d.body) != ok {
		t.Fatal("failure pages differ in more than the echoed username")
	}
	if !strings.Contains(a.body, `value="root"`) {
		t.Fatal("username should be kept")
	}
}

func TestLoginLockoutShows429WithRetryAfter(t *testing.T) {
	h := newHarness(t)
	c, tok := h.loginForm()
	var last resp
	for i := 0; i < 7; i++ {
		last = h.do("POST", "/login", url.Values{"username": {"root"}, "password": {"wrong password!!"}, "totp": {h.code()}, "csrf": {tok}}, c)
	}
	if last.StatusCode != http.StatusTooManyRequests || last.Header.Get("Retry-After") == "" || !strings.Contains(last.body, "Too many failed attempts") {
		t.Fatalf("%d %v %s", last.StatusCode, last.Header, last.body)
	}
	// even correct credentials are refused while locked
	r := h.do("POST", "/login", url.Values{"username": {"root"}, "password": {h.pw}, "totp": {h.code()}, "csrf": {tok}}, c)
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("%d", r.StatusCode)
	}
}

func TestLoginSessionIsSharedWithAPIAndLogoutEndsIt(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	if c := s.cookie; c.Value == "" {
		t.Fatal("no cookie")
	}
	r := h.do("GET", "/", nil, s.with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, "root") {
		t.Fatalf("%d", r.StatusCode)
	}
	out := h.do("POST", "/logout", url.Values{"csrf": {s.csrf}}, s.with()...)
	if out.StatusCode != http.StatusSeeOther || !strings.HasPrefix(out.Header.Get("Location"), "/login") {
		t.Fatalf("%d %s", out.StatusCode, out.Header.Get("Location"))
	}
	if r := h.do("GET", "/", nil, s.with()...); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("session survived logout: %d", r.StatusCode)
	}
}

func TestSecurityHeadersAndNoScriptsOrInlineStyles(t *testing.T) {
	h := newHarness(t)
	h.seed()
	s := h.login()
	for _, p := range []string{"/", "/modules", "/modules/forms", "/audit", "/events", "/leads", "/settings", "/nope"} {
		r := h.do("GET", p, nil, s.with()...)
		csp := r.Header.Get("Content-Security-Policy")
		for _, want := range []string{"script-src 'none'", "frame-ancestors 'none'", "form-action 'self'", "base-uri 'none'", "default-src 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", p, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: unsafe-inline in CSP", p)
		}
		if r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("X-Content-Type-Options") != "nosniff" || r.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", p, r.Header)
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
			t.Errorf("%s: content type %s", p, r.Header.Get("Content-Type"))
		}
		low := strings.ToLower(r.body)
		for _, bad := range []string{"<script", "javascript:", " style=", "<style"} {
			if strings.Contains(low, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		if regexp.MustCompile(`\son[a-z]+=`).MatchString(low) {
			t.Errorf("%s has an inline event handler", p)
		}
	}
}

func TestNotFoundPage(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/no/such/page", nil)
	if r.StatusCode != 404 || !strings.Contains(r.body, "Page not found") || strings.Contains(r.body, "Log out") {
		t.Fatalf("anonymous 404: %d %s", r.StatusCode, r.body)
	}
	s := h.login()
	r = h.do("GET", "/no/such/page", nil, s.with()...)
	if r.StatusCode != 404 || !strings.Contains(r.body, "Page not found") || !strings.Contains(r.body, "Log out") {
		t.Fatalf("signed-in 404: %d", r.StatusCode)
	}
	if r := h.do("GET", "/modules/nope", nil, s.with()...); r.StatusCode != 404 || !strings.Contains(r.body, "Module not found") {
		t.Fatalf("%d", r.StatusCode)
	}
	if r := h.do("GET", "/events/abc", nil, s.with()...); r.StatusCode != 404 {
		t.Fatalf("%d", r.StatusCode)
	}
}

func TestDashboard(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	r := h.do("GET", "/", nil, s.with()...)
	for _, want := range []string{"Acme Pty Ltd", "acme.example", "No modules are installed yet", "make enable"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("empty dashboard lacks %q", want)
		}
	}
	h.seed()
	dead := h.killDelivery()
	r = h.do("GET", "/", nil, s.with()...)
	for _, want := range []string{"Forms gateway", "Wiki", "Failed", "helm install failed: timeout", "leads", "module.enable", "/events/" + itoa(dead) + "/requeue", "admin.login"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	if strings.Contains(r.body, "s3cret") || strings.Contains(r.body, "private@example.com") {
		t.Fatal("secrets or event payloads leaked to the dashboard")
	}
}

func TestHostileDataIsEscapedEverywhere(t *testing.T) {
	h := newHarness(t)
	h.seed()
	const evil = `<script>alert(1)</script>"'><img src=x onerror=alert(2)>`
	var m config.Module
	m.ID, m.Name, m.Version = "evil", evil, evil
	m.Provider.Name, m.Provider.Licence, m.Provider.LicenceClass = evil, evil, "osi"
	m.Description = evil
	m.Permissions = []string{evil}
	m.Consumes = []string{evil}
	must(t, h.db.UpsertModule(context.Background(), store.Module{ID: "evil", Version: evil, Namespace: "mod-evil", ServiceAccount: "evil", State: store.StateFailed, LastError: evil, HMACSecret: "x", Manifest: m}, nil, nil))
	_, err := h.audit.Append(context.Background(), auditEntry(evil))
	must(t, err)
	h.leads.leads = []Lead{{ID: "1", FormID: evil, Status: "new", Email: evil, Name: evil, Note: evil, Fields: map[string]any{evil: evil, "n": 5}, CreatedAt: fixedNow, UpdatedAt: fixedNow}}
	s := h.login()
	for _, p := range []string{"/", "/modules", "/modules/evil", "/audit", "/audit?actor=" + url.QueryEscape(evil), "/leads", "/leads/1", "/events", "/leads?status=" + url.QueryEscape(evil)} {
		r := h.do("GET", p, nil, s.with()...)
		if strings.Contains(r.body, "<script>alert") || strings.Contains(r.body, "<img src=x") || strings.Contains(r.body, `onerror=alert(2)>`) {
			t.Errorf("%s: raw hostile markup reached the page", p)
		}
	}
	if r := h.do("GET", "/modules/evil", nil, s.with()...); !strings.Contains(r.body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("module name was not rendered as escaped text:\n%s", r.body)
	}
	// Flash messages come only from the catalogue, never from the query string.
	r := h.do("GET", "/settings?flash="+url.QueryEscape(evil), nil, s.with()...)
	if strings.Contains(r.body, "alert(1)") {
		t.Error("flash echoed arbitrary text")
	}
}

func TestModulesPages(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	if r := h.do("GET", "/modules", nil, s.with()...); !strings.Contains(r.body, "No modules are installed yet") {
		t.Fatal("empty state missing")
	}
	h.seed()
	r := h.do("GET", "/modules", nil, s.with()...)
	for _, want := range []string{"Forms gateway", "Wiki", "Source-available", "32 MB", "256 MB", "Total declared RAM of enabled modules: 32 MB"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("modules page lacks %q", want)
		}
	}
	r = h.do("GET", "/modules/wiki", nil, s.with()...)
	for _, want := range []string{"Source-available", "BUSL-1.1", "256 MB", "postgres", "helm install failed: timeout"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("wiki detail lacks %q", want)
		}
	}
	r = h.do("GET", "/modules/forms", nil, s.with()...)
	for _, want := range []string{"leads.v1", "leads.read", "email.send"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("forms detail lacks %q", want)
		}
	}
	if strings.Contains(r.body, "Source-available") || strings.Contains(r.body, "s3cret") {
		t.Error("forms page wrongly flagged or leaked the HMAC secret")
	}
}

func TestRequeueFlow(t *testing.T) {
	h := newHarness(t)
	h.seed()
	s := h.login()
	id := h.killDelivery()
	path := "/events/" + itoa(id) + "/requeue"
	// open redirect attempt: next must be a known internal path.
	r := h.do("POST", path, url.Values{"csrf": {s.csrf}, "next": {"https://evil.example/"}}, s.with()...)
	if r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/events") {
		t.Fatalf("%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	d, _ := h.db.GetDelivery(context.Background(), id)
	if d.Status != "pending" || d.Attempts != 0 {
		t.Fatalf("%+v", d)
	}
	recs, _ := h.audit.Query(context.Background(), auditFilter("delivery."))
	if len(recs) != 1 || recs[0].Actor != "root" || recs[0].Target != itoa(id) {
		t.Fatalf("%+v", recs)
	}
	// second requeue: the delivery is no longer dead
	if r := h.do("POST", path, url.Values{"csrf": {s.csrf}}, s.with()...); r.StatusCode != http.StatusConflict || !strings.Contains(r.body, "Only dead deliveries") {
		t.Fatalf("%d", r.StatusCode)
	}
	if r := h.do("POST", "/events/999/requeue", url.Values{"csrf": {s.csrf}}, s.with()...); r.StatusCode != 404 {
		t.Fatalf("%d", r.StatusCode)
	}
	// the redirect target for the dashboard form
	h.killDelivery()
	r = h.do("POST", path, url.Values{"csrf": {s.csrf}, "next": {"/"}}, s.with()...)
	if !strings.HasPrefix(r.Header.Get("Location"), "/?flash=") {
		t.Fatalf("%s", r.Header.Get("Location"))
	}
}

func TestEventsPages(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	if r := h.do("GET", "/events", nil, s.with()...); !strings.Contains(r.body, "No events have been delivered yet") {
		t.Fatal("empty state")
	}
	h.seed()
	id := h.killDelivery()
	r := h.do("GET", "/events", nil, s.with()...)
	if !strings.Contains(r.body, "lead.created.v1") || !strings.Contains(r.body, "Dead") {
		t.Fatalf("%s", r.body)
	}
	r = h.do("GET", "/events/"+itoa(id), nil, s.with()...)
	for _, want := range []string{"bad gateway", "502", "Requeue", "evt-1"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	if strings.Contains(r.body, "private@example.com") {
		t.Fatal("event payload shown")
	}
}

func TestAuditViewerFilterVerifyExport(t *testing.T) {
	h := newHarness(t)
	h.seed()
	s := h.login()
	r := h.do("GET", "/audit", nil, s.with()...)
	if !strings.Contains(r.body, "module.enable") || !strings.Contains(r.body, "admin.login") {
		t.Fatal("entries missing")
	}
	r = h.do("GET", "/audit?actor=cli", nil, s.with()...)
	if !strings.Contains(r.body, "module.enable") || strings.Contains(r.body, "admin.login") {
		t.Fatal("actor filter")
	}
	r = h.do("GET", "/audit?action=admin.&before=abc", nil, s.with()...)
	if r.StatusCode != 200 || strings.Contains(r.body, "module.enable") {
		t.Fatalf("%d", r.StatusCode)
	}
	// paging: limit is one page, an "Older" link carries before=<seq>
	for i := 0; i < pageSize+3; i++ {
		_, err := h.audit.Append(context.Background(), auditEntry("x"))
		must(t, err)
	}
	r = h.do("GET", "/audit", nil, s.with()...)
	if !regexp.MustCompile(`href="/audit\?[^"]*before=\d+`).MatchString(r.body) {
		t.Fatalf("no older link")
	}

	v := h.do("POST", "/audit/verify", url.Values{"csrf": {s.csrf}}, s.with()...)
	if v.StatusCode != 200 || !strings.Contains(v.body, "Audit log verified") {
		t.Fatalf("%d %s", v.StatusCode, v.body)
	}
	// tamper
	if _, err := h.db.Exec(`UPDATE audit_log SET actor='mallory' WHERE seq=1`); err != nil {
		t.Fatal(err)
	}
	v = h.do("POST", "/audit/verify", url.Values{"csrf": {s.csrf}}, s.with()...)
	if !strings.Contains(v.body, "hash-mismatch") || !strings.Contains(v.body, "TAMPERED") {
		t.Fatalf("tampering not reported:\n%s", v.body)
	}

	if g := h.do("GET", "/audit/export", nil, s.with()...); g.StatusCode == 200 {
		t.Fatal("export must not be reachable with GET (it writes an audit row)")
	}
	e := h.do("POST", "/audit/export", url.Values{"csrf": {s.csrf}}, s.with()...)
	if e.StatusCode != 200 || !strings.Contains(e.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(e.Header.Get("Content-Type"), "ndjson") {
		t.Fatalf("%d %v", e.StatusCode, e.Header)
	}
	if lines := strings.Count(strings.TrimSpace(e.body), "\n") + 1; lines < pageSize {
		t.Fatalf("export has %d lines", lines)
	}
	recs, _ := h.audit.Query(context.Background(), auditFilter("audit."))
	if len(recs) < 3 { // verify x2, export
		t.Fatalf("verify/export not audited: %+v", recs)
	}
}

func TestLeadsViews(t *testing.T) {
	h := newHarness(t)
	h.seed()
	h.leads.leads = []Lead{
		{ID: "7", FormID: "contact", Status: "new", Email: "ann@example.com", Name: "Ann", Fields: map[string]any{"message": "Hello"}, CreatedAt: fixedNow, UpdatedAt: fixedNow},
		{ID: "8", FormID: "contact", Status: "closed", Email: "bob@example.com", Name: "Bob", CreatedAt: fixedNow, UpdatedAt: fixedNow},
	}
	s := h.login()
	r := h.do("GET", "/leads", nil, s.with()...)
	if !strings.Contains(r.body, "ann@example.com") || !strings.Contains(r.body, "bob@example.com") || !strings.Contains(r.body, "cursor=42") {
		t.Fatalf("%s", r.body)
	}
	r = h.do("GET", "/leads?status=closed&cursor=5", nil, s.with()...)
	if strings.Contains(r.body, "ann@example.com") || h.leads.listQ[len(h.leads.listQ)-1] != (LeadQuery{Status: "closed", Cursor: "5", Limit: pageSize}) {
		t.Fatalf("filter not passed: %+v", h.leads.listQ)
	}
	// An unknown status filter is ignored, never forwarded.
	h.do("GET", "/leads?status=bogus", nil, s.with()...)
	if h.leads.listQ[len(h.leads.listQ)-1].Status != "" {
		t.Fatal("bogus status forwarded to the gateway")
	}
	r = h.do("GET", "/leads/7", nil, s.with()...)
	if !strings.Contains(r.body, "Hello") || !strings.Contains(r.body, `action="/leads/7/status"`) {
		t.Fatalf("%s", r.body)
	}
	if r := h.do("GET", "/leads/404", nil, s.with()...); r.StatusCode != 404 {
		t.Fatalf("%d", r.StatusCode)
	}

	post := func(id, st string) resp {
		return h.do("POST", "/leads/"+id+"/status", url.Values{"csrf": {s.csrf}, "status": {st}}, s.with()...)
	}
	if r := post("7", "qualified"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/leads/7?flash=flash.lead_status" {
		t.Fatalf("%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if h.leads.statuses["7"] != "qualified" {
		t.Fatalf("%v", h.leads.statuses)
	}
	if r := post("7", "hacked"); r.StatusCode != 400 {
		t.Fatalf("%d", r.StatusCode)
	}
	if r := post("404", "new"); r.StatusCode != 404 {
		t.Fatalf("%d", r.StatusCode)
	}
	recs, _ := h.audit.Query(context.Background(), auditFilter("lead."))
	if len(recs) != 1 || recs[0].Action != "lead.status" || recs[0].Target != "7" || !strings.Contains(recs[0].Detail, "qualified") {
		t.Fatalf("%+v", recs)
	}
	if strings.Contains(recs[0].Detail, "ann@example.com") {
		t.Fatal("PII in the audit detail")
	}
}

func TestLeadsUnavailableAndGatewayFailure(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	h.leads.err = ErrLeadsUnavailable
	r := h.do("GET", "/leads", nil, s.with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, "Leads are not available") {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	h.leads.err = errors.New("dial tcp 10.1.2.3:8081: connection refused")
	r = h.do("GET", "/leads", nil, s.with()...)
	if r.StatusCode != http.StatusBadGateway || strings.Contains(r.body, "10.1.2.3") || !strings.Contains(r.body, "Request id") {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	h.ui.Leads = nil // not wired at all
	if r := h.do("GET", "/leads", nil, s.with()...); r.StatusCode != 200 || !strings.Contains(r.body, "Leads are not available") {
		t.Fatalf("%d", r.StatusCode)
	}
}

func TestSettingsChangePassword(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	*h.now = h.now.Add(31 * time.Second) // a TOTP step the login has not used
	otp := h.code()
	form := func(cur, n, c string) url.Values {
		return url.Values{"csrf": {s.csrf}, "current": {cur}, "otp": {otp}, "new": {n}, "confirm": {c}}
	}
	if r := h.do("POST", "/settings/password", form(h.pw, "a new long password", "different"), s.with()...); r.StatusCode != 400 || !strings.Contains(r.body, "do not match") {
		t.Fatalf("%d", r.StatusCode)
	}
	if r := h.do("POST", "/settings/password", form(h.pw, "short", "short"), s.with()...); r.StatusCode != 400 || !strings.Contains(r.body, "at least 12") {
		t.Fatalf("%d", r.StatusCode)
	}
	r := h.do("POST", "/settings/password", form("wrong current pw", "a new long password", "a new long password"), s.with()...)
	if r.StatusCode != 401 || !strings.Contains(r.body, "is incorrect") {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	bad := form(h.pw, "a new long password", "a new long password")
	bad.Set("otp", "000000")
	if r := h.do("POST", "/settings/password", bad, s.with()...); r.StatusCode != 401 {
		t.Fatalf("wrong authentication code accepted: %d", r.StatusCode)
	}
	bad.Del("otp")
	if r := h.do("POST", "/settings/password", bad, s.with()...); r.StatusCode != 401 {
		t.Fatalf("missing authentication code accepted: %d", r.StatusCode)
	}
	r = h.do("POST", "/settings/password", form(h.pw, "a new long password", "a new long password"), s.with()...)
	if r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/login") {
		t.Fatalf("%d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := h.do("GET", "/", nil, s.with()...); r.StatusCode != http.StatusSeeOther {
		t.Fatal("session survived a password change")
	}
	if strings.Contains(h.logs.String(), "a new long password") || strings.Contains(h.logs.String(), h.pw) {
		t.Fatal("password in logs")
	}
}

func TestSettingsRotateTOTP(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	r := h.do("POST", "/settings/totp", url.Values{"csrf": {s.csrf}, "current": {"wrong current pw"}}, s.with()...)
	if r.StatusCode != 401 {
		t.Fatalf("%d", r.StatusCode)
	}
	r = h.do("POST", "/settings/totp", url.Values{"csrf": {s.csrf}, "current": {h.pw}}, s.with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, "otpauth://totp/") {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	secret := regexp.MustCompile(`name="secret" value="([A-Z2-7]+)"`).FindStringSubmatch(r.body)
	if secret == nil {
		t.Fatalf("no secret field:\n%s", r.body)
	}
	if u, _ := h.db.AdminByUsername(context.Background(), "root"); u.TOTPSecret != h.secret {
		t.Fatal("secret replaced before confirmation")
	}
	*h.now = h.now.Add(31 * time.Second)
	conf := func(code string) resp {
		return h.do("POST", "/settings/totp/confirm", url.Values{"csrf": {s.csrf}, "current": {h.pw}, "otp": {h.code()}, "secret": {secret[1]}, "code": {code}}, s.with()...)
	}
	if r := h.do("POST", "/settings/totp/confirm", url.Values{"csrf": {s.csrf}, "current": {h.pw}, "secret": {secret[1]}, "code": {"000000"}}, s.with()...); r.StatusCode != 401 {
		t.Fatalf("missing current code accepted: %d", r.StatusCode)
	}
	if r := conf("000000"); r.StatusCode != 400 || !strings.Contains(r.body, "otpauth://") {
		t.Fatalf("%d", r.StatusCode) // re-shows the secret so the operator can retry
	}
	*h.now = h.now.Add(31 * time.Second) // the first attempt consumed its current code
	key, _ := authDecode(secret[1])
	code := authCode(key, *h.now)
	r = conf(code)
	if r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/login") {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	if u, _ := h.db.AdminByUsername(context.Background(), "root"); u.TOTPSecret != secret[1] {
		t.Fatal("secret not rotated")
	}
}

func TestSettingsSessions(t *testing.T) {
	h := newHarness(t)
	a := h.login()
	*h.now = h.now.Add(31 * time.Second) // a fresh TOTP step for the second login
	b := h.login()
	r := h.do("GET", "/settings", nil, a.with()...)
	ids := regexp.MustCompile(`/settings/sessions/([0-9a-f]{16})/revoke`).FindAllStringSubmatch(r.body, -1)
	if len(ids) != 1 || !strings.Contains(r.body, "This session") {
		t.Fatalf("want one revocable session besides the current one, got %d\n%s", len(ids), r.body)
	}
	if strings.Contains(r.body, a.cookie.Value) || strings.Contains(r.body, b.cookie.Value) {
		t.Fatal("cookie token rendered")
	}
	if r := h.do("POST", "/settings/sessions/ffffffffffffffff/revoke", url.Values{"csrf": {a.csrf}}, a.with()...); r.StatusCode != 404 {
		t.Fatalf("unknown session: %d", r.StatusCode)
	}
	if r := h.do("POST", "/settings/sessions/"+ids[0][1]+"/revoke", url.Values{"csrf": {a.csrf}}, a.with()...); r.StatusCode != 303 {
		t.Fatalf("%d", r.StatusCode)
	}
	if r := h.do("GET", "/", nil, b.with()...); r.StatusCode != http.StatusSeeOther {
		t.Fatal("revoked session still works")
	}
	if r := h.do("GET", "/", nil, a.with()...); r.StatusCode != 200 {
		t.Fatal("current session was revoked")
	}
}

func TestStaticAndManifest(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/static/admin.css", nil)
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/css") || !strings.Contains(r.Header.Get("Cache-Control"), "max-age") {
		t.Fatalf("%d %v", r.StatusCode, r.Header)
	}
	if r := h.do("GET", "/static/../adminui.go", nil); r.StatusCode == 200 || strings.Contains(r.body, "package adminui") {
		t.Fatalf("traversal: %d", r.StatusCode)
	}
	if r := h.do("GET", "/static/missing.css", nil); r.StatusCode != 404 {
		t.Fatalf("%d", r.StatusCode)
	}
	m := h.do("GET", "/manifest.webmanifest", nil)
	if m.StatusCode != 200 || !strings.HasPrefix(m.Header.Get("Content-Type"), "application/manifest+json") {
		t.Fatalf("%d %v", m.StatusCode, m.Header)
	}
	for _, want := range []string{`"display":"standalone"`, `"start_url":"/"`, `icon-192.png"`, `icon-512.png"`, `"scope":"/"`} {
		if !strings.Contains(strings.ReplaceAll(m.body, " ", ""), want) {
			t.Errorf("manifest lacks %s:\n%s", want, m.body)
		}
	}
	for _, f := range []string{"icon-192.png", "icon-512.png", "icon.svg"} {
		if r := h.do("GET", "/static/"+f, nil); r.StatusCode != 200 || len(r.body) < 50 {
			t.Errorf("%s: %d", f, r.StatusCode)
		}
	}
	// The only service worker is the push-only one of specs/025-pwa-webpush (see pwa_test.go).
	if r := h.do("GET", "/sw.js", nil); r.StatusCode != 200 || strings.Contains(r.body, "fetch") {
		t.Fatalf("service worker: %d", r.StatusCode)
	}
}

func TestRoutesMatchContract(t *testing.T) {
	b, err := os.ReadFile("../../../specs/010-admin-ui/contracts/ui-routes.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^\\| `((?:GET|POST) [^`]+)` \\|").FindAllStringSubmatch(string(b), -1) {
		doc[m[1]] = true
	}
	code := map[string]bool{}
	for _, r := range Routes() {
		code[r] = true
		if !doc[r] {
			t.Errorf("route %q is not documented in ui-routes.md", r)
		}
	}
	for r := range doc {
		if !code[r] {
			t.Errorf("ui-routes.md documents %q which is not served", r)
		}
	}
}

func TestCombinedHandlerSendsAPIPathsToTheAPI(t *testing.T) {
	h := newHarness(t)
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("API:" + r.URL.Path)) })
	srv := newServer(t, h.ui.Handler(api))
	for _, p := range []string{"/api/v1/session", "/healthz"} {
		r, _ := http.Get(srv.URL + p)
		b := readAll(r)
		if b != "API:"+p {
			t.Errorf("%s -> %q", p, b)
		}
	}
	r, _ := http.Get(srv.URL + "/login")
	if !strings.Contains(readAll(r), "<form") {
		t.Fatal("UI not served at /login")
	}
}

func TestAuditVerifyAndExportAreSingleFlight(t *testing.T) {
	h := newHarness(t)
	s := h.login()
	h.ui.heavy.Lock() // a verification is "running"
	for _, p := range []string{"/audit/verify", "/audit/export"} {
		r := h.do("POST", p, url.Values{"csrf": {s.csrf}}, s.with()...)
		if r.StatusCode != http.StatusServiceUnavailable || r.Header.Get("Retry-After") == "" {
			t.Errorf("%s while busy -> %d", p, r.StatusCode)
		}
	}
	h.ui.heavy.Unlock()
	if r := h.do("POST", "/audit/verify", url.Values{"csrf": {s.csrf}}, s.with()...); r.StatusCode != 200 {
		t.Fatalf("lock not released: %d", r.StatusCode)
	}
	if r := h.do("POST", "/audit/verify", url.Values{"csrf": {s.csrf}}, s.with()...); r.StatusCode != 200 {
		t.Fatalf("second sequential verify: %d", r.StatusCode)
	}
}

func TestClipCountsRunes(t *testing.T) {
	if got := clip(strings.Repeat("é", 70), 64); got != strings.Repeat("é", 64) {
		t.Fatalf("clip cut inside a character: %q", got)
	}
	if got := clip("  ab  ", 64); got != "ab" {
		t.Fatalf("%q", got)
	}
}

func TestPlainHTTPDevKeepsUnprefixedLoginCookie(t *testing.T) {
	h := newHarness(t)
	h.ui.Secure = false
	r := h.do("GET", "/login", nil)
	if c := cookieNamed(r.Response, "can_login_csrf"); c == nil || c.Secure {
		t.Fatalf("%+v", c)
	}
	if cookieNamed(r.Response, "__Host-can_login_csrf") != nil {
		t.Fatal("__Host- cookies need Secure")
	}
}
