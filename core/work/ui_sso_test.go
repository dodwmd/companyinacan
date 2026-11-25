package work

import (
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

// Single sign-on from the admin UI (specs/036-module-ui-sso): the person arrives with a one-time
// launch ticket and gets a session of this module's own.

type ssoEnv struct {
	*harness
	core *fakecore.Core
}

func newSSOEnv(t *testing.T, mutate func(*Config)) *ssoEnv {
	t.Helper()
	core := fakecore.New(t, "work")
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "work.db")
	if mutate != nil {
		mutate(cfg)
	}
	clk := &clock{t: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}
	app, err := NewApp(cfg, Secrets{AdminToken: bootstrap}, Options{Now: clk.Now, Logger: slog.New(slog.DiscardHandler), Can: core.Client(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	h := &harness{t: t, app: app, clk: clk, h: app.Handler()}
	h.mustDo(201, "POST", "/v1/projects", bootstrap, map[string]any{"key": "ACME", "name": "Acme"})
	return &ssoEnv{h, core}
}

func (e *ssoEnv) ticket(perms ...string) string {
	if len(perms) == 0 {
		perms = []string{PermWrite}
	}
	return e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Name: "Alice", Permissions: perms})
}

var hiddenTicketRe = regexp.MustCompile(`name="ticket" value="([^"]+)"`)

// hold gives the browser the nonce cookie can-core set in it at launch.
func (e *ssoEnv) hold(b *uiBrowser, ticket string) {
	if n := e.core.Binding(ticket); n != "" {
		b.cookies["can_launch_work"] = &http.Cookie{Name: "can_launch_work", Value: n}
	}
}

// land is the launching browser: it holds the nonce, opens the link and posts the form.
func (e *ssoEnv) land(b *uiBrowser, ticket string) uiResp {
	e.hold(b, ticket)
	e.t.Helper()
	page := b.do("GET", "/ui/login/ticket?t="+ticket, nil)
	if page.Code != 200 { // refused or unavailable before any form was offered
		return page
	}
	return b.do("POST", "/ui/login/ticket", url.Values{"csrf": {uiCSRFRe.FindStringSubmatch(page.Body)[1]}, "ticket": {hiddenTicketRe.FindStringSubmatch(page.Body)[1]}})
}

func TestTicketLinkOnlyShowsAFormAndDoesNotConsumeTheTicket(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := newUIBrowser(t, e.harness)
	tk := e.ticket()
	e.hold(b, tk)
	for i := 0; i < 3; i++ { // a link scanner, a prefetch, the person: GET never consumes
		r := b.do("GET", "/ui/login/ticket?t="+tk, nil)
		if r.Code != 200 || !strings.Contains(r.Body, `method="post" action="/ui/login/ticket"`) {
			t.Fatalf("%d %.300s", r.Code, r.Body)
		}
		if strings.Contains(r.Body, "http-equiv") || strings.Contains(r.Body, "<script") {
			t.Fatal("the page moves on by itself: a scanner would consume the ticket")
		}
		for h, want := range map[string]string{"Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
			if r.Header.Get(h) != want {
				t.Fatalf("%s = %q", h, r.Header.Get(h))
			}
		}
		if strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src") && !strings.Contains(r.Header.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatalf("csp %q", r.Header.Get("Content-Security-Policy"))
		}
	}
	if e.core.Redeems != 0 {
		t.Fatalf("GET reached can-core %d times", e.core.Redeems)
	}
	if r := e.land(b, tk); r.Code != 303 {
		t.Fatalf("the ticket was spent by a GET: %d %.300s", r.Code, r.Body)
	}
}

