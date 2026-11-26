// Package conformance is the security suite every module web UI must pass. It drives a UI only
// through its HTTP handler, with an in-memory can-core (modclient/fakecore) behind it, so the same
// checks run against every module whatever its internals. Each module has a ui_conformance_test.go
// that describes itself (Module) and calls Run.
//
// What differs between modules on purpose (timeouts, re-check intervals, body limits, the failed
// attempt that is first answered 429) is declared in Module, so a change to any of them fails a test.
package conformance

import (
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

// Env is one freshly built module.
type Env struct {
	H    http.Handler
	Core *fakecore.Core
	// Advance moves the module's clock (sessions, introspection cache and the fake can-core).
	Advance func(time.Duration)
}

// Module describes one web UI.
type Module struct {
	Name string // the module id can-core launches
	// New builds the module. secure is what a deployment runs: Secure cookies.
	New func(t *testing.T, secure bool) *Env
	// Prefix is where the UI is mounted: "" or "/ui".
	Prefix string
	// SessionCookie and CSRFCookie are the names without the __Host- prefix.
	SessionCookie, CSRFCookie string
	// User are the permissions of an ordinary person, Privileged those of one whose session is
	// re-checked more often.
	User, Privileged []string

	Idle, Absolute time.Duration
	// How old the last check of a token session may be before it is made again; 0: every request.
	Recheck, PrivilegedRecheck time.Duration
	// The same for a launch-ticket session.
	SSORecheck, SSOPrivilegedRecheck time.Duration

	// LoginCSRFMaxAge is the lifetime of the sign-in form's cookie, in seconds.
	LoginCSRFMaxAge int
	// FormLimit is the largest body of a signed-in form post.
	FormLimit int
	// FirstLimited is which consecutive failed sign-in from one address is the first answered 429.
	FirstLimited int
	// StepUp is set for a module with irreversible actions: it prepares one as the signed-in
	// administrator and returns where to post it and with what (without the token).
	StepUp func(t *testing.T, e *Env, b *Browser) (path string, form url.Values)
}

const (
	introspectTTL = 30 * time.Second // modclient's cache of a positive answer
	second        = time.Second
)

// Browser is a minimal cookie-keeping client.
type Browser struct {
	t       *testing.T
	m       *Module
	e       *Env
	Cookies map[string]string
	// Secure is how the module under test sets its cookies.
	Secure bool
}

// Do sends one request. hdr are name, value pairs. Unless hdr says otherwise a POST carries
// Sec-Fetch-Site: same-origin, as a browser's own form post does.
func (b *Browser) Do(method, path string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, b.m.Prefix+path, body)
	r.RemoteAddr = "192.0.2.10:1234"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == http.MethodPost {
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			r.Header.Del(hdr[i])
		} else {
			r.Header.Set(hdr[i], hdr[i+1])
		}
	}
	for k, v := range b.Cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	b.e.H.ServeHTTP(w, r)
	for _, ck := range w.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(b.Cookies, ck.Name)
		} else {
			b.Cookies[ck.Name] = ck.Value
		}
	}
	return w
}

// Get is Do for a GET.
func (b *Browser) Get(path string) *httptest.ResponseRecorder { return b.Do("GET", path, nil) }

var (
	csrfRe   = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	ticketRe = regexp.MustCompile(`name="ticket" value="([^"]+)"`)
)

// CSRF loads a page and returns the CSRF token of its first form.
func (b *Browser) CSRF(path string) string {
	b.t.Helper()
	w := b.Get(path)
	m := csrfRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		b.t.Fatalf("no csrf field on %s (%d): %.300s", path, w.Code, w.Body)
	}
	return html.UnescapeString(m[1])
}

// Name is the cookie name as the module sets it.
func (b *Browser) Name(base string) string {
	if b.Secure {
		return "__Host-" + base
	}
	return base
}

// Alive reports whether the browser's session still opens the home page.
func (b *Browser) Alive() bool {
	b.t.Helper()
	switch w := b.Get("/"); w.Code {
	case http.StatusOK:
		return true
	case http.StatusSeeOther:
		if !strings.HasPrefix(w.Header().Get("Location"), b.m.Prefix+"/login") {
			b.t.Fatalf("home redirects to %q", w.Header().Get("Location"))
		}
		return false
	default:
		b.t.Fatalf("home: %d %.300s", w.Code, w.Body)
		return false
	}
}

