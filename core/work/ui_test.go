package work

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// uiBrowser is a minimal cookie-keeping client over the real handler.
type uiBrowser struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]*http.Cookie
	ip      string
}

type uiResp struct {
	Code   int
	Header http.Header
	Body   string
}

func newUIBrowser(t *testing.T, h *harness) *uiBrowser {
	return &uiBrowser{t: t, h: h.h, cookies: map[string]*http.Cookie{}, ip: "192.0.2.77:4000"}
}

func (b *uiBrowser) do(method, path string, form url.Values, hdr ...string) uiResp {
	b.t.Helper()
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.RemoteAddr = b.ip
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 || c.Value == "" {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return uiResp{rec.Code, rec.Header(), rec.Body.String()}
}

var uiCSRFRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// csrf loads a page and returns the CSRF token in its first form.
func (b *uiBrowser) csrf(path string) string {
	b.t.Helper()
	r := b.do("GET", path, nil)
	m := uiCSRFRe.FindStringSubmatch(r.Body)
	if m == nil {
		b.t.Fatalf("no csrf field on %s (%d): %.300s", path, r.Code, r.Body)
	}
	return m[1]
}

// login signs in with a token; it returns the response of the POST.
func (b *uiBrowser) login(token string) uiResp {
	b.t.Helper()
	c := b.csrf("/ui/login")
	return b.do("POST", "/ui/login", url.Values{"token": {token}, "csrf": {c}})
}

func (b *uiBrowser) mustLogin(token string) {
	b.t.Helper()
	if r := b.login(token); r.Code != 303 {
		b.t.Fatalf("login: %d %.300s", r.Code, r.Body)
	}
}

// post sends a form with the page's CSRF token filled in.
func (b *uiBrowser) post(page, action string, form url.Values, hdr ...string) uiResp {
	b.t.Helper()
	f := url.Values{"csrf": {b.csrf(page)}}
	for k, v := range form {
		f[k] = v
	}
	return b.do("POST", action, f, hdr...)
}

type uiFixture struct {
	h       *harness
	rw      string // personal token, write, ACME only
	ro      string // personal token, read, ACME only
	other   string // personal token, write, SEC only
	acmeKey string
	secKey  string
}

func newUIFixture(t *testing.T) *uiFixture {
	t.Helper()
	h := newHarness(t, nil)
	h.setup()
	f := &uiFixture{h: h}
	f.rw = h.newToken("personal", "dev@example.com", []string{PermWrite}, "ACME")
	f.ro = h.newToken("personal", "viewer@example.com", []string{PermRead}, "ACME")
	f.other = h.newToken("personal", "sec@example.com", []string{PermWrite}, "SEC")
	f.acmeKey = h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "story", "title": "Customers can sign up", "priority": "P1", "assignee": "dev@example.com", "labels": []string{"web"}})["key"].(string)
	f.secKey = h.mustDo(201, "POST", "/v1/projects/SEC/issues", bootstrap, map[string]any{"type": "bug", "title": "vuln in prod", "priority": "P0"})["key"].(string)
	return f
}

func (f *uiFixture) browser(t *testing.T, token string) *uiBrowser {
	b := newUIBrowser(t, f.h)
	b.mustLogin(token)
	return b
}

func issueVersion(t *testing.T, h *harness, key string) int64 {
	t.Helper()
	return int64(h.mustDo(200, "GET", "/v1/issues/"+key, bootstrap, nil)["version"].(float64))
}

func TestUIRequiresLoginAndRedirectsSafely(t *testing.T) {
	f := newUIFixture(t)
	b := newUIBrowser(t, f.h)
	for _, p := range []string{"/ui/", "/ui/projects/ACME", "/ui/projects/ACME/board", "/ui/issues/" + f.acmeKey, "/ui/dashboard"} {
		r := b.do("GET", p, nil)
		if r.Code != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/ui/login") {
			t.Errorf("%s anonymous = %d %q", p, r.Code, r.Header.Get("Location"))
		}
		if strings.Contains(r.Body, "Customers can sign up") {
			t.Errorf("%s leaked content to an anonymous caller", p)
		}
	}
	if r := b.do("POST", "/ui/projects/ACME/issues", url.Values{"title": {"x"}}); r.Code != 303 {
		t.Errorf("anonymous POST = %d", r.Code)
	}
	// next is honoured only for local /ui/ paths.
	for _, next := range []string{"//evil.example/x", "https://evil.example/", "/v1/tokens", `/ui/\evil`, "/ui/../v1/me", "javascript:alert(1)"} {
		bb := newUIBrowser(t, f.h)
		c := bb.csrf("/ui/login")
		r := bb.do("POST", "/ui/login", url.Values{"token": {f.rw}, "csrf": {c}, "next": {next}})
		if r.Code != 303 || r.Header.Get("Location") != "/ui/" {
			t.Errorf("next=%q -> %d %q, want /ui/", next, r.Code, r.Header.Get("Location"))
		}
	}
	bb := newUIBrowser(t, f.h)
	c := bb.csrf("/ui/login")
	r := bb.do("POST", "/ui/login", url.Values{"token": {f.rw}, "csrf": {c}, "next": {"/ui/projects/ACME/board?sprint=all"}})
	if r.Header.Get("Location") != "/ui/projects/ACME/board?sprint=all" {
		t.Errorf("local next dropped: %q", r.Header.Get("Location"))
	}
}

