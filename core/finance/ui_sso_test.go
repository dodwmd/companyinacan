package finance

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

// Single sign-on from the admin UI (specs/036-module-ui-sso): the person arrives with a one-time
// launch ticket and gets a session of this module's own.

type ssoEnv struct {
	*uiEnv
	core *fakecore.Core
}

func newSSOEnv(t *testing.T, mutate func(*Config)) *ssoEnv {
	t.Helper()
	core := fakecore.New(t, "finance")
	u := newUIEnv(t, func(c *Config, o *Options) {
		c.Webhook.URL = ""
		o.Can = core.Client(t)
		if mutate != nil {
			mutate(c)
		}
	})
	return &ssoEnv{u, core}
}

func (e *ssoEnv) ticket(perms ...string) string {
	if len(perms) == 0 {
		perms = []string{PermRead, PermWrite}
	}
	return e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Name: "Alice", Permissions: perms})
}

var hiddenTicketRe = regexp.MustCompile(`name="ticket" value="([^"]+)"`)

// hold gives the browser the nonce cookie can-core set in it at launch.
func (e *ssoEnv) hold(c *client, ticket string) {
	if n := e.core.Binding(ticket); n != "" {
		c.cookies["__Secure-can_launch_finance"] = n
	}
}

// land opens the link from the launcher and posts the interstitial's form, like a browser.
func (e *ssoEnv) land(c *client, ticket string) *httptest.ResponseRecorder {
	e.hold(c, ticket)
	e.t.Helper()
	page := c.do("GET", "/login/ticket?t="+ticket, nil)
	if page.Code != 200 { // refused or unavailable before any form was offered
		return page
	}
	return c.do("POST", "/login/ticket", url.Values{"csrf": {csrfRe.FindStringSubmatch(page.Body.String())[1]}, "ticket": {hiddenTicketRe.FindStringSubmatch(page.Body.String())[1]}}, "Sec-Fetch-Site", "same-origin")
}

func TestTicketLinkOnlyShowsAFormAndDoesNotConsumeTheTicket(t *testing.T) {
	e := newSSOEnv(t, nil)
	c := e.client(t)
	tk := e.ticket()
	e.hold(c, tk)
	for i := 0; i < 3; i++ { // a link scanner, a prefetch, the person: GET never consumes
		r := c.do("GET", "/login/ticket?t="+tk, nil)
		if r.Code != 200 || !strings.Contains(r.Body.String(), `method="post" action="/login/ticket"`) {
			t.Fatalf("%d %.300s", r.Code, r.Body)
		}
		if strings.Contains(r.Body.String(), "http-equiv") || strings.Contains(r.Body.String(), "<script") {
			t.Fatal("the page moves on by itself: a scanner would consume the ticket")
		}
		for h, want := range map[string]string{"Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
			if r.Header().Get(h) != want {
				t.Fatalf("%s = %q", h, r.Header().Get(h))
			}
		}
		if !strings.Contains(r.Header().Get("Content-Security-Policy"), "script-src 'none'") {
			t.Fatalf("csp %q", r.Header().Get("Content-Security-Policy"))
		}
	}
	if e.core.Redeems != 0 {
		t.Fatalf("GET reached can-core %d times", e.core.Redeems)
	}
	if r := e.land(c, tk); r.Code != 303 {
		t.Fatalf("the ticket was spent by a GET: %d %.300s", r.Code, r.Body)
	}
}

func TestTicketLoginMakesASessionOfTheModuleAndLandsOnACleanURL(t *testing.T) {
	e := newSSOEnv(t, nil)
	c := e.client(t)
	r := e.land(c, e.ticket())
	if r.Code != 303 || r.Header().Get("Location") != "/" {
		t.Fatalf("%d %q", r.Code, r.Header().Get("Location"))
	}
	if p := c.get("/"); p.Code != 200 || strings.Contains(p.Body.String(), "cans_") {
		t.Fatalf("signed-in page: %d %.300s", p.Code, p.Body)
	}
	for n, v := range c.cookies {
		if strings.Contains(v, "cant_") || strings.Contains(v, "cans_") || strings.Contains(v, "canpat_") {
			t.Fatalf("cookie %s carries a credential", n)
		}
	}
	if r := c.post("/logout", nil); r.Code != 303 {
		t.Fatalf("logout %d", r.Code)
	}
	if p := c.get("/"); p.Code != 303 {
		t.Fatalf("after logout %d", p.Code)
	}
}

