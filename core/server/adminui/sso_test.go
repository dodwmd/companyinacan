package adminui

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/sso"
	"github.com/dodwmd/companyinacan/core/server/sso/ssotest"
)

type ssoHarness struct {
	*harness
	idp *ssotest.IdP
	cfg config.SSO
	svc *sso.Service
}

func newSSOHarness(t *testing.T, mut func(*config.SSO)) *ssoHarness {
	t.Helper()
	h := newHarness(t)
	idp := ssotest.New(t, "/realms/acme")
	clock := func() time.Time { return *h.now }
	idp.Now = clock
	s := &ssoHarness{harness: h, idp: idp}
	s.cfg = config.SSO{
		Enabled: true, Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecretRef: "acme-sso", RedirectBaseURL: "http://admin.test", AllowInsecureDev: true,
		GroupRoles: map[string][]string{"ops": {"operator"}}, RPLogout: true,
	}
	if mut != nil {
		mut(&s.cfg)
	}
	rb := rbac.New(h.db, h.audit, rbac.Options{Now: clock})
	s.svc = &sso.Service{DB: h.db, Auth: h.ui.Auth, RBAC: rb, Audit: h.audit, Now: clock, Secure: true, Secrets: sso.StaticSecrets{"acme-sso": idp.ClientSecret},
		Source: func(context.Context) (*config.SSO, string, error) { c := s.cfg; return &c, "acme.example", nil }}
	h.ui.RBAC, h.ui.SSO = rb, s.svc
	return s
}

var metaRe = regexp.MustCompile(`<meta http-equiv="refresh" content="0;url=([^"]*)">`)

func (s *ssoHarness) begin(next string) (loc string, state *http.Cookie) {
	s.t.Helper()
	p := "/auth/oidc/login"
	if next != "" {
		p += "?next=" + url.QueryEscape(next)
	}
	r := s.do("GET", p, nil)
	if r.StatusCode != http.StatusSeeOther {
		s.t.Fatalf("login: %d %s", r.StatusCode, r.body)
	}
	c := cookieNamed(r.Response, sso.StateCookieName)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/auth/oidc" {
		s.t.Fatalf("state cookie %+v", c)
	}
	return r.Header.Get("Location"), &http.Cookie{Name: c.Name, Value: c.Value}
}

func (s *ssoHarness) callback(cb string, cookies ...*http.Cookie) resp {
	u, _ := url.Parse(cb)
	return s.do("GET", "/auth/oidc/callback?"+u.RawQuery, nil, cookies...)
}

func (s *ssoHarness) signIn(u ssotest.User, next string, extra ...*http.Cookie) (resp, session) {
	s.t.Helper()
	loc, st := s.begin(next)
	r := s.callback(s.idp.Approve(loc, u), append([]*http.Cookie{st}, extra...)...)
	sc := cookieNamed(r.Response, "can_session")
	if sc == nil || sc.MaxAge < 0 {
		return r, session{}
	}
	ses := session{cookie: &http.Cookie{Name: sc.Name, Value: sc.Value}}
	return r, ses
}

func opUser() ssotest.User {
	return ssotest.User{Sub: "u-1", Email: "olive@acme.example", EmailVerified: true, Name: "Olive", Groups: []string{"ops"}}
}

func TestLoginPageOffersSSOOnlyWhenConfigured(t *testing.T) {
	h := newHarness(t)
	if strings.Contains(h.do("GET", "/login", nil).body, "/auth/oidc/login") {
		t.Fatal("no SSO configured: no button")
	}
	s := newSSOHarness(t, nil)
	body := s.do("GET", "/login", nil).body
	if !strings.Contains(body, `href="/auth/oidc/login"`) || !strings.Contains(body, `name="password"`) {
		t.Fatalf("button and the break-glass form expected:\n%s", body)
	}
	s.cfg.Enabled = false
	s.svc.Reload()
	if strings.Contains(s.do("GET", "/login", nil).body, "/auth/oidc/login") {
		t.Fatal("disabled: no button")
	}
	if r := s.do("GET", "/auth/oidc/login", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled login route: %d", r.StatusCode)
	}
	if r := s.do("GET", "/auth/oidc/callback?code=x&state=y", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled callback route: %d", r.StatusCode)
	}
}