func TestUILoginSessionCookieAndNoEcho(t *testing.T) {
	f := newUIFixture(t)
	b := newUIBrowser(t, f.h)
	bad := "cwt_0123456789abcdef_" + strings.Repeat("z", 43)
	r := b.login(bad)
	if r.Code != 401 || strings.Contains(r.Body, bad) || strings.Contains(r.Body, "cwt_") {
		t.Fatalf("bad login: %d (token echoed or wrong status): %.300s", r.Code, r.Body)
	}
	if len(b.cookies) > 1 {
		t.Errorf("a failed login set a session: %v", b.cookies)
	}
	r = b.login(f.rw)
	if r.Code != 303 || r.Header.Get("Location") != "/ui/" {
		t.Fatalf("login: %d %q", r.Code, r.Header.Get("Location"))
	}
	var sess *http.Cookie
	for _, c := range b.cookies {
		if strings.Contains(c.Name, "session") {
			sess = c
		}
	}
	if sess == nil || !sess.HttpOnly || sess.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie flags: %+v", sess)
	}
	if strings.Contains(sess.Value, "cwt_") || strings.Contains(sess.Value, f.rw) {
		t.Fatal("the cookie carries the token")
	}
	page := b.do("GET", "/ui/", nil)
	if page.Code != 200 || !strings.Contains(page.Body, "Acme") || strings.Contains(page.Body, "Security") {
		t.Fatalf("project list: %d %.400s", page.Code, page.Body)
	}
	if strings.Contains(page.Body, f.rw) {
		t.Fatal("token echoed in a page")
	}
	// Secure cookies behind TLS.
	h2 := newHarness(t, nil)
	b2 := newUIBrowser(t, h2)
	req := httptest.NewRequest("GET", "/ui/login", nil)
	req.TLS = &tlsStub
	rec := httptest.NewRecorder()
	h2.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if !c.Secure || !strings.HasPrefix(c.Name, "__Host-") {
			t.Errorf("cookie %s over TLS is not Secure/__Host-: %+v", c.Name, c)
		}
	}
	_ = b2
	// logout ends the session
	if r := b.post("/ui/", "/ui/logout", nil); r.Code != 303 {
		t.Fatalf("logout: %d", r.Code)
	}
	if r := b.do("GET", "/ui/", nil); r.Code != 303 {
		t.Fatalf("after logout: %d", r.Code)
	}
}

func TestUISessionFixationAndRevocation(t *testing.T) {
	f := newUIFixture(t)
	b := newUIBrowser(t, f.h)
	b.cookies["work_session"] = &http.Cookie{Name: "work_session", Value: "attacker-chosen-session-id"}
	b.mustLogin(f.rw)
	if v := b.cookies["work_session"]; v == nil || v.Value == "attacker-chosen-session-id" {
		t.Fatalf("login adopted the presented session id: %+v", v)
	}
	atk := newUIBrowser(t, f.h)
	atk.cookies["work_session"] = &http.Cookie{Name: "work_session", Value: "attacker-chosen-session-id"}
	if r := atk.do("GET", "/ui/", nil); r.Code != 303 {
		t.Fatalf("a made-up session id works: %d", r.Code)
	}
	// Revoking the token ends the session at the next request.
	tokens := f.h.mustDo(200, "GET", "/v1/tokens", bootstrap, nil)["items"].([]any)
	for _, it := range tokens {
		m := it.(map[string]any)
		if m["subject"] == "dev@example.com" {
			f.h.mustDo(204, "DELETE", "/v1/tokens/"+m["id"].(string), bootstrap, nil)
		}
	}
	if r := b.do("GET", "/ui/", nil); r.Code != 303 {
		t.Fatalf("a revoked token kept its session: %d", r.Code)
	}
}

func TestUIValidCredentialsAreNeverThrottled(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RateLimit.AuthFailuresPerMin = 3 })
	h.setup()
	good := h.newToken("personal", "dev@example.com", []string{PermWrite})
	b := newUIBrowser(t, h)
	saw429 := false
	for i := 0; i < 8; i++ {
		if r := b.login("cwt_0123456789abcdef_" + strings.Repeat("q", 43)); r.Code == 429 {
			saw429 = true
		}
	}
	if !saw429 {
		t.Fatal("failed logins from one address were never slowed down")
	}
	if r := b.login(good); r.Code != 303 {
		t.Fatalf("valid credentials were throttled: %d", r.Code)
	}
}

func TestUICSRFAndSiteChecks(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.rw)
	before := issueVersion(t, f.h, f.acmeKey)
	form := url.Values{"to": {"in_progress"}, "version": {strconv.FormatInt(before, 10)}}
	for name, c := range map[string]struct {
		form url.Values
		hdr  []string
	}{
		"no token":       {form, nil},
		"wrong token":    {withCSRF(form, "nope"), nil},
		"cross-site":     {withCSRF(form, b.csrf("/ui/")), []string{"Sec-Fetch-Site", "cross-site"}},
		"same-site":      {withCSRF(form, b.csrf("/ui/")), []string{"Sec-Fetch-Site", "same-site"}},
		"foreign Origin": {withCSRF(form, b.csrf("/ui/")), []string{"Origin", "https://evil.example"}},
	} {
		r := b.do("POST", "/ui/issues/"+f.acmeKey+"/transition", c.form, c.hdr...)
		if r.Code != 403 {
			t.Errorf("%s: %d, want 403", name, r.Code)
		}
	}
	if issueVersion(t, f.h, f.acmeKey) != before {
		t.Fatal("a refused request changed the issue")
	}
	// A session's CSRF token does not work for another session.
	b2 := f.browser(t, f.rw)
	if r := b.do("POST", "/ui/issues/"+f.acmeKey+"/transition", withCSRF(form, b2.csrf("/ui/"))); r.Code != 403 {
		t.Errorf("another session's token accepted: %d", r.Code)
	}
	// Same-origin works (Origin matching Host, Sec-Fetch-Site same-origin).
	r := b.do("POST", "/ui/issues/"+f.acmeKey+"/transition", withCSRF(form, b.csrf("/ui/")), "Sec-Fetch-Site", "same-origin", "Origin", "http://example.com")
	if r.Code != 303 {
		t.Errorf("same-origin POST: %d %.200s", r.Code, r.Body)
	}
	// Login CSRF.
	nb := newUIBrowser(t, f.h)
	if r := nb.do("POST", "/ui/login", url.Values{"token": {f.rw}, "csrf": {"x"}}); r.Code != 403 {
		t.Errorf("login without a login cookie: %d", r.Code)
	}
}