func TestTicketLoginMakesASessionOfTheModuleAndLandsOnACleanURL(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := newUIBrowser(t, e.harness)
	r := e.land(b, e.ticket())
	if r.Code != 303 || r.Header.Get("Location") != "/ui/" {
		t.Fatalf("%d %q", r.Code, r.Header.Get("Location"))
	}
	if strings.Contains(r.Header.Get("Location"), "t=") || strings.Contains(r.Header.Get("Location"), "cant_") {
		t.Fatal("the ticket stays in the address bar")
	}
	if p := b.do("GET", "/ui/", nil); p.Code != 200 || !strings.Contains(p.Body, "Alice") || strings.Contains(p.Body, "user:alice") {
		t.Fatalf("signed-in page: %d %.300s", p.Code, p.Body)
	}
	// The module's own cookie is all the browser holds: no ticket, no handle, no token.
	for _, c := range b.cookies {
		if strings.Contains(c.Value, "cant_") || strings.Contains(c.Value, "cans_") || strings.Contains(c.Value, "canpat_") {
			t.Fatalf("cookie %s carries a credential", c.Name)
		}
	}
	// Logging out ends the module session only.
	if r := b.post("/ui/", "/ui/logout", nil); r.Code != 303 {
		t.Fatalf("logout %d", r.Code)
	}
	if p := b.do("GET", "/ui/", nil); p.Code != 303 {
		t.Fatalf("after logout %d", p.Code)
	}
}

func TestTicketReplayAndForeignTicketsAreRefusedWithoutASession(t *testing.T) {
	e := newSSOEnv(t, nil)
	tk := e.ticket()
	e.land(newUIBrowser(t, e.harness), tk)
	b := newUIBrowser(t, e.harness)
	r := e.land(b, tk)
	if r.Code != 401 || !strings.Contains(r.Body, `role="alert"`) {
		t.Fatalf("replay: %d %.300s", r.Code, r.Body)
	}
	foreign := e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{PermWrite}, Module: "people"})
	b = newUIBrowser(t, e.harness)
	if r := e.land(b, foreign); r.Code != 401 {
		t.Fatalf("a ticket for another module: %d", r.Code)
	}
	for _, c := range b.cookies {
		if strings.Contains(c.Name, "session") {
			t.Fatalf("a refused login set %s", c.Name)
		}
	}
	// Every refusal reads the same.
	r1 := e.land(newUIBrowser(t, e.harness), "cant_"+strings.Repeat("Q", 43))
	r2 := e.land(newUIBrowser(t, e.harness), tk)
	if r1.Code != r2.Code || len(r1.Body) != len(r2.Body) {
		t.Fatal("refusals differ")
	}
}

// Login CSRF with two browsers: the attacker launches their own account and sends the link to the
// victim. The victim's browser lacks the launch nonce, so the link does nothing there.
func TestALinkOpenedInAnotherBrowserSignsNobodyIn(t *testing.T) {
	e := newSSOEnv(t, nil)
	attacker, victim := newUIBrowser(t, e.harness), newUIBrowser(t, e.harness)
	tk := e.core.NewTicket(fakecore.Grant{Subject: "user:mallory", Name: "Mallory", Permissions: []string{PermWrite}})
	e.hold(attacker, tk)
	r := victim.do("GET", "/ui/login/ticket?t="+tk, nil)
	if r.Code != 401 || strings.Contains(r.Body, "Mallory") || hiddenTicketRe.MatchString(r.Body) {
		t.Fatalf("the victim was offered the attacker's account: %d %.300s", r.Code, r.Body)
	}
	// Even with a login token of their own (from the ordinary sign-in page) and, say, the nonce of
	// a launch of their own, the victim's post signs nobody in.
	csrf := victim.csrf("/ui/login")
	if r := victim.do("POST", "/ui/login/ticket", url.Values{"csrf": {csrf}, "ticket": {tk}}); r.Code != 401 {
		t.Fatalf("post without the nonce: %d", r.Code)
	}
	e.hold(victim, e.ticket())
	csrf = victim.csrf("/ui/login")
	if r := victim.do("POST", "/ui/login/ticket", url.Values{"csrf": {csrf}, "ticket": {tk}}); r.Code != 401 {
		t.Fatalf("post with another launch's nonce: %d", r.Code)
	}
	for _, c := range victim.cookies {
		if strings.Contains(c.Name, "session") {
			t.Fatalf("the victim got a session: %s", c.Name)
		}
	}
	// The ticket was not spent by any of that: it still works where it was launched.
	if r := e.land(attacker, tk); r.Code != 303 {
		t.Fatalf("the launching browser: %d", r.Code)
	}
}