func TestTicketReplayAndForeignTicketsAreRefusedWithoutASession(t *testing.T) {
	e := newSSOEnv(t, nil)
	tk := e.ticket()
	e.land(e.client(t), tk)
	c := e.client(t)
	r := e.land(c, tk)
	if r.Code != 401 || !strings.Contains(r.Body.String(), `role="alert"`) {
		t.Fatalf("replay: %d %.300s", r.Code, r.Body)
	}
	foreign := e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{PermWrite}, Module: "people"})
	c = e.client(t)
	if r := e.land(c, foreign); r.Code != 401 {
		t.Fatalf("a ticket for another module: %d", r.Code)
	}
	if _, ok := c.cookies[uiSessionCookie]; ok {
		t.Fatal("a refused login set a session cookie")
	}
	r1 := e.land(e.client(t), "cant_"+strings.Repeat("Q", 43))
	r2 := e.land(e.client(t), tk)
	if r1.Code != r2.Code || r1.Body.Len() != r2.Body.Len() {
		t.Fatal("refusals differ")
	}
}

func TestTicketPostNeedsTheDoubleSubmitTokenAndSameSite(t *testing.T) {
	e := newSSOEnv(t, nil)
	tk := e.ticket()
	c := e.client(t)
	e.hold(c, tk)
	page := c.do("GET", "/login/ticket?t="+tk, nil)
	csrf := csrfRe.FindStringSubmatch(page.Body.String())[1]
	same := "Sec-Fetch-Site"
	if r := c.do("POST", "/login/ticket", url.Values{"csrf": {"wrong"}, "ticket": {tk}}, same, "same-origin"); r.Code != 403 {
		t.Fatalf("wrong csrf %d", r.Code)
	}
	if r := c.do("POST", "/login/ticket", url.Values{"ticket": {tk}}, same, "same-origin"); r.Code != 403 {
		t.Fatalf("missing csrf %d", r.Code)
	}
	if r := c.do("POST", "/login/ticket", url.Values{"csrf": {csrf}, "ticket": {tk}}, same, "cross-site"); r.Code != 403 {
		t.Fatalf("cross-site %d", r.Code)
	}
	fresh := e.client(t) // a forged cross-site login has no cookie
	if r := fresh.do("POST", "/login/ticket", url.Values{"csrf": {csrf}, "ticket": {tk}}, same, "same-origin"); r.Code != 403 {
		t.Fatalf("login CSRF %d", r.Code)
	}
	if e.core.Redeems != 0 {
		t.Fatalf("a refused post reached can-core (%d)", e.core.Redeems)
	}
	if r := e.land(e.client(t), tk); r.Code != 303 {
		t.Fatalf("the ticket was spent by refused posts: %d", r.Code)
	}
}

func TestTicketLoginIssuesANewSessionIdWhateverTheBrowserSent(t *testing.T) {
	e := newSSOEnv(t, nil)
	c := e.client(t)
	e.land(c, e.ticket())
	old := c.cookies[uiSessionCookie]
	if old == "" {
		t.Fatal("no session cookie")
	}
	e.land(c, e.ticket())
	if c.cookies[uiSessionCookie] == old || c.cookies[uiSessionCookie] == "" {
		t.Fatal("the session id was reused")
	}
	p := e.client(t)
	p.cookies[uiSessionCookie] = "planted"
	e.land(p, e.ticket())
	if p.cookies[uiSessionCookie] == "planted" || p.cookies[uiSessionCookie] == "" {
		t.Fatal("a planted session id was adopted")
	}
}

func TestReturnPathIsAnOpenRedirectProofLandingPlace(t *testing.T) {
	e := newSSOEnv(t, nil)
	for ret, want := range map[string]string{
		"/invoices": "/invoices", "/": "/", "": "/",
		"//evil.example": "/", "/../api": "/", "https://evil.example": "/", "/x?y=z": "/", "\\evil": "/", "/a//b": "/",
	} {
		tk := e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{PermRead}, ReturnPath: ret})
		r := e.land(e.client(t), tk)
		if r.Code != 303 || r.Header().Get("Location") != want {
			t.Errorf("return %q -> %d %q, want %q", ret, r.Code, r.Header().Get("Location"), want)
		}
	}
}

func TestPermissionsOfATicketSessionAreExactlyTheModulePermissionsReturned(t *testing.T) {
	e := newSSOEnv(t, nil)
	c := e.client(t)
	e.land(c, e.ticket(PermRead))
	c.get("/")
	if p := c.get("/parties"); p.Code != 200 {
		t.Fatalf("read: %d", p.Code)
	}
	if r := c.post("/parties", url.Values{"kind": {"customer"}, "name": {"Acme"}}); r.Code != 403 {
		t.Fatalf("a read-only SSO session created a party: %d", r.Code)
	}
	if r := c.post("/invoices/1/approve", nil); r.Code != 403 {
		t.Fatalf("a read-only SSO session approved: %d", r.Code)
	}
	none := e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{"work.read"}})
	if r := e.land(e.client(t), none); r.Code != 401 {
		t.Fatalf("no finance permission: %d", r.Code)
	}
}