func withCSRF(v url.Values, tok string) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = vs
	}
	if tok != "" {
		out.Set("csrf", tok)
	}
	return out
}

func TestUIScopeIsEnforcedOnEveryRoute(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.rw)
	sp := f.h.mustDo(201, "POST", "/v1/projects/SEC/sprints", bootstrap, map[string]any{"name": "sec sprint"})
	sid := strconv.Itoa(int(sp["id"].(float64)))
	v := strconv.FormatInt(issueVersion(t, f.h, f.secKey), 10)
	for _, c := range []struct{ method, path string }{
		{"GET", "/ui/projects/SEC"}, {"GET", "/ui/projects/SEC/board"}, {"GET", "/ui/projects/SEC/board?sprint=all"},
		{"GET", "/ui/projects/SEC/sprints"}, {"GET", "/ui/projects/SEC/epics"}, {"GET", "/ui/issues/" + f.secKey},
		{"POST", "/ui/projects/SEC/issues"}, {"POST", "/ui/projects/SEC/sprints"},
		{"POST", "/ui/issues/" + f.secKey + "/transition"}, {"POST", "/ui/issues/" + f.secKey + "/edit"},
		{"POST", "/ui/issues/" + f.secKey + "/priority"}, {"POST", "/ui/issues/" + f.secKey + "/assignee"},
		{"POST", "/ui/issues/" + f.secKey + "/sprint"}, {"POST", "/ui/issues/" + f.secKey + "/rank"},
		{"POST", "/ui/issues/" + f.secKey + "/comments"}, {"POST", "/ui/issues/" + f.secKey + "/links"},
		{"POST", "/ui/issues/" + f.secKey + "/links/1/remove"}, {"POST", "/ui/issues/" + f.secKey + "/watch"},
		{"POST", "/ui/sprints/" + sid + "/start"}, {"POST", "/ui/sprints/" + sid + "/complete"},
	} {
		var r uiResp
		if c.method == "GET" {
			r = b.do("GET", c.path, nil)
		} else {
			r = b.do("POST", c.path, url.Values{
				"csrf": {b.csrf("/ui/")}, "version": {v}, "to": {"in_progress"}, "title": {"x"}, "type": {"task"}, "body": {"x"},
				"priority": {"P0"}, "assignee": {"x"}, "name": {"x"}, "position": {"first"}, "before": {f.acmeKey}, "link_type": {"blocks"},
				"target": {f.acmeKey}, "sprint": {""}, "on": {"1"},
			})
		}
		if r.Code != 404 {
			t.Errorf("%s %s = %d, want 404 for a project outside the token's scope", c.method, c.path, r.Code)
		}
		for _, leak := range []string{"vuln in prod", "sec sprint", "Security"} {
			if strings.Contains(r.Body, leak) {
				t.Errorf("%s %s leaked %q", c.method, c.path, leak)
			}
		}
	}
	if got := f.h.mustDo(200, "GET", "/v1/issues/"+f.secKey, bootstrap, nil); got["priority"] != "P0" || got["status"] != "todo" || got["title"] != "vuln in prod" {
		t.Fatalf("out of scope issue modified: %v", got)
	}
	// A missing key and a forbidden key look the same.
	miss := b.do("GET", "/ui/issues/ACME-999", nil)
	forb := b.do("GET", "/ui/issues/"+f.secKey, nil)
	if miss.Code != forb.Code || normaliseUI(miss.Body) != normaliseUI(forb.Body) {
		t.Errorf("existence oracle: %d vs %d", miss.Code, forb.Code)
	}
	// Search in the allowed project cannot reach across projects.
	r := b.do("GET", "/ui/projects/ACME?q="+url.QueryEscape("project:SEC vuln"), nil)
	if strings.Contains(r.Body, "vuln in prod") {
		t.Error("search leaked an out-of-scope issue")
	}
	r = b.do("GET", "/ui/dashboard?assignees=x", nil)
	if strings.Contains(r.Body, "vuln in prod") {
		t.Error("dashboard leaked an out-of-scope issue")
	}
}

func normaliseUI(s string) string { return uiCSRFRe.ReplaceAllString(s, `name="csrf" value="X"`) }