// Login posts the sign-in form with a token.
func (b *Browser) Login(token string, hdr ...string) *httptest.ResponseRecorder {
	b.t.Helper()
	// A signed-in browser is sent away from the form, so read it without the session cookie and
	// post it with the cookie, as a person with the form open in another tab does.
	held, name := b.Cookies[b.Name(b.m.SessionCookie)], b.Name(b.m.SessionCookie)
	delete(b.Cookies, name)
	c := b.CSRF("/login")
	if held != "" {
		b.Cookies[name] = held
	}
	return b.Do("POST", "/login", url.Values{"token": {token}, "csrf": {c}}, hdr...)
}

// MustLogin signs in or fails the test.
func (b *Browser) MustLogin(token string) {
	b.t.Helper()
	if w := b.Login(token); w.Code != http.StatusSeeOther {
		b.t.Fatalf("login: %d %.300s", w.Code, w.Body)
	}
}

type suite struct {
	m *Module
	t *testing.T
	e *Env
	n int
}

func start(t *testing.T, m *Module, secure bool) *suite {
	t.Helper()
	return &suite{m: m, t: t, e: m.New(t, secure)}
}

func (s *suite) browser() *Browser {
	return &Browser{t: s.t, m: s.m, e: s.e, Cookies: map[string]string{}}
}

// token registers a fresh personal access token.
func (s *suite) token(subject string, perms []string) string {
	s.n++
	tok := "canpat_conformance-0123456789-" + string(rune('a'+s.n))
	s.e.Core.AddToken(tok, fakecore.Token{Subject: subject, Permissions: perms, Expires: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)})
	return tok
}

func (s *suite) signedIn(perms []string) (*Browser, string) {
	s.t.Helper()
	tok := s.token("user:alice", perms)
	b := s.browser()
	b.Secure = false
	b.MustLogin(tok)
	if !b.Alive() {
		s.t.Fatal("no session after login")
	}
	return b, tok
}

func (s *suite) ticket(subject string, perms []string, g ...func(*fakecore.Grant)) string {
	gr := fakecore.Grant{Subject: subject, Name: "Person " + subject, Permissions: perms}
	for _, f := range g {
		f(&gr)
	}
	return s.e.Core.NewTicket(gr)
}

// interstitial opens the launch link in the launching browser.
func (s *suite) interstitial(b *Browser, ticket string) *httptest.ResponseRecorder {
	s.t.Helper()
	name := "can_launch_" + s.m.Name
	if b.Secure {
		name = "__Secure-" + name
	}
	b.Cookies[name] = s.e.Core.Binding(ticket)
	return b.Get("/login/ticket?t=" + ticket)
}

func formValues(s *suite, page *httptest.ResponseRecorder) url.Values {
	s.t.Helper()
	c, tk := csrfRe.FindStringSubmatch(page.Body.String()), ticketRe.FindStringSubmatch(page.Body.String())
	if c == nil || tk == nil {
		s.t.Fatalf("no ticket form (%d): %.400s", page.Code, page.Body)
	}
	return url.Values{"csrf": {c[1]}, "ticket": {tk[1]}}
}

func (s *suite) land(b *Browser, ticket string) *httptest.ResponseRecorder {
	s.t.Helper()
	page := s.interstitial(b, ticket)
	if page.Code != http.StatusOK {
		return page
	}
	return b.Do("POST", "/login/ticket", formValues(s, page))
}

func (s *suite) mustLand(perms []string) *Browser {
	s.t.Helper()
	b := s.browser()
	if w := s.land(b, s.ticket("user:alice", perms)); w.Code != http.StatusSeeOther {
		s.t.Fatalf("ticket login: %d %.300s", w.Code, w.Body)
	}
	if !b.Alive() {
		s.t.Fatal("no session after the ticket")
	}
	return b
}

func want(t *testing.T, what string, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Errorf("%s: status %d, want %d: %.200s", what, w.Code, code, w.Body)
	}
}

// crossSite are the headers of posts that did not come from the UI's own pages.
var crossSite = map[string][]string{
	"cross-site":          {"Sec-Fetch-Site", "cross-site"},
	"same-site sibling":   {"Sec-Fetch-Site", "same-site"},
	"foreign Origin":      {"Sec-Fetch-Site", "", "Origin", "https://evil.example"},
	"foreign Origin only": {"Origin", "https://evil.example"},
	"Origin null alone":   {"Sec-Fetch-Site", "", "Origin", "null"},
	"Origin null, none":   {"Sec-Fetch-Site", "none", "Origin", "null"},
	"unparsable Origin":   {"Origin", "https://%zz"},
}

