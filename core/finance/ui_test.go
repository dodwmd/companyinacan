package finance

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// All names, ids and amounts are fake. The UI is exercised through the real combined handler
// (API under /v1, UI everywhere else) over the same Store the JSON API uses.

type uiEnv struct {
	*env
}

func newUIEnv(t *testing.T, opts ...envOpt) *uiEnv {
	t.Helper()
	opts = append([]envOpt{func(c *Config, _ *Options) {
		c.UI.Enabled, c.UI.InsecureCookies = true, true
	}}, opts...)
	return &uiEnv{newEnv(t, opts...)}
}

type client struct {
	t       *testing.T
	e       *env
	cookies map[string]string
	csrf    string
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (u *uiEnv) client(t *testing.T) *client {
	return &client{t: t, e: u.env, cookies: map[string]string{}}
}

func (c *client) do(method, path string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	r.RemoteAddr = "192.0.2.10:1234"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	for k, v := range c.cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	c.e.h.ServeHTTP(w, r)
	for _, ck := range w.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck.Value
		}
	}
	if m := csrfRe.FindStringSubmatch(w.Body.String()); m != nil && w.Code == 200 && !strings.Contains(path, "/login") {
		c.csrf = m[1]
	}
	return w
}

func (c *client) get(path string) *httptest.ResponseRecorder { return c.do("GET", path, nil) }

// post adds the session's CSRF token (taken from the last page that carried one).
func (c *client) post(path string, form url.Values) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	if form.Get("csrf") == "" {
		form.Set("csrf", c.csrf)
	}
	return c.do("POST", path, form, "Sec-Fetch-Site", "same-origin")
}

func (c *client) loginWith(tok string) *httptest.ResponseRecorder {
	c.t.Helper()
	w := c.get("/login")
	m := csrfRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		c.t.Fatalf("no csrf on login page: %s", w.Body.String())
	}
	return c.do("POST", "/login", url.Values{"token": {tok}, "csrf": {m[1]}}, "Sec-Fetch-Site", "same-origin")
}

// login signs in with a stored token and loads a page so the session CSRF token is known.
func (u *uiEnv) login(t *testing.T, tok string) *client {
	t.Helper()
	c := u.client(t)
	if w := c.loginWith(tok); w.Code != 303 {
		t.Fatalf("login = %d %s", w.Code, w.Body.String())
	}
	c.get("/")
	if c.csrf == "" {
		t.Fatal("no session csrf token on the home page")
	}
	return c
}

func mustHave(t *testing.T, w *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(w.Body.String(), s) {
			t.Errorf("missing %q in %s", s, w.Body.String())
		}
	}
}

func mustNot(t *testing.T, w *httptest.ResponseRecorder, bad ...string) {
	t.Helper()
	for _, s := range bad {
		if strings.Contains(w.Body.String(), s) {
			t.Errorf("must not contain %q", s)
		}
	}
}

func status(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status = %d, want %d: %s", w.Code, want, w.Body.String())
	}
}

// ---- sessions --------------------------------------------------------------------------------

func TestUILoginSessionLifecycle(t *testing.T) {
	u := newUIEnv(t)
	tok := u.token("user:ann", []string{"finance.read", "finance.write"}, nil, nil, false)

	anon := u.client(t)
	w := anon.get("/")
	status(t, w, 303)
	if w.Header().Get("Location") != "/login" {
		t.Fatalf("redirect to %q", w.Header().Get("Location"))
	}
	for _, h := range []string{"Cache-Control"} {
		if w.Header().Get(h) != "no-store" {
			t.Errorf("%s = %q", h, w.Header().Get(h))
		}
	}
	if csp := anon.get("/login").Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	if w := anon.loginWith("not-a-token"); w.Code != 401 {
		t.Fatalf("wrong token = %d", w.Code)
	}
	w = anon.loginWith(tok)
	status(t, w, 303)
	var sc *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "can_finance_session" {
			sc = ck
		}
	}
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v", sc)
	}
	if strings.Contains(w.Body.String(), tok) || strings.Contains(sc.Value, tok) {
		t.Fatal("the token must never come back to the browser")
	}
	status(t, anon.get("/"), 200)

	// logout ends the session; replaying the old cookie does not resurrect it
	old := anon.cookies["can_finance_session"]
	anon.get("/")
	status(t, anon.post("/logout", nil), 303)
	replay := u.client(t)
	replay.cookies["can_finance_session"] = old
	status(t, replay.get("/"), 303)

	// a presented session id is never adopted at login (fixation)
	fix := u.client(t)
	fix.cookies["can_finance_session"] = "attacker-chosen-session-id"
	fix.loginWith(tok)
	if fix.cookies["can_finance_session"] == "attacker-chosen-session-id" {
		t.Fatal("session fixation: the presented id was adopted")
	}
}

func TestUISecureCookiesByDefault(t *testing.T) {
	u := newUIEnv(t, func(c *Config, _ *Options) { c.UI.InsecureCookies = false })
	tok := u.token("user:ann", []string{"finance.read"}, nil, nil, false)
	c := u.client(t)
	w := c.get("/login")
	found := false
	for _, ck := range w.Result().Cookies() {
		found = true
		if !ck.Secure || !strings.HasPrefix(ck.Name, "__Host-") {
			t.Errorf("cookie %+v must be Secure with the __Host- prefix", ck)
		}
	}
	if !found {
		t.Fatal("no login cookie")
	}
	_ = tok
}