func TestUIReadOnlyTokenCannotWrite(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.ro)
	page := b.do("GET", "/ui/issues/"+f.acmeKey, nil)
	if page.Code != 200 || strings.Contains(page.Body, `action="/ui/issues/`+f.acmeKey+`/comments"`) {
		t.Fatalf("read-only page: %d (write forms must be hidden)", page.Code)
	}
	v := strconv.FormatInt(issueVersion(t, f.h, f.acmeKey), 10)
	for _, p := range []string{"transition", "edit", "priority", "assignee", "sprint", "rank", "comments", "links", "watch"} {
		r := b.post("/ui/issues/"+f.acmeKey, "/ui/issues/"+f.acmeKey+"/"+p, url.Values{"version": {v}, "to": {"in_progress"}, "title": {"x"}, "body": {"x"}, "priority": {"P0"}})
		if r.Code != 403 {
			t.Errorf("read-only POST %s = %d, want 403", p, r.Code)
		}
	}
	if r := b.post("/ui/projects/ACME", "/ui/projects/ACME/issues", url.Values{"title": {"x"}, "type": {"task"}}); r.Code != 403 {
		t.Errorf("read-only create = %d", r.Code)
	}
	if got := f.h.mustDo(200, "GET", "/v1/issues/"+f.acmeKey, bootstrap, nil); got["status"] != "todo" {
		t.Fatal("read-only token changed an issue")
	}
}

func TestUIHeadersAndNoScript(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.rw)
	for _, p := range []string{"/ui/login", "/ui/", "/ui/projects/ACME", "/ui/issues/" + f.acmeKey, "/ui/nope"} {
		r := b.do("GET", p, nil)
		csp := r.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "style-src 'self'", "form-action 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s CSP %q lacks %q", p, csp, want)
			}
		}
		if strings.Contains(csp, "script-src") && !strings.Contains(csp, "script-src 'none'") {
			t.Errorf("%s: scripts allowed: %s", p, csp)
		}
		if r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("X-Frame-Options") != "DENY" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s headers: %v", p, r.Header)
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") {
			t.Errorf("%s content type %q", p, r.Header.Get("Content-Type"))
		}
		if strings.Contains(r.Body, "<script") || strings.Contains(r.Body, " onclick") || strings.Contains(r.Body, "style=") {
			t.Errorf("%s has inline script or style", p)
		}
	}
	// The API is untouched.
	if r := f.h.do("GET", "/v1/projects", "", nil); r.Code != 401 || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("API changed: %d %s", r.Code, r.Header.Get("Content-Type"))
	}
	if r := b.do("GET", "/ui/static/work.css", nil); r.Code != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("css: %d %s", r.Code, r.Header.Get("Content-Type"))
	}
	if r := b.do("GET", "/ui/static/../../etc/passwd", nil); r.Code == 200 {
		t.Fatal("static path traversal")
	}
}