func TestAnotherAccountsSessionIsNeverReplacedSilently(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := newUIBrowser(t, e.harness)
	e.land(b, e.core.NewTicket(fakecore.Grant{Subject: "user:bob", Name: "Bob", Permissions: []string{PermWrite}}))
	tk := e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Name: "Alice", Permissions: []string{PermWrite}})
	e.hold(b, tk)
	page := b.do("GET", "/ui/login/ticket?t="+tk, nil)
	if page.Code != 200 || !strings.Contains(page.Body, "Alice") || !strings.Contains(page.Body, "Bob") || !strings.Contains(page.Body, `name="replace" value="user:alice"`) {
		t.Fatalf("no warning naming both accounts: %d %.600s", page.Code, page.Body)
	}
	form := url.Values{"csrf": {uiCSRFRe.FindStringSubmatch(page.Body)[1]}, "ticket": {tk}}
	before := e.core.Redeems
	r := b.do("POST", "/ui/login/ticket", form)
	if r.Code != 409 || e.core.Redeems != before {
		t.Fatalf("replaced without confirmation: %d (redeems %d)", r.Code, e.core.Redeems-before)
	}
	if p := b.do("GET", "/ui/", nil); p.Code != 200 || !strings.Contains(p.Body, "Bob") {
		t.Fatalf("the existing session was dropped: %d", p.Code)
	}
	form.Set("csrf", uiCSRFRe.FindStringSubmatch(r.Body)[1])
	form.Set("replace", "user:alice")
	if r := b.do("POST", "/ui/login/ticket", form); r.Code != 303 {
		t.Fatalf("confirmed: %d %.300s", r.Code, r.Body)
	}
	if p := b.do("GET", "/ui/", nil); !strings.Contains(p.Body, "Alice") {
		t.Fatalf("%.300s", p.Body)
	}
}

func TestInterstitialNamesTheAccount(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := newUIBrowser(t, e.harness)
	tk := e.ticket()
	e.hold(b, tk)
	page := b.do("GET", "/ui/login/ticket?t="+tk, nil)
	if !strings.Contains(page.Body, "Alice") || !strings.Contains(page.Body, "<code>alice</code>") || strings.Contains(page.Body, `name="replace"`) {
		t.Fatalf("%.600s", page.Body)
	}
}

func TestTicketPostNeedsTheDoubleSubmitTokenAndSameSite(t *testing.T) {
	e := newSSOEnv(t, nil)
	tk := e.ticket()
	b := newUIBrowser(t, e.harness)
	e.hold(b, tk)
	page := b.do("GET", "/ui/login/ticket?t="+tk, nil)
	csrf := uiCSRFRe.FindStringSubmatch(page.Body)[1]
	if r := b.do("POST", "/ui/login/ticket", url.Values{"csrf": {"wrong"}, "ticket": {tk}}); r.Code != 403 {
		t.Fatalf("wrong csrf %d", r.Code)
	}
	if r := b.do("POST", "/ui/login/ticket", url.Values{"ticket": {tk}}); r.Code != 403 {
		t.Fatalf("missing csrf %d", r.Code)
	}
	if r := b.do("POST", "/ui/login/ticket", url.Values{"csrf": {csrf}, "ticket": {tk}}, "Sec-Fetch-Site", "cross-site"); r.Code != 403 {
		t.Fatalf("cross-site %d", r.Code)
	}
	if r := b.do("POST", "/ui/login/ticket", url.Values{"csrf": {csrf}, "ticket": {tk}}, "Origin", "https://evil.example"); r.Code != 403 {
		t.Fatalf("foreign origin %d", r.Code)
	}
	// A forged cross-site login (an attacker's ticket in the victim's browser) has no cookie.
	fresh := newUIBrowser(t, e.harness)
	if r := fresh.do("POST", "/ui/login/ticket", url.Values{"csrf": {csrf}, "ticket": {tk}}); r.Code != 403 {
		t.Fatalf("login CSRF %d", r.Code)
	}
	if e.core.Redeems != 0 {
		t.Fatalf("a refused post reached can-core (%d)", e.core.Redeems)
	}
	// A refused post renders a fresh sign-in page (and token); the genuine flow works from a new link.
	if r := e.land(newUIBrowser(t, e.harness), tk); r.Code != 303 {
		t.Fatalf("the ticket was spent by refused posts: %d", r.Code)
	}
}