func TestUIForceSecureRejectsInsecureCookies(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UI.Enabled, cfg.UI.InsecureCookies, cfg.UI.ForceSecure = true, true, true
	if err := cfg.Validate(); err == nil {
		t.Fatal("insecure_cookies together with force_secure must be refused")
	}
}

func TestUITimeoutsAndRecheck(t *testing.T) {
	u := newUIEnv(t)
	reader := u.token("user:rita", []string{"finance.read"}, nil, nil, false)
	c := u.login(t, reader)
	u.advance(29 * time.Minute)
	status(t, c.get("/"), 200)
	u.advance(29 * time.Minute) // idle clock restarted by the request above
	status(t, c.get("/"), 200)
	u.advance(31 * time.Minute)
	status(t, c.get("/"), 303) // idle timeout

	// absolute timeout
	c = u.login(t, reader)
	for i := 0; i < 17; i++ {
		u.advance(29 * time.Minute)
		c.get("/")
	}
	status(t, c.get("/"), 303)
}

func TestUIRevokedApproverTokenEndsSessionWithinAMinute(t *testing.T) {
	u := newUIEnv(t)
	var tok struct {
		Token string `json:"token"`
		Info  struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	u.must(201, adminTok, "POST", "/v1/tokens", obj{"kind": "service", "subject": "user:boss", "name": "boss", "scopes": []string{"finance.approve"}}, &tok)
	c := u.login(t, tok.Token)
	status(t, c.get("/"), 200)
	u.must(204, adminTok, "DELETE", "/v1/tokens/"+tok.Info.ID, nil, nil)
	u.advance(61 * time.Second)
	status(t, c.get("/"), 303)

	// a read-only token is re-checked less often, but still within the default window
	var ro struct {
		Token string `json:"token"`
		Info  struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	u.must(201, adminTok, "POST", "/v1/tokens", obj{"kind": "service", "subject": "user:ro", "name": "ro", "scopes": []string{"finance.read"}}, &ro)
	c = u.login(t, ro.Token)
	u.must(204, adminTok, "DELETE", "/v1/tokens/"+ro.Info.ID, nil, nil)
	u.advance(6 * time.Minute)
	status(t, c.get("/"), 303)
}

func TestUIValidCredentialsAreNeverThrottled(t *testing.T) {
	u := newUIEnv(t)
	tok := u.token("user:ann", []string{"finance.read"}, nil, nil, false)
	bad := u.client(t)
	var last int
	for i := 0; i < 30; i++ {
		last = bad.loginWith("wrong-token-" + string(rune('a'+i%20))).Code
	}
	if last != 429 {
		t.Fatalf("a flood of bad tokens ended at %d, want 429", last)
	}
	good := u.client(t)
	if w := good.loginWith(tok); w.Code != 303 {
		t.Fatalf("a valid token after a flood = %d, want 303", w.Code)
	}
}

func TestUICSRFAndFetchMetadata(t *testing.T) {
	u := newUIEnv(t)
	tok := u.token("user:ann", []string{"finance.read", "finance.write"}, nil, nil, false)
	c := u.login(t, tok)
	// missing token
	w := c.do("POST", "/parties", url.Values{"name": {"X"}, "kind": {"customer"}}, "Sec-Fetch-Site", "same-origin")
	status(t, w, 403)
	// cross-site request, even with a valid token
	w = c.do("POST", "/parties", url.Values{"name": {"X"}, "kind": {"customer"}, "csrf": {c.csrf}}, "Sec-Fetch-Site", "cross-site")
	status(t, w, 403)
	// the logout form is CSRF-protected too
	status(t, c.do("POST", "/logout", url.Values{}, "Sec-Fetch-Site", "same-origin"), 403)
	status(t, c.get("/"), 200)
}

func TestUIPublicLinkTokenIsNotLogged(t *testing.T) {
	var buf bytes.Buffer
	u := newUIEnv(t, func(_ *Config, o *Options) { o.Logger = slog.New(slog.NewTextHandler(&buf, nil)) })
	secret := "SECRETLINKTOKENVALUE.abc123"
	status(t, httptestGet(u.h, "/public/invoices/"+secret), 404)
	if strings.Contains(buf.String(), "SECRETLINKTOKENVALUE") {
		t.Fatalf("the request log holds the link token: %s", buf.String())
	}
}

func httptestGet(h http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = "192.0.2.10:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// ---- errors ------------------------------------------------------------------------------------

func TestUIErrorStatuses(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrNotFound, 404}, {ErrForbidden, 403}, {ErrSeparation, 403}, {ErrConflict, 409}, {ErrLocked, 409},
		{ErrVersionMismatch, 412}, {ErrIdempotencyMismatch, 422}, {invalid("x", "bad"), 422},
		{errDeadline(), 503}, {ErrApprovalsUnavailable, 503}, {io.ErrUnexpectedEOF, 500},
	}
	for _, c := range cases {
		if got := uiStatus(c.err); got != c.want {
			t.Errorf("uiStatus(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