func TestSSOFlowEndToEnd(t *testing.T) {
	s := newSSOHarness(t, nil)
	r, ses := s.signIn(opUser(), "/modules")
	if r.StatusCode != http.StatusOK || ses.cookie == nil {
		t.Fatalf("callback: %d %s", r.StatusCode, r.body)
	}
	// The session cookie is the same hardened one the password login sets.
	sc := cookieNamed(r.Response, "can_session")
	if !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie %+v", sc)
	}
	// The transaction cookie is cleared; the page moves on by itself (not by redirect) to next.
	if c := cookieNamed(r.Response, sso.StateCookieName); c == nil || c.MaxAge >= 0 {
		t.Fatalf("state cookie not cleared: %+v", c)
	}
	if m := metaRe.FindStringSubmatch(r.body); m == nil || m[1] != "/modules" {
		t.Fatalf("interstitial: %v\n%s", m, r.body)
	}
	if r.Header.Get("Location") != "" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers %v", r.Header)
	}
	if got := s.do("GET", "/modules", nil, ses.cookie); got.StatusCode != http.StatusOK {
		t.Fatalf("operator may view modules: %d", got.StatusCode)
	}
	// An operator may not manage access, and so may not open the SSO admin page.
	if got := s.do("GET", "/settings/sso", nil, ses.cookie); got.StatusCode != http.StatusForbidden {
		t.Fatalf("settings/sso for an operator: %d", got.StatusCode)
	}
	// An SSO user has no password to change.
	st := s.do("GET", "/settings", nil, ses.cookie)
	if st.StatusCode != 200 || strings.Contains(st.body, `name="current"`) || !strings.Contains(st.body, "single sign-on") {
		t.Fatalf("settings page for an SSO user:\n%s", st.body)
	}
	csrf := csrfOf(t, st.body)
	pw := s.do("POST", "/settings/password", url.Values{"csrf": {csrf}, "current": {"x"}, "otp": {"000000"}, "new": {"a long enough new password"}, "confirm": {"a long enough new password"}}, ses.cookie)
	if pw.StatusCode == http.StatusSeeOther {
		t.Fatal("an SSO user cannot set a password")
	}
}

func TestOpenRedirectViaNextIsClosed(t *testing.T) {
	for _, next := range []string{"//evil.example/x", "https://evil.example", "/\\evil.example", "javascript:alert(1)", "/auth/oidc/login", "evil.example"} {
		s := newSSOHarness(t, nil)
		r, ses := s.signIn(opUser(), next)
		if ses.cookie == nil {
			t.Fatalf("%q: no session: %d %s", next, r.StatusCode, r.body)
		}
		if m := metaRe.FindStringSubmatch(r.body); m == nil || m[1] != "/" {
			t.Errorf("next %q led to %v", next, m)
		}
	}
}

func TestCallbackRefusals(t *testing.T) {
	s := newSSOHarness(t, nil)
	loc, st := s.begin("")
	cb := s.idp.Approve(loc, opUser())
	// state mismatch
	u, _ := url.Parse(cb)
	q := u.Query()
	q.Set("state", "forged")
	r := s.do("GET", "/auth/oidc/callback?"+q.Encode(), nil, st)
	if r.StatusCode != http.StatusForbidden || cookieNamed(r.Response, "can_session") != nil || !strings.Contains(r.body, "Single sign-on was refused") {
		t.Fatalf("state mismatch: %d %s", r.StatusCode, r.body)
	}
	// no cookie at all
	r = s.callback(cb)
	if r.StatusCode != http.StatusForbidden || cookieNamed(r.Response, "can_session") != nil {
		t.Fatalf("no cookie: %d", r.StatusCode)
	}
	// a refusal never says why
	for _, leak := range []string{"state", "nonce", "audience", "issuer", "token"} {
		if strings.Contains(strings.ToLower(strings.SplitN(r.body, "<main", 2)[1]), leak) {
			t.Errorf("the refusal page mentions %q", leak)
		}
	}
	// the provider says no
	loc2, st2 := s.begin("")
	cu, _ := url.Parse(loc2)
	r = s.do("GET", "/auth/oidc/callback?error=access_denied&error_description=%3Cscript%3E&state="+cu.Query().Get("state"), nil, st2)
	if r.StatusCode != http.StatusForbidden || strings.Contains(r.body, "<script>") {
		t.Fatalf("idp error: %d", r.StatusCode)
	}
}

func TestSessionFixationAtTheEdge(t *testing.T) {
	s := newSSOHarness(t, nil)
	_, first := s.signIn(opUser(), "")
	r, second := s.signIn(opUser(), "", first.cookie)
	if second.cookie == nil || second.cookie.Value == first.cookie.Value {
		t.Fatalf("a new token is required: %d", r.StatusCode)
	}
	if got := s.do("GET", "/modules", nil, first.cookie); got.StatusCode != http.StatusSeeOther {
		t.Fatalf("the old session must be dead: %d", got.StatusCode)
	}
}