// With Referrer-Policy: no-referrer (every page here sends it) a real browser posts a form with
// "Origin: null"; the browser-controlled Sec-Fetch-Site then says whether it is our own page.
func TestOriginNullIsAcceptedOnlyForASameOriginPost(t *testing.T) {
	e := newSSOEnv(t, nil)
	for name, c := range map[string]struct {
		hdr  []string
		want int
	}{
		"browser, same origin":    {[]string{"Origin", "null", "Sec-Fetch-Site", "same-origin"}, 303},
		"null alone":              {[]string{"Origin", "null"}, 403},
		"null, no site":           {[]string{"Origin", "null", "Sec-Fetch-Site", "none"}, 403},
		"null, cross-site":        {[]string{"Origin", "null", "Sec-Fetch-Site", "cross-site"}, 403},
		"null, same-site sibling": {[]string{"Origin", "null", "Sec-Fetch-Site", "same-site"}, 403},
	} {
		b := newUIBrowser(t, e.harness)
		tk := e.ticket()
		e.hold(b, tk)
		page := b.do("GET", "/ui/login/ticket?t="+tk, nil)
		r := b.do("POST", "/ui/login/ticket", url.Values{"csrf": {uiCSRFRe.FindStringSubmatch(page.Body)[1]}, "ticket": {tk}}, c.hdr...)
		if r.Code != c.want {
			t.Errorf("%s: %d, want %d", name, r.Code, c.want)
		}
	}
}

func TestTicketLoginIssuesANewSessionIdWhateverTheBrowserSent(t *testing.T) {
	e := newSSOEnv(t, nil)
	first := newUIBrowser(t, e.harness)
	e.land(first, e.ticket())
	var old string
	for _, c := range first.cookies {
		if strings.Contains(c.Name, "work_session") {
			old = c.Value
		}
	}
	if old == "" {
		t.Fatal("no session cookie")
	}
	// Same browser signs in again: the id changes, and the old one is dead.
	e.land(first, e.ticket())
	for _, c := range first.cookies {
		if strings.Contains(c.Name, "work_session") && c.Value == old {
			t.Fatal("the session id was reused")
		}
	}
	// An attacker-planted cookie value is never adopted.
	b := newUIBrowser(t, e.harness)
	b.cookies["work_session"] = &http.Cookie{Name: "work_session", Value: "planted"}
	e.land(b, e.ticket())
	for _, c := range b.cookies {
		if strings.Contains(c.Name, "work_session") && c.Value == "planted" {
			t.Fatal("a planted session id was adopted")
		}
	}
}

func TestReturnPathIsAnOpenRedirectProofLandingPlace(t *testing.T) {
	e := newSSOEnv(t, nil)
	for ret, want := range map[string]string{
		"/projects/ACME": "/ui/projects/ACME", "/": "/ui/", "": "/ui/",
		"//evil.example": "/ui/", "/../api": "/ui/", "https://evil.example": "/ui/", "/login": "/ui/", "/x?y=z": "/ui/", "\\evil": "/ui/",
	} {
		tk := e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{PermWrite}, ReturnPath: ret})
		r := e.land(newUIBrowser(t, e.harness), tk)
		if r.Code != 303 || r.Header.Get("Location") != want {
			t.Errorf("return %q -> %d %q, want %q", ret, r.Code, r.Header.Get("Location"), want)
		}
	}
}

func TestPermissionsOfATicketSessionAreExactlyTheModulePermissionsReturned(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := newUIBrowser(t, e.harness)
	e.land(b, e.ticket(PermRead))
	if p := b.do("GET", "/ui/projects/ACME", nil); p.Code != 200 {
		t.Fatalf("read: %d", p.Code)
	}
	r := b.post("/ui/projects/ACME", "/ui/projects/ACME/issues", url.Values{"title": {"x"}, "type": {"task"}})
	if r.Code != 403 {
		t.Fatalf("a read-only SSO session created an issue: %d", r.Code)
	}
	// A person with none of this module's permissions gets no session.
	none := e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{"people.read"}})
	nb := newUIBrowser(t, e.harness)
	if r := e.land(nb, none); r.Code != 401 {
		t.Fatalf("no work permission: %d", r.Code)
	}
}