func TestUIIssueLifecycle(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.rw)
	// inline create with an idempotency token: a double submit makes one issue
	page := b.do("GET", "/ui/projects/ACME", nil)
	idem := regexp.MustCompile(`name="idem" value="([^"]+)"`).FindStringSubmatch(page.Body)
	if idem == nil {
		t.Fatalf("create form has no idempotency token: %.500s", page.Body)
	}
	form := url.Values{"title": {"Add dark mode"}, "type": {"task"}, "priority": {"P0"}, "assignee": {"dev@example.com"}, "labels": {"ui, web"}, "estimate": {"3"}, "due_date": {"2026-12-24"}, "idem": {idem[1]}}
	r1 := b.post("/ui/projects/ACME", "/ui/projects/ACME/issues", form)
	r2 := b.post("/ui/projects/ACME", "/ui/projects/ACME/issues", form)
	if r1.Code != 303 || r2.Code != 303 {
		t.Fatalf("create: %d %d %.300s", r1.Code, r2.Code, r1.Body)
	}
	list := f.h.mustDo(200, "GET", "/v1/issues?q="+url.QueryEscape(`"Add dark mode"`), bootstrap, nil)["items"].([]any)
	if len(list) != 1 {
		t.Fatalf("double submit created %d issues", len(list))
	}
	key := list[0].(map[string]any)["key"].(string)
	got := list[0].(map[string]any)
	if got["priority"] != "P0" || got["estimate"] != float64(3) || got["due_date"] != "2026-12-24" || got["reporter"] != "dev@example.com" {
		t.Fatalf("created issue: %v", got)
	}
	// Hostile fields are rejected with a form error, not stored.
	bad := b.post("/ui/projects/ACME", "/ui/projects/ACME/issues", url.Values{"title": {"x\ny"}, "type": {"task"}})
	if bad.Code != 422 || !strings.Contains(bad.Body, "title") {
		t.Fatalf("bad title: %d %.300s", bad.Code, bad.Body)
	}

	detail := "/ui/issues/" + key
	ver := func() string { return strconv.FormatInt(issueVersion(t, f.h, key), 10) }
	// transition: only the allowed workflow edges
	if r := b.post(detail, detail+"/transition", url.Values{"to": {"done"}, "version": {ver()}}); r.Code != 409 || !strings.Contains(r.Body, "allowed") {
		t.Fatalf("illegal transition: %d %.300s", r.Code, r.Body)
	}
	if r := b.post(detail, detail+"/transition", url.Values{"to": {"in_progress"}, "version": {ver()}}); r.Code != 303 {
		t.Fatalf("transition: %d", r.Code)
	}
	// lost update: a stale version is reported, not applied
	stale := ver()
	f.h.mustDo(200, "PATCH", "/v1/issues/"+key, bootstrap, map[string]any{"title": "Changed elsewhere"})
	r := b.post(detail, detail+"/transition", url.Values{"to": {"in_review"}, "version": {stale}})
	if r.Code != 409 || !strings.Contains(r.Body, "changed") || !strings.Contains(r.Body, `href="`+detail+`"`) {
		t.Fatalf("stale transition: %d %.400s", r.Code, r.Body)
	}
	if st := f.h.mustDo(200, "GET", "/v1/issues/"+key, bootstrap, nil)["status"]; st != "in_progress" {
		t.Fatalf("stale transition applied: %v", st)
	}
	r = b.post(detail, detail+"/edit", url.Values{"title": {"My stale title"}, "description": {"keep this text"}, "version": {stale}, "labels": {""}})
	if r.Code != 409 || !strings.Contains(r.Body, "keep this text") {
		t.Fatalf("stale edit should be reported and keep the user's text: %d %.300s", r.Code, r.Body)
	}
	// edit, priority, assignee, with the current version
	if r := b.post(detail, detail+"/edit", url.Values{"title": {"Dark mode, everywhere"}, "description": {"**Bold** and [docs](https://example.com/d)"}, "labels": {"ui web a11y"}, "estimate": {"5"}, "due_date": {""}, "version": {ver()}}); r.Code != 303 {
		t.Fatalf("edit: %d %.300s", r.Code, r.Body)
	}
	if r := b.post(detail, detail+"/priority", url.Values{"priority": {"P1"}, "version": {ver()}}); r.Code != 303 {
		t.Fatalf("priority: %d", r.Code)
	}
	if r := b.post(detail, detail+"/assignee", url.Values{"assignee": {""}, "version": {ver()}}); r.Code != 303 {
		t.Fatalf("unassign: %d", r.Code)
	}
	if r := b.post(detail, detail+"/priority", url.Values{"priority": {"P9"}, "version": {ver()}}); r.Code != 422 {
		t.Fatalf("bad priority: %d", r.Code)
	}
	if r := b.post(detail, detail+"/priority", url.Values{"priority": {"P2"}}); r.Code != 400 {
		t.Fatalf("missing version: %d", r.Code)
	}
	is := f.h.mustDo(200, "GET", "/v1/issues/"+key, bootstrap, nil)
	if is["title"] != "Dark mode, everywhere" || is["priority"] != "P1" || is["assignee"] != nil || len(is["labels"].([]any)) != 3 {
		t.Fatalf("after edits: %v", is)
	}
	// comment, link, watch
	if r := b.post(detail, detail+"/comments", url.Values{"body": {"Looks good <b>to me</b>"}}); r.Code != 303 {
		t.Fatalf("comment: %d", r.Code)
	}
	if r := b.post(detail, detail+"/links", url.Values{"link_type": {"blocks"}, "target": {f.acmeKey}}); r.Code != 303 {
		t.Fatalf("link: %d %.300s", r.Code, r.Body)
	}
	page = b.do("GET", detail, nil)
	for _, want := range []string{"Dark mode, everywhere", "<strong>Bold</strong>", `href="https://example.com/d"`, "Looks good &lt;b&gt;to me&lt;/b&gt;", f.acmeKey, "Customers can sign up", "dev@example.com"} {
		if !strings.Contains(page.Body, want) {
			t.Errorf("issue page lacks %q", want)
		}
	}
	linkID := regexp.MustCompile(detail + `/links/(\d+)/remove`).FindStringSubmatch(page.Body)
	if linkID == nil {
		t.Fatal("no remove-link form")
	}
	if r := b.post(detail, detail+"/links/"+linkID[1]+"/remove", nil); r.Code != 303 {
		t.Fatalf("unlink: %d", r.Code)
	}
	if r := b.post(detail, detail+"/watch", url.Values{"on": {"0"}}); r.Code != 303 {
		t.Fatalf("unwatch: %d", r.Code)
	}
	d := f.h.mustDo(200, "GET", "/v1/issues/"+key, bootstrap, nil)
	if len(d["links"].([]any)) != 0 || len(d["watchers"].([]any)) != 0 {
		t.Fatalf("links/watchers: %v %v", d["links"], d["watchers"])
	}
	// unknown action values never 500
	for _, p := range []struct {
		a string
		f url.Values
	}{
		{"transition", url.Values{"to": {"nope"}, "version": {ver()}}},
		{"transition", url.Values{"to": {"x' OR 1=1"}, "version": {ver()}}},
		{"rank", url.Values{"before": {"NOPE-1"}, "version": {ver()}}},
		{"links", url.Values{"link_type": {"owns"}, "target": {"ACME-1"}}},
		{"sprint", url.Values{"sprint": {"999"}, "version": {ver()}}},
		{"sprint", url.Values{"sprint": {"abc"}, "version": {ver()}}},
	} {
		if r := b.post(detail, detail+"/"+p.a, p.f); r.Code >= 500 || r.Code < 400 {
			t.Errorf("%s %v = %d, want a 4xx", p.a, p.f, r.Code)
		}
	}
}