func TestLogoutNeedsCSRFAndCanEndTheProviderSession(t *testing.T) {
	s := newSSOHarness(t, nil)
	_, ses := s.signIn(opUser(), "")
	csrf := csrfOf(t, s.do("GET", "/settings", nil, ses.cookie).body)
	// CSRF: a cross-site or token-less POST does nothing.
	if r := s.do("POST", "/logout", url.Values{}, ses.cookie); r.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without csrf: %d", r.StatusCode)
	}
	r := s.do("POST", "/logout", url.Values{"csrf": {csrf}}, ses.cookie)
	// The POST must not redirect to the provider (form-action 'self'); a page does.
	if r.StatusCode != http.StatusOK || r.Header.Get("Location") != "" {
		t.Fatalf("logout: %d %v", r.StatusCode, r.Header)
	}
	m := metaRe.FindStringSubmatch(r.body)
	if m == nil || !strings.HasPrefix(strings.ReplaceAll(m[1], "&amp;", "&"), s.idp.Issuer+"/logout?client_id=can-admin") {
		t.Fatalf("expected the provider's end-session URL: %v", m)
	}
	if c := cookieNamed(r.Response, "can_session"); c == nil || c.MaxAge >= 0 {
		t.Fatalf("session cookie not cleared: %+v", c)
	}
	if got := s.do("GET", "/modules", nil, ses.cookie); got.StatusCode != http.StatusSeeOther {
		t.Fatal("session must be gone")
	}
	// Without rp_logout the logout is the ordinary redirect.
	s.cfg.RPLogout = false
	s.svc.Reload()
	_, ses2 := s.signIn(opUser(), "")
	csrf2 := csrfOf(t, s.do("GET", "/settings", nil, ses2.cookie).body)
	if r := s.do("POST", "/logout", url.Values{"csrf": {csrf2}}, ses2.cookie); r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/login") {
		t.Fatalf("%d %v", r.StatusCode, r.Header)
	}
}

func TestSSOAdminPageAndDiscoveryTestAreOwnerToolsWithCSRF(t *testing.T) {
	s := newSSOHarness(t, nil)
	_, ses := s.signIn(opUser(), "")
	_ = ses
	root := s.login() // the local break-glass owner
	page := s.do("GET", "/settings/sso", nil, root.with()...)
	if page.StatusCode != 200 || !strings.Contains(page.body, "Configured and enabled") || !strings.Contains(page.body, "acme-sso") ||
		!strings.Contains(page.body, "http://admin.test/auth/oidc/callback") || strings.Contains(page.body, s.idp.ClientSecret) {
		t.Fatalf("status page:\n%s", page.body)
	}
	if !strings.Contains(page.body, "olive@acme.example") {
		t.Fatal("the known identity is listed")
	}
	// CSRF and same-site are enforced on the test button.
	if r := s.do("POST", "/settings/sso/test", url.Values{}, root.with()...); r.StatusCode != http.StatusForbidden {
		t.Fatalf("no csrf: %d", r.StatusCode)
	}
	r := s.do("POST", "/settings/sso/test", url.Values{"csrf": {root.csrf}}, root.with()...)
	if r.StatusCode != 200 || !strings.Contains(r.body, "discovery and signing keys fetched") {
		t.Fatalf("test: %d %s", r.StatusCode, r.body)
	}
	s.idp.DiscIssuer = "https://elsewhere.example"
	r = s.do("POST", "/settings/sso/test", url.Values{"csrf": {root.csrf}}, root.with()...)
	if !strings.Contains(r.body, "does not match") {
		t.Fatalf("a mismatching issuer must show as a failure:\n%s", r.body)
	}
}

func TestLinkThroughTheUI(t *testing.T) {
	s := newSSOHarness(t, nil)
	_, ses := s.signIn(opUser(), "")
	root := s.login()
	ids, _ := s.svc.Identities(context.Background())
	if len(ids) != 1 {
		t.Fatal(ids)
	}
	path := "/settings/sso/identities/" + strconv.FormatInt(ids[0].ID, 10)
	// The operator (not an owner, no rbac.manage) cannot.
	csrf := csrfOf(t, s.do("GET", "/settings", nil, ses.cookie).body)
	if r := s.do("POST", path+"/link", url.Values{"csrf": {csrf}, "username": {"root"}}, ses.cookie); r.StatusCode != http.StatusForbidden {
		t.Fatalf("operator link: %d", r.StatusCode)
	}
	// An unknown user is a conflict, not a crash.
	if r := s.do("POST", path+"/link", url.Values{"csrf": {root.csrf}, "username": {"ghost"}}, root.with()...); r.StatusCode != http.StatusConflict {
		t.Fatalf("ghost: %d", r.StatusCode)
	}
	r := s.do("POST", path+"/link", url.Values{"csrf": {root.csrf}, "username": {"root"}}, root.with()...)
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("link: %d %s", r.StatusCode, r.body)
	}
	if got := s.do("GET", "/modules", nil, ses.cookie); got.StatusCode != http.StatusSeeOther {
		t.Fatal("the provisioned user's session ends when the identity is linked")
	}
	// Signing in through SSO now lands on the local owner account.
	_, ses2 := s.signIn(opUser(), "")
	if got := s.do("GET", "/settings/sso", nil, ses2.cookie); got.StatusCode != 200 {
		t.Fatalf("linked to root: %d", got.StatusCode)
	}
	r = s.do("POST", path+"/unlink", url.Values{"csrf": {root.csrf}}, root.with()...)
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("unlink: %d %s", r.StatusCode, r.body)
	}
	var n int
	_ = s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM admin_users WHERE username='root'`).Scan(&n)
	if n != 1 {
		t.Fatal("unlinking must not delete the local account")
	}
}

func TestSSOPagesAreUnavailableWithoutService(t *testing.T) {
	h := newHarness(t)
	root := h.login()
	if r := h.do("GET", "/settings/sso", nil, root.with()...); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("%d", r.StatusCode)
	}
}