// Run runs the whole suite against a module.
func Run(t *testing.T, m Module) {
	t.Run("csrf", func(t *testing.T) {
		s := start(t, &m, false)
		b, tok := s.signedIn(m.User)
		other := s.browser()
		other.MustLogin(tok)
		for name, csrf := range map[string]string{"none": "", "wrong": "nope", "another session's": other.CSRF("/")} {
			want(t, "csrf "+name, b.Do("POST", "/logout", url.Values{"csrf": {csrf}}), http.StatusForbidden)
			if !b.Alive() {
				t.Fatalf("csrf %s: the refused post logged out", name)
			}
		}
		want(t, "login without the cookie", s.browser().Do("POST", "/login", url.Values{"token": {tok}, "csrf": {"x"}}), http.StatusForbidden)
		nb := s.browser()
		nb.CSRF("/login")
		want(t, "login with a wrong token", nb.Do("POST", "/login", url.Values{"token": {tok}, "csrf": {"x"}}), http.StatusForbidden)
		if nb.Cookies[m.SessionCookie] != "" {
			t.Error("a refused login set a session")
		}
		lb := s.browser()
		page := s.interstitial(lb, s.ticket("user:alice", m.User))
		f := formValues(s, page)
		f.Set("csrf", "x")
		want(t, "ticket with a wrong token", lb.Do("POST", "/login/ticket", f), http.StatusForbidden)
		if s.e.Core.Redeems != 0 {
			t.Error("a refused post spent the ticket")
		}
		csrf := b.CSRF("/")
		old := b.Cookies[m.SessionCookie]
		want(t, "logout", b.Do("POST", "/logout", url.Values{"csrf": {csrf}}), http.StatusSeeOther)
		if b.Cookies[m.SessionCookie] != "" {
			t.Error("logout kept the cookie")
		}
		b.Cookies[m.SessionCookie] = old
		if b.Alive() {
			t.Error("a logged out session id still works")
		}
	})

	t.Run("fixation", func(t *testing.T) {
		s := start(t, &m, false)
		tok := s.token("user:alice", m.User)
		b := s.browser()
		b.Cookies[m.SessionCookie] = "planted-session-id"
		b.MustLogin(tok)
		first := b.Cookies[m.SessionCookie]
		if first == "planted-session-id" || len(first) < 40 {
			t.Fatalf("session id %q", first)
		}
		b.MustLogin(tok)
		if b.Cookies[m.SessionCookie] == first {
			t.Fatal("a second login kept the id")
		}
		stale := s.browser()
		stale.Cookies[m.SessionCookie] = first
		if stale.Alive() {
			t.Error("the replaced session still works")
		}
		second := b.Cookies[m.SessionCookie]
		if w := s.land(b, s.ticket("user:alice", m.User)); w.Code != http.StatusSeeOther {
			t.Fatalf("ticket: %d %.300s", w.Code, w.Body)
		}
		stale.Cookies[m.SessionCookie] = second
		if b.Cookies[m.SessionCookie] == second || stale.Alive() {
			t.Error("a ticket login kept the old session")
		}
	})

	t.Run("another person's session is not replaced silently", func(t *testing.T) {
		s := start(t, &m, false)
		b, _ := s.signedIn(m.User)
		old := b.Cookies[m.SessionCookie]
		page := s.interstitial(b, s.ticket("user:bob", m.User))
		f := formValues(s, page)
		asked := b.Do("POST", "/login/ticket", f)
		want(t, "without confirmation", asked, http.StatusConflict)
		if b.Cookies[m.SessionCookie] != old || s.e.Core.Redeems != 0 {
			t.Fatal("replaced without being asked")
		}
		f = formValues(s, asked)
		f.Set("replace", "user:bob")
		want(t, "confirmed", b.Do("POST", "/login/ticket", f), http.StatusSeeOther)
		if b.Cookies[m.SessionCookie] == old {
			t.Error("not replaced")
		}
	})

	t.Run("cross-site posts", func(t *testing.T) {
		s := start(t, &m, false)
		b, tok := s.signedIn(m.User)
		csrf := b.CSRF("/")
		lb := s.browser()
		login := url.Values{"token": {tok}, "csrf": {lb.CSRF("/login")}}
		tb := s.browser()
		ticket := formValues(s, s.interstitial(tb, s.ticket("user:alice", m.User)))
		for name, hdr := range crossSite {
			want(t, name+": logout", b.Do("POST", "/logout", url.Values{"csrf": {csrf}}, hdr...), http.StatusForbidden)
			want(t, name+": login", lb.Do("POST", "/login", login, hdr...), http.StatusForbidden)
			want(t, name+": ticket", tb.Do("POST", "/login/ticket", ticket, hdr...), http.StatusForbidden)
		}
		if !b.Alive() || s.e.Core.Redeems != 0 || lb.Cookies[m.SessionCookie] != "" {
			t.Fatal("a refused post had an effect")
		}
		for name, hdr := range map[string][]string{
			"own Origin":          {"Origin", "http://example.com"},
			"null from own page":  {"Origin", "null"},
			"no headers (client)": {"Sec-Fetch-Site", ""},
		} {
			nb := s.browser()
			want(t, name, nb.Login(tok, hdr...), http.StatusSeeOther)
		}
	})

	t.Run("cookie flags", func(t *testing.T) {
		for _, secure := range []bool{true, false} {
			s := start(t, &m, secure)
			tok := s.token("user:alice", m.User)
			b := s.browser()
			b.Secure = secure
			check := func(w *httptest.ResponseRecorder, name string, maxAge int) {
				t.Helper()
				for _, c := range w.Result().Cookies() {
					if c.Name != b.Name(name) || c.Value == "" {
						continue
					}
					if !c.HttpOnly || c.Secure != secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" || c.MaxAge != maxAge || !c.Expires.IsZero() {
						t.Errorf("secure=%v cookie %s", secure, c.String())
					}
					return
				}
				t.Errorf("secure=%v: no cookie %s in %v", secure, b.Name(name), w.Header()["Set-Cookie"])
			}
			page := b.Get("/login")
			check(page, m.CSRFCookie, m.LoginCSRFMaxAge)
			check(b.Do("POST", "/login", url.Values{"token": {tok}, "csrf": {csrfRe.FindStringSubmatch(page.Body.String())[1]}}), m.SessionCookie, 0)
			if b.Cookies[b.Name(m.CSRFCookie)] != "" {
				t.Error("the sign-in cookie outlived the sign-in")
			}
			tb := s.browser()
			tb.Secure = secure
			p := s.interstitial(tb, s.ticket("user:alice", m.User))
			check(p, m.CSRFCookie, m.LoginCSRFMaxAge)
			check(tb.Do("POST", "/login/ticket", formValues(s, p)), m.SessionCookie, 0)
		}
	})

	t.Run("security headers", func(t *testing.T) {
		s := start(t, &m, false)
		b, _ := s.signedIn(m.User)
		for name, w := range map[string]*httptest.ResponseRecorder{
			"home": b.Get("/"), "sign-in": s.browser().Get("/login"), "not found": b.Get("/no-such-page"),
			"refused": b.Do("POST", "/logout", url.Values{}), "cross-site": b.Do("POST", "/logout", url.Values{}, "Sec-Fetch-Site", "cross-site"),
		} {
			for h, v := range map[string]string{
				"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY",
				"Cross-Origin-Resource-Policy": "same-origin", "Permissions-Policy": "camera=(), microphone=(), geolocation=()",
				"Content-Security-Policy": "default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
			} {
				if got := w.Header().Get(h); got != v {
					t.Errorf("%s: %s = %q, want %q", name, h, got, v)
				}
			}
		}
	})

	t.Run("idle and absolute expiry", func(t *testing.T) {
		for _, kind := range []string{"token", "ticket"} {
			s := start(t, &m, false)
			sign := func() *Browser {
				if kind == "ticket" {
					// the ticket session's own deadline is kept out of the way
					b := s.browser()
					far := s.ticket("user:alice", m.User, func(g *fakecore.Grant) { g.SessionExpires = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) })
					want(t, "ticket", s.land(b, far), http.StatusSeeOther)
					return b
				}
				b, _ := s.signedIn(m.User)
				return b
			}
			b := sign()
			s.e.Advance(m.Idle - second)
			if !b.Alive() {
				t.Fatalf("%s: ended before the idle timeout", kind)
			}
			s.e.Advance(m.Idle + second)
			if b.Alive() {
				t.Errorf("%s: an idle session survived", kind)
			}
			b = sign()
			var age time.Duration
			for step := m.Idle - second; age+step < m.Absolute; age += step {
				s.e.Advance(step)
				if !b.Alive() {
					t.Fatalf("%s: ended after %s of use", kind, age+step)
				}
			}
			s.e.Advance(m.Absolute - age + second)
			if b.Alive() {
				t.Errorf("%s: outlived the absolute timeout", kind)
			}
		}
	})

	t.Run("ticket session ends at can-core's deadline", func(t *testing.T) {
		s := start(t, &m, false)
		s.e.Advance(0)
		b := s.browser()
		first := s.ticket("user:alice", m.User)
		page := s.interstitial(b, first)
		want(t, "ticket", b.Do("POST", "/login/ticket", formValues(s, page)), http.StatusSeeOther)
		// fakecore's default session_expires_at is 12 hours ahead
		var age time.Duration
		for step := min(m.Idle-second, time.Hour); age+step < min(12*time.Hour, m.Absolute); age += step {
			s.e.Advance(step)
			if !b.Alive() {
				t.Fatalf("ended after %s", age+step)
			}
		}
		s.e.Advance(min(12*time.Hour, m.Absolute) - age + second)
		if b.Alive() {
			t.Error("outlived its deadline")
		}
	})

	// revoked is the common shape of the re-check tests: the session outlives the revocation by at
	// most the interval (plus, for a token, the introspection cache).
	revoked := func(t *testing.T, s *suite, b *Browser, revoke func(), interval, cache time.Duration) {
		t.Helper()
		if !b.Alive() {
			t.Fatal("not signed in")
		}
		revoke()
		// still cached or not yet due: an interval of 0 only waits for the cache
		if early := max(interval, cache) - second; early > 0 {
			s.e.Advance(early)
			if !b.Alive() {
				t.Errorf("checked again before %s", early+second)
			}
		}
		s.e.Advance(2 * second)
		if b.Alive() {
			t.Errorf("a revoked credential still works after %s", max(interval, cache)+second)
		}
	}

	t.Run("revocation ends a token session", func(t *testing.T) {
		for name, c := range map[string]struct {
			perms    []string
			interval time.Duration
		}{"ordinary": {m.User, m.Recheck}, "privileged": {m.Privileged, m.PrivilegedRecheck}} {
			t.Run(name, func(t *testing.T) {
				s := start(t, &m, false)
				b, tok := s.signedIn(c.perms)
				// from here the answer cached at sign-in is no older than the session's last check
				revoked(t, s, b, func() { s.e.Core.Revoke(tok) }, c.interval, introspectTTL)
			})
		}
	})

	t.Run("revocation ends a ticket session", func(t *testing.T) {
		for name, c := range map[string]struct {
			perms    []string
			interval time.Duration
		}{"ordinary": {m.User, m.SSORecheck}, "privileged": {m.Privileged, m.SSOPrivilegedRecheck}} {
			t.Run(name, func(t *testing.T) {
				s := start(t, &m, false)
				b := s.mustLand(c.perms)
				revoked(t, s, b, func() { s.e.Core.EndSessions("user:alice") }, c.interval, 0)
			})
		}
		t.Run("can-core down fails closed", func(t *testing.T) {
			s := start(t, &m, false)
			b := s.mustLand(m.User)
			s.e.Core.SetDown(true)
			s.e.Advance(m.SSORecheck + second)
			if b.Alive() {
				t.Error("survived without an answer from can-core")
			}
		})
	})

	t.Run("body limits", func(t *testing.T) {
		s := start(t, &m, false)
		b, tok := s.signedIn(m.User)
		csrf := b.CSRF("/")
		pad := func(n int) string { return strings.Repeat("x", n) }
		w := b.Do("POST", "/logout", url.Values{"csrf": {csrf}, "pad": {pad(m.FormLimit)}})
		if w.Code != http.StatusForbidden && w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("oversized form: %d", w.Code)
		}
		if !b.Alive() {
			t.Fatal("an oversized form was acted on")
		}
		// public forms: bytes and fields
		for name, extra := range map[string]url.Values{"bytes": {"pad": {pad(4 << 10)}}, "fields": manyFields(20)} {
			lb := s.browser()
			f := url.Values{"token": {tok}, "csrf": {lb.CSRF("/login")}}
			for k, v := range extra {
				f[k] = v
			}
			if w := lb.Do("POST", "/login", f); w.Code < 400 || w.Code >= 500 || lb.Cookies[m.SessionCookie] != "" {
				t.Errorf("sign-in, too many %s: %d", name, w.Code)
			}
			tb := s.browser()
			tf := formValues(s, s.interstitial(tb, s.ticket("user:alice", m.User)))
			for k, v := range extra {
				tf[k] = v
			}
			if w := tb.Do("POST", "/login/ticket", tf); w.Code < 400 || w.Code >= 500 || tb.Cookies[m.SessionCookie] != "" {
				t.Errorf("ticket, too many %s: %d", name, w.Code)
			}
		}
		if s.e.Core.Redeems != 0 {
			t.Error("an oversized post reached can-core")
		}
		want(t, "a form within the limit", b.Do("POST", "/logout", url.Values{"csrf": {csrf}, "pad": {pad(m.FormLimit / 2)}}), http.StatusSeeOther)
	})

	t.Run("login throttle", func(t *testing.T) {
		s := start(t, &m, false)
		good := s.token("user:alice", m.User)
		bad := "canpat_unknown-0123456789-abcdefgh"
		b := s.browser()
		for i := 1; i <= m.FirstLimited+2; i++ {
			code := http.StatusUnauthorized
			if i >= m.FirstLimited {
				code = http.StatusTooManyRequests
			}
			w := b.Login(bad)
			if w.Code != code {
				t.Fatalf("failure %d: %d, want %d", i, w.Code, code)
			}
			if code == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
				t.Error("no Retry-After")
			}
		}
		// the address is over its limit: valid credentials still work, and refused tickets are limited too
		s.browser().MustLogin(good)
		if w := s.land(s.browser(), s.ticket("user:alice", m.User)); w.Code != http.StatusSeeOther {
			t.Errorf("a valid ticket was throttled: %d", w.Code)
		}
		used := s.ticket("user:alice", m.User)
		tb := s.browser()
		f := formValues(s, s.interstitial(tb, used))
		want(t, "ticket", tb.Do("POST", "/login/ticket", f), http.StatusSeeOther)
		rb := s.browser()
		c := rb.CSRF("/login")
		f.Set("csrf", c)
		want(t, "replayed ticket from a limited address", rb.Do("POST", "/login/ticket", f), http.StatusTooManyRequests)
	})

	if m.StepUp == nil {
		return
	}

	t.Run("step-up with a revoked token", func(t *testing.T) {
		s := start(t, &m, false)
		b, tok := s.signedIn(m.Privileged)
		path, form := m.StepUp(t, s.e, b)
		form.Set("csrf", b.CSRF("/"))
		form.Set("token", "canpat_not-this-session-0123456789")
		if w := b.Do("POST", path, form); w.Code < 400 {
			t.Fatalf("accepted another token: %d", w.Code)
		}
		s.e.Core.Revoke(tok) // the cached answer still says the token is good
		if !b.Alive() {
			t.Fatal("an ordinary page should still use the cached answer")
		}
		form.Set("token", tok)
		if w := b.Do("POST", path, form); w.Code < 400 {
			t.Fatalf("a revoked token confirmed an irreversible action: %d", w.Code)
		}
		fresh := s.token("user:alice", m.Privileged)
		b.MustLogin(fresh)
		form.Set("csrf", b.CSRF("/"))
		form.Set("token", fresh)
		want(t, "with a valid token", b.Do("POST", path, form), http.StatusOK)
	})

	t.Run("step-up of a ticket session", func(t *testing.T) {
		for name, tc := range map[string]struct {
			grant func(*fakecore.Grant)
			after func(*suite)
			ok    bool
		}{
			"fresh sign-in": {ok: true},
			"revoked":       {after: func(s *suite) { s.e.Core.EndSessions("user:alice") }},
			"demoted":       {after: func(s *suite) { s.e.Core.SetSessionPermissions("user:alice", m.User) }},
			"stale sign-in": {after: func(s *suite) { s.e.Advance(5*time.Minute + second) }},
			"unknown time":  {grant: func(g *fakecore.Grant) { g.NoAuthTime = true }},
		} {
			t.Run(name, func(t *testing.T) {
				s := start(t, &m, false)
				var gs []func(*fakecore.Grant)
				if tc.grant != nil {
					gs = append(gs, tc.grant)
				}
				b := s.browser()
				want(t, "ticket", s.land(b, s.ticket("user:alice", m.Privileged, gs...)), http.StatusSeeOther)
				path, form := m.StepUp(t, s.e, b)
				form.Set("csrf", b.CSRF("/"))
				if tc.after != nil {
					tc.after(s)
				}
				w := b.Do("POST", path, form)
				if tc.ok != (w.Code == http.StatusOK) || (!tc.ok && w.Code < 300) {
					t.Errorf("status %d: %.300s", w.Code, w.Body)
				}
			})
		}
	})
}

func manyFields(n int) url.Values {
	v := url.Values{}
	for i := range n {
		v.Set("f"+string(rune('a'+i)), "1")
	}
	return v
}