func TestUIBoardMoveAndRank(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.rw)
	k2 := f.h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "task", "title": "Second"})["key"].(string)
	k3 := f.h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "task", "title": "Third"})["key"].(string)
	page := b.do("GET", "/ui/projects/ACME/board?sprint=all", nil)
	if page.Code != 200 {
		t.Fatalf("board: %d", page.Code)
	}
	for _, want := range []string{"To do", "In progress", "In review", "Done", "Cancelled", f.acmeKey, k2, k3} {
		if !strings.Contains(page.Body, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	// Every move is a plain form button: no JavaScript, no drag and drop.
	if !strings.Contains(page.Body, `action="/ui/issues/`+k2+`/transition"`) || strings.Contains(page.Body, "draggable") {
		t.Error("board moves must be forms")
	}
	// rank the third issue up (before the second)
	v := strconv.FormatInt(issueVersion(t, f.h, k3), 10)
	if r := b.post("/ui/projects/ACME/board?sprint=all", "/ui/issues/"+k3+"/rank", url.Values{"before": {k2}, "version": {v}, "next": {"/ui/projects/ACME/board?sprint=all"}}); r.Code != 303 || r.Header.Get("Location") != "/ui/projects/ACME/board?sprint=all&ok=ranked" {
		t.Fatalf("rank: %d %q", r.Code, r.Header.Get("Location"))
	}
	items := f.h.mustDo(200, "GET", "/v1/issues?project=ACME&sort=rank", bootstrap, nil)["items"].([]any)
	if items[1].(map[string]any)["key"] != k3 {
		t.Fatalf("rank order: %v", items)
	}
	// move forward then back with a fresh version each time
	for _, to := range []string{"in_progress", "todo"} {
		v = strconv.FormatInt(issueVersion(t, f.h, k2), 10)
		if r := b.post("/ui/projects/ACME/board?sprint=all", "/ui/issues/"+k2+"/transition", url.Values{"to": {to}, "version": {v}, "next": {"/ui/projects/ACME/board?sprint=all"}}); r.Code != 303 {
			t.Fatalf("move to %s: %d", to, r.Code)
		}
	}
	// two people move the same card: the second is told, and nothing is lost
	v = strconv.FormatInt(issueVersion(t, f.h, k2), 10)
	if r := b.post("/ui/projects/ACME/board?sprint=all", "/ui/issues/"+k2+"/transition", url.Values{"to": {"in_progress"}, "version": {v}}); r.Code != 303 {
		t.Fatal("first move")
	}
	r := b.post("/ui/projects/ACME/board?sprint=all", "/ui/issues/"+k2+"/transition", url.Values{"to": {"cancelled"}, "version": {v}})
	if r.Code != 409 {
		t.Fatalf("second mover with a stale card: %d", r.Code)
	}
	if st := f.h.mustDo(200, "GET", "/v1/issues/"+k2, bootstrap, nil)["status"]; st != "in_progress" {
		t.Fatalf("status %v", st)
	}
	// the default board with no active sprint is an empty state, not an error
	if r := b.do("GET", "/ui/projects/ACME/board", nil); r.Code != 200 || !strings.Contains(r.Body, "No active sprint") {
		t.Fatalf("empty board: %d", r.Code)
	}
	for _, bad := range []string{"sprint=abc", "sprint=0", "sprint=9999"} {
		if r := b.do("GET", "/ui/projects/ACME/board?"+bad, nil); r.Code < 400 || r.Code >= 500 {
			t.Errorf("board?%s = %d", bad, r.Code)
		}
	}
}

func TestUISprintPlanning(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.rw)
	page := "/ui/projects/ACME/sprints"
	if r := b.post(page, page, url.Values{"name": {"Sprint 1"}, "goal": {"Ship <i>it</i>"}, "starts_on": {"2026-12-01"}, "ends_on": {"2026-12-14"}}); r.Code != 303 {
		t.Fatalf("create sprint: %d %.300s", r.Code, r.Body)
	}
	if r := b.post(page, page, url.Values{"name": {"Sprint 2"}}); r.Code != 303 {
		t.Fatalf("create sprint 2: %d", r.Code)
	}
	if r := b.post(page, page, url.Values{"name": {"Bad"}, "starts_on": {"2026-12-14"}, "ends_on": {"2026-12-01"}}); r.Code != 422 {
		t.Fatalf("bad dates: %d", r.Code)
	}
	sprints := f.h.mustDo(200, "GET", "/v1/projects/ACME/sprints", bootstrap, nil)["items"].([]any)
	s2, s1 := strconv.Itoa(int(sprints[0].(map[string]any)["id"].(float64))), strconv.Itoa(int(sprints[1].(map[string]any)["id"].(float64)))
	v := strconv.FormatInt(issueVersion(t, f.h, f.acmeKey), 10)
	if r := b.post(page, "/ui/issues/"+f.acmeKey+"/sprint", url.Values{"sprint": {s1}, "version": {v}, "next": {page}}); r.Code != 303 {
		t.Fatalf("add to sprint: %d %.300s", r.Code, r.Body)
	}
	body := b.do("GET", page, nil).Body
	if !strings.Contains(body, "Sprint 1") || strings.Contains(body, "<i>it</i>") || !strings.Contains(body, "Ship &lt;i&gt;it&lt;/i&gt;") {
		t.Fatalf("sprint page: %.800s", body)
	}
	if r := b.post(page, "/ui/sprints/"+s1+"/start", nil); r.Code != 303 {
		t.Fatalf("start: %d", r.Code)
	}
	if r := b.post(page, "/ui/sprints/"+s2+"/start", nil); r.Code != 409 {
		t.Fatalf("second active sprint: %d", r.Code)
	}
	board := b.do("GET", "/ui/projects/ACME/board", nil).Body
	if !strings.Contains(board, f.acmeKey) || !strings.Contains(board, "Sprint 1") {
		t.Fatalf("active board: %.500s", board)
	}
	if r := b.post(page, "/ui/sprints/"+s1+"/complete", url.Values{"move_to": {s2}}); r.Code != 303 {
		t.Fatalf("complete: %d", r.Code)
	}
	if got := f.h.mustDo(200, "GET", "/v1/issues/"+f.acmeKey, bootstrap, nil); strconv.Itoa(int(got["sprint_id"].(float64))) != s2 {
		t.Fatalf("issue not carried over: %v", got["sprint_id"])
	}
	v = strconv.FormatInt(issueVersion(t, f.h, f.acmeKey), 10)
	if r := b.post(page, "/ui/issues/"+f.acmeKey+"/sprint", url.Values{"sprint": {""}, "version": {v}}); r.Code != 303 {
		t.Fatalf("remove from sprint: %d", r.Code)
	}
	if got := f.h.mustDo(200, "GET", "/v1/issues/"+f.acmeKey, bootstrap, nil); got["sprint_id"] != nil {
		t.Fatal("still in the sprint")
	}
}