func TestTicketSessionFollowsCanCoreAtTheRevalidationInterval(t *testing.T) {
	e := newSSOEnv(t, nil)
	c := e.client(t)
	e.land(c, e.ticket(PermRead, PermWrite))
	c.get("/")
	create := func(name string) int {
		return c.post("/parties", url.Values{"kind": {"customer"}, "name": {name}}).Code
	}
	if code := create("One"); code != 303 {
		t.Fatalf("write: %d", code)
	}
	before := e.core.Revalidates
	c.get("/")
	if e.core.Revalidates != before {
		t.Fatal("revalidated on every request")
	}
	e.core.SetSessionPermissions("user:alice", []string{PermRead})
	if code := create("Two"); code != 303 {
		t.Fatalf("inside the interval the old permissions still apply: %d", code)
	}
	e.advance(5*time.Minute + time.Second)
	if code := create("Three"); code != 403 {
		t.Fatalf("after the interval write must be gone: %d", code)
	}
	if p := c.get("/"); p.Code != 200 {
		t.Fatalf("read remains: %d", p.Code)
	}
	e.core.EndSessions("user:alice")
	e.advance(5*time.Minute + time.Second)
	if p := c.get("/"); p.Code != 303 || p.Header().Get("Location") != "/login" {
		t.Fatalf("a disabled user kept the session: %d", p.Code)
	}
}

func TestApproverTicketSessionIsRecheckedEveryMinute(t *testing.T) {
	e := newSSOEnv(t, nil)
	c := e.client(t)
	e.land(c, e.ticket(PermRead, PermApprove))
	c.get("/")
	e.core.EndSessions("user:alice")
	e.advance(30 * time.Second)
	if p := c.get("/"); p.Code != 200 {
		t.Fatalf("inside the minute: %d", p.Code)
	}
	e.advance(time.Minute)
	if p := c.get("/"); p.Code != 303 {
		t.Fatalf("an approver kept the session past a minute: %d", p.Code)
	}
}

func TestCanCoreOutageEndsTicketSessionsAtTheNextCheckAndNeverActivates(t *testing.T) {
	e := newSSOEnv(t, nil)
	c := e.client(t)
	tk := e.ticket()
	e.core.SetDown(true)
	if r := e.land(c, tk); r.Code != 503 {
		t.Fatalf("can-core down at redeem: %d", r.Code)
	}
	e.core.SetDown(false)
	if r := e.land(c, tk); r.Code != 303 {
		t.Fatalf("the ticket must survive an outage: %d", r.Code)
	}
	e.core.SetDown(true)
	e.advance(5*time.Minute + time.Second)
	if p := c.get("/"); p.Code != 303 {
		t.Fatalf("an unverifiable session stayed open: %d", p.Code)
	}
}

func TestPastedTokenLoginCanBeTurnedOff(t *testing.T) {
	off := false
	e := newSSOEnv(t, func(c *Config) { c.UI.TokenLogin = &off })
	c := e.client(t)
	page := c.get("/login")
	if page.Code != 200 || strings.Contains(page.Body.String(), `name="token"`) {
		t.Fatalf("the paste form is still offered: %d", page.Code)
	}
	tok := e.token("user:dev", []string{PermRead}, nil, nil, false)
	if r := c.do("POST", "/login", url.Values{"token": {tok}, "csrf": {"x"}}, "Sec-Fetch-Site", "same-origin"); r.Code != 403 {
		t.Fatalf("a pasted token was considered although token_login is off: %d", r.Code)
	}
	if _, ok := c.cookies[uiSessionCookie]; ok {
		t.Fatal("session set")
	}
	if r := e.land(c, e.ticket()); r.Code != 303 {
		t.Fatalf("%d", r.Code)
	}
}

func TestTicketRoutesAreAbsentWithoutCanCore(t *testing.T) {
	u := newUIEnv(t)
	c := u.client(t)
	if r := c.get("/login/ticket?t=cant_" + strings.Repeat("A", 43)); r.Code != 404 {
		t.Fatalf("GET %d", r.Code)
	}
	if r := c.do("POST", "/login/ticket", url.Values{"csrf": {"x"}, "ticket": {"y"}}, "Sec-Fetch-Site", "same-origin"); r.Code != 404 {
		t.Fatalf("POST %d", r.Code)
	}
}