func TestTicketSessionFollowsCanCoreAtTheRevalidationInterval(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := newUIBrowser(t, e.harness)
	e.land(b, e.ticket(PermRead, PermWrite))
	if r := b.post("/ui/projects/ACME", "/ui/projects/ACME/issues", url.Values{"title": {"x"}, "type": {"task"}}); r.Code != 303 {
		t.Fatalf("write: %d %.200s", r.Code, r.Body)
	}
	before := e.core.Revalidates
	b.do("GET", "/ui/", nil)
	if e.core.Revalidates != before {
		t.Fatal("revalidated on every request")
	}
	// The write permission is taken away: it applies at the next check, not before.
	e.core.SetSessionPermissions("user:alice", []string{PermRead})
	if r := b.post("/ui/projects/ACME", "/ui/projects/ACME/issues", url.Values{"title": {"y"}, "type": {"task"}}); r.Code != 303 {
		t.Fatalf("inside the interval the old permissions still apply: %d", r.Code)
	}
	e.clk.Advance(time.Minute + time.Second)
	if r := b.post("/ui/projects/ACME", "/ui/projects/ACME/issues", url.Values{"title": {"z"}, "type": {"task"}}); r.Code != 403 {
		t.Fatalf("after the interval write must be gone: %d", r.Code)
	}
	if p := b.do("GET", "/ui/", nil); p.Code != 200 {
		t.Fatalf("read remains: %d", p.Code)
	}
	// The account is disabled in can-core: the next check ends the module session.
	e.core.EndSessions("user:alice")
	e.clk.Advance(time.Minute + time.Second)
	if p := b.do("GET", "/ui/", nil); p.Code != 303 || !strings.HasPrefix(p.Header.Get("Location"), "/ui/login") {
		t.Fatalf("a disabled user kept the session: %d", p.Code)
	}
}

func TestCanCoreOutageEndsTicketSessionsAtTheNextCheckAndNeverActivates(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := newUIBrowser(t, e.harness)
	tk := e.ticket()
	e.core.SetDown(true)
	if r := e.land(b, tk); r.Code != 503 {
		t.Fatalf("can-core down at redeem: %d", r.Code)
	}
	e.core.SetDown(false)
	if r := e.land(b, tk); r.Code != 303 {
		t.Fatalf("the ticket must survive an outage: %d", r.Code)
	}
	e.core.SetDown(true)
	e.clk.Advance(time.Minute + time.Second)
	if p := b.do("GET", "/ui/", nil); p.Code != 303 {
		t.Fatalf("an unverifiable session stayed open: %d", p.Code)
	}
}

func TestPastedTokenLoginCanBeTurnedOff(t *testing.T) {
	off := false
	e := newSSOEnv(t, func(c *Config) { c.UI.TokenLogin = &off })
	b := newUIBrowser(t, e.harness)
	page := b.do("GET", "/ui/login", nil)
	if page.Code != 200 || strings.Contains(page.Body, `name="token"`) {
		t.Fatalf("the paste form is still offered: %d", page.Code)
	}
	tok := e.newToken("personal", "dev@example.com", []string{PermWrite})
	csrf := uiCSRFRe.FindStringSubmatch(page.Body)
	if csrf == nil {
		csrf = []string{"", "x"}
	}
	if r := b.do("POST", "/ui/login", url.Values{"token": {tok}, "csrf": {csrf[1]}}); r.Code == 303 {
		t.Fatal("a pasted token logged in although token_login is off")
	}
	// Ticket login still works.
	if r := e.land(b, e.ticket()); r.Code != 303 {
		t.Fatalf("%d", r.Code)
	}
}

func TestTicketNeverReachesLogsOrErrors(t *testing.T) {
	var logs strings.Builder
	core := fakecore.New(t, "work")
	cfg := DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "work.db")
	app, err := NewApp(cfg, Secrets{AdminToken: bootstrap}, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil)), Can: core.Client(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	h := &harness{t: t, app: app, clk: &clock{t: time.Now()}, h: app.Handler()}
	b := newUIBrowser(t, h)
	tk := core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{PermWrite}})
	(&ssoEnv{h, core}).land(b, tk)
	b.do("GET", "/ui/login/ticket?t=cant_"+strings.Repeat("Z", 43), nil)
	if strings.Contains(logs.String(), tk) || strings.Contains(logs.String(), "cant_") {
		t.Fatalf("a ticket reached the log:\n%s", logs.String())
	}
}