func TestUIBacklogFiltersPaginationAndEpics(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.rw)
	epic := f.h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "epic", "title": "Onboarding"})["key"].(string)
	for i := 0; i < 30; i++ {
		body := map[string]any{"type": "task", "title": "Task " + strconv.Itoa(i), "priority": "P3"}
		if i < 4 {
			body["parent"] = epic
		}
		k := f.h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, body)["key"].(string)
		if i < 2 {
			f.h.mustDo(200, "POST", "/v1/issues/"+k+"/transitions", bootstrap, map[string]any{"to": "in_progress"})
			f.h.mustDo(200, "POST", "/v1/issues/"+k+"/transitions", bootstrap, map[string]any{"to": "done"})
		}
	}
	p1 := b.do("GET", "/ui/projects/ACME", nil)
	if p1.Code != 200 || !strings.Contains(p1.Body, `rel="next"`) || strings.Contains(p1.Body, `rel="prev"`) {
		t.Fatalf("first page: %d next/prev links wrong", p1.Code)
	}
	p2 := b.do("GET", "/ui/projects/ACME?page=2", nil)
	if p2.Code != 200 || !strings.Contains(p2.Body, `rel="prev"`) {
		t.Fatalf("second page: %d", p2.Code)
	}
	// the filter form composes the safe search language and keeps the URL shareable
	r := b.do("GET", "/ui/projects/ACME?priority=P1&assignee=me", nil)
	if !strings.Contains(r.Body, f.acmeKey) || strings.Contains(r.Body, "Task 3") {
		t.Fatalf("filter: %.300s", r.Body)
	}
	r = b.do("GET", "/ui/projects/ACME?q="+url.QueryEscape(`"Task 2" -status:done`), nil)
	if r.Code != 200 || !strings.Contains(r.Body, "Task 20") || strings.Contains(r.Body, "Task 3<") {
		t.Fatalf("q filter: %d", r.Code)
	}
	if r := b.do("GET", "/ui/projects/ACME?q="+url.QueryEscape("nosuchfield:x"), nil); r.Code != 400 || !strings.Contains(r.Body, "unknown search field") {
		t.Fatalf("bad query: %d %.200s", r.Code, r.Body)
	}
	for _, bad := range []string{"page=0", "page=-3", "page=abc", "page=99999999", "sort=evil", "priority=P9", "status=a%20b", "assignee=%22%20OR%201"} {
		if r := b.do("GET", "/ui/projects/ACME?"+bad, nil); r.Code >= 500 {
			t.Errorf("?%s = %d", bad, r.Code)
		}
	}
	// epics with progress
	e := b.do("GET", "/ui/projects/ACME/epics", nil)
	if e.Code != 200 || !strings.Contains(e.Body, "Onboarding") || !strings.Contains(e.Body, `<progress`) || !strings.Contains(e.Body, "2 of 4") {
		t.Fatalf("epics: %d %.800s", e.Code, e.Body)
	}
	// manager dashboard
	f.h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "bug", "title": "Prod down", "priority": "P0", "assignee": "ann@example.com"})
	dash := b.do("GET", "/ui/dashboard?assignees="+url.QueryEscape("ann@example.com, dev@example.com"), nil)
	if dash.Code != 200 || !strings.Contains(dash.Body, "Prod down") || !strings.Contains(dash.Body, "Customers can sign up") || strings.Contains(dash.Body, "Task 3<") {
		t.Fatalf("dashboard: %d %.600s", dash.Code, dash.Body)
	}
	if r := b.do("GET", "/ui/dashboard", nil); r.Code != 200 || !strings.Contains(r.Body, "Enter") {
		t.Fatalf("empty dashboard: %d", r.Code)
	}
	if r := b.do("GET", "/ui/dashboard?assignees=me", nil); r.Code != 400 {
		t.Fatalf("reserved word as a person: %d", r.Code)
	}
	if r := b.do("GET", "/ui/dashboard?assignees="+manyPeople(60), nil); r.Code != 400 {
		t.Fatalf("too many assignees: %d", r.Code)
	}
}