func TestTicketNeverReachesLogsOrErrors(t *testing.T) {
	var logs bytes.Buffer
	core := fakecore.New(t, "finance")
	u := newUIEnv(t, func(c *Config, o *Options) {
		c.Webhook.URL = ""
		o.Can = core.Client(t)
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	e := &ssoEnv{u, core}
	c := e.client(t)
	tk := core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{PermRead}})
	e.land(c, tk)
	c.get("/login/ticket?t=cant_" + strings.Repeat("Z", 43))
	if strings.Contains(logs.String(), tk) || strings.Contains(logs.String(), "cant_") {
		t.Fatalf("a ticket reached the log:\n%s", logs.String())
	}
}

// Login CSRF with two browsers: the attacker launches their own account and sends the link to the
// victim. The victim's browser lacks the launch nonce, so the link does nothing there.
func TestALinkOpenedInAnotherBrowserSignsNobodyIn(t *testing.T) {
	e := newSSOEnv(t, nil)
	attacker, victim := e.client(t), e.client(t)
	tk := e.core.NewTicket(fakecore.Grant{Subject: "user:mallory", Name: "Mallory", Permissions: []string{PermRead, PermWrite}})
	e.hold(attacker, tk)
	r := victim.get("/login/ticket?t=" + tk)
	if r.Code != 401 || strings.Contains(r.Body.String(), "<code>mallory</code>") || hiddenTicketRe.MatchString(r.Body.String()) {
		t.Fatalf("the victim was offered the attacker's account: %d %.300s", r.Code, r.Body.String())
	}
	// With a login token of their own (from the ordinary sign-in page), and even the nonce of a
	// launch of their own, the victim's post signs nobody in.
	post := func() int {
		page := victim.get("/login")
		form := url.Values{"csrf": {csrfRe.FindStringSubmatch(page.Body.String())[1]}, "ticket": {tk}}
		return victim.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin").Code
	}
	if code := post(); code != 401 {
		t.Fatalf("post without the nonce: %d", code)
	}
	e.hold(victim, e.ticket())
	if code := post(); code != 401 {
		t.Fatalf("post with another launch's nonce: %d", code)
	}
	if _, ok := victim.cookies[uiSessionCookie]; ok {
		t.Fatal("the victim got a session")
	}
	if e.core.Redeems != 1 { // without a nonce nothing even reaches can-core
		t.Fatalf("redeem calls: %d", e.core.Redeems)
	}
	// None of that spent the ticket: it still works where it was launched.
	e.land(attacker, tk)
	if _, ok := attacker.cookies[uiSessionCookie]; !ok {
		t.Fatal("the launching browser could not sign in")
	}
}

func TestAnotherAccountsSessionIsNeverReplacedSilently(t *testing.T) {
	e := newSSOEnv(t, nil)
	b := e.client(t)
	e.land(b, e.core.NewTicket(fakecore.Grant{Subject: "user:bob", Name: "Bob Existing", Permissions: []string{PermRead, PermWrite}}))
	old := b.cookies[uiSessionCookie]
	if old == "" {
		t.Fatal("no first session")
	}
	tk := e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Name: "Alice Incoming", Permissions: []string{PermRead, PermWrite}})
	e.hold(b, tk)
	page := b.get("/login/ticket?t=" + tk)
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "Alice Incoming") || !strings.Contains(body, "<code>alice</code>") || !strings.Contains(body, "Bob Existing") || !strings.Contains(body, `name="replace" value="user:alice"`) {
		t.Fatalf("no warning naming both accounts: %d %.900s", page.Code, body)
	}
	form := url.Values{"csrf": {csrfRe.FindStringSubmatch(body)[1]}, "ticket": {tk}}
	before := e.core.Redeems
	r := b.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin")
	if r.Code != 409 || e.core.Redeems != before || b.cookies[uiSessionCookie] != old {
		t.Fatalf("replaced without confirmation: %d", r.Code)
	}
	if p := b.get("/"); p.Code != 200 {
		t.Fatalf("the existing session was dropped: %d", p.Code)
	}
	form.Set("csrf", csrfRe.FindStringSubmatch(r.Body.String())[1])
	form.Set("replace", "user:alice")
	if r := b.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin"); r.Code != 303 || b.cookies[uiSessionCookie] == old {
		t.Fatalf("confirmed: %d %.300s", r.Code, r.Body.String())
	}
}