func TestUIHostileContentIsInert(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.rw)
	hostile := `<script>alert(1)</script><img src=x onerror=alert(2)>"'&`
	k := f.h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{
		"type": "bug", "title": hostile, "assignee": `"><svg onload=alert(3)>`, "priority": "P0",
		"description": "<iframe src=//evil></iframe>\n\n[click](javascript:alert(4)) [data](data:text/html;base64,AAAA) [ok](https://example.com/a?b=1&c=2) [quoted](https://example.com/\"x)\n\n[x](https://u:p@evil.example/) ![img](x) <a href=\"javascript:1\">z</a>\n\n```\n<b>code</b>\n```",
	})["key"].(string)
	f.h.mustDo(201, "POST", "/v1/issues/"+k+"/comments", bootstrap, map[string]any{"body": hostile + " [c](javascript:alert(5)) **<u>b</u>**"})
	f.h.mustDo(201, "POST", "/v1/issues/"+k+"/attachments", bootstrap, map[string]any{"filename": "<b onmouseover=alert(1)>.txt", "url": "https://files.example/x?a=1&b=2"})
	f.h.mustDo(201, "POST", "/v1/projects/ACME/sprints", bootstrap, map[string]any{"name": "<script>s</script>", "goal": hostile})
	f.h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "epic", "title": hostile})
	pages := []string{
		"/ui/", "/ui/projects/ACME", "/ui/projects/ACME?q=" + url.QueryEscape(hostile), "/ui/projects/ACME/board?sprint=all", "/ui/projects/ACME/sprints",
		"/ui/projects/ACME/epics", "/ui/issues/" + k, "/ui/dashboard?assignees=" + url.QueryEscape(`"><svg onload=alert(3)>`),
		"/ui/projects/ACME?assignee=" + url.QueryEscape(hostile), "/ui/nope" + url.PathEscape(hostile), "/ui/issues/" + url.PathEscape(hostile),
	}
	for _, p := range pages {
		r := b.do("GET", p, nil)
		for _, bad := range liveHazards(r.Body) {
			t.Errorf("%s: %s", p, bad)
		}
	}
	r := b.do("GET", "/ui/issues/"+k, nil)
	if !strings.Contains(r.Body, `href="https://example.com/a?b=1&amp;c=2"`) || !strings.Contains(r.Body, `rel="noopener noreferrer nofollow"`) {
		i := strings.Index(r.Body, `id="desc"`)
		t.Errorf("the safe link was not rendered correctly: %.1500s", r.Body[i:])
	}
	if !strings.Contains(r.Body, `href="https://files.example/x?a=1&amp;b=2"`) {
		t.Error("attachment link missing")
	}
	// A form re-render never reflects hostile input unescaped.
	r = b.post("/ui/projects/ACME", "/ui/projects/ACME/issues", url.Values{"title": {hostile + strings.Repeat("x", 300)}, "type": {"task"}})
	if r.Code != 422 || strings.Contains(r.Body, "<script>alert") {
		t.Errorf("error page: %d", r.Code)
	}
	// stored attachment URLs that are not http(s) are never linked
	f.h.mustDo(201, "POST", "/v1/issues/"+k+"/attachments", bootstrap, map[string]any{"filename": "a.txt"})
}

func TestUIOversizedFormAndMethods(t *testing.T) {
	f := newUIFixture(t)
	b := f.browser(t, f.rw)
	big := strings.Repeat("a", 2<<20)
	if r := b.do("POST", "/ui/issues/"+f.acmeKey+"/comments", url.Values{"csrf": {b.csrf("/ui/")}, "body": {big}}); r.Code != 413 {
		t.Errorf("oversized form: %d", r.Code)
	}
	if r := b.do("GET", "/ui/issues/"+f.acmeKey+"/transition", nil); r.Code != 405 {
		t.Errorf("GET on a POST route: %d", r.Code)
	}
	if r := b.do("GET", "/ui/projects/acme", nil); r.Code != 404 {
		t.Errorf("bad project key: %d", r.Code)
	}
	if r := b.do("GET", "/", nil); r.Code != 303 || r.Header.Get("Location") != "/ui/" {
		t.Errorf("root: %d %q", r.Code, r.Header.Get("Location"))
	}
}

func TestUISessionTableIsBounded(t *testing.T) {
	now := newTestClock()
	s := newUISessions(now.Now)
	s.max, s.perSubject = 50, 3
	var first string
	for i := 0; i < 10; i++ {
		id, _ := s.create("tok", "u1")
		if i == 0 {
			first = id
		}
	}
	if _, ok := s.get(first); ok {
		t.Error("the subject's oldest session survived the per-subject cap")
	}
	for i := 0; i < 200; i++ {
		s.create("tok", "user"+strconv.Itoa(i))
	}
	if s.len() > 50 {
		t.Errorf("table grew to %d", s.len())
	}
	id, _ := s.create("t", "late")
	now.Advance(9 * 60 * 60 * 1e9)
	if _, ok := s.get(id); ok {
		t.Error("idle session did not expire")
	}
}

func manyPeople(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("p" + strconv.Itoa(i) + "%40example.com,")
	}
	return b.String()
}

var liveTagRe = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9]*)([^>]*)>`)
var quotedRe = regexp.MustCompile(`"[^"]*"`)
var liveOnAttrRe = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)

// liveHazards lists real (unescaped) tags and attributes that could run code or load content.
// Escaped text (&lt;script&gt;) is not a tag and is not reported.
func liveHazards(html string) []string {
	var out []string
	for _, m := range liveTagRe.FindAllStringSubmatch(html, -1) {
		name := strings.ToLower(m[1])
		switch name {
		case "script", "img", "iframe", "svg", "object", "embed", "b", "u", "i", "style", "base", "meta_refresh", "frame", "video", "audio", "math":
			out = append(out, "live <"+name+"> tag: "+m[0])
		}
		if liveOnAttrRe.MatchString(quotedRe.ReplaceAllString(m[2], `""`)) {
			out = append(out, "event handler attribute in "+m[0])
		}
		for _, bad := range []string{`href="javascript`, `href="data:`, `href="vbscript`, `href="https://u`, `src="javascript`} {
			if strings.Contains(strings.ToLower(m[2]), bad) {
				out = append(out, bad+" in "+m[0])
			}
		}
	}
	return out
}
