package people

import (
	"bytes"
	"log/slog"
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

const ssoSubject = "user:alice"

// newSSOEnv builds the UI over a fake can-core, with a directory record linked to user:alice.
func newSSOEnv(t *testing.T) *ssoEnv {
	t.Helper()
	u := newUIEnv(t)
	core := fakecore.New(t, "people")
	u.ui.Sessions = core.Client(t)
	core.Now = u.ui.Now
	_, err := u.s.CreatePerson(u.ctx, u.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "Alice", FamilyName: "Example", WorkEmail: "alice@example.com", SSOSubject: ssoSubject, StartDate: "2025-01-06"})
	u.must(err)
	return &ssoEnv{u, core}
}

func (e *ssoEnv) ticket(perms ...string) string {
	if len(perms) == 0 {
		perms = []string{"people.read", "performance.read", "performance.write"}
	}
	return e.core.NewTicket(fakecore.Grant{Subject: ssoSubject, Name: "Alice (account)", Permissions: perms})
}

var hiddenTicketRe = regexp.MustCompile(`name="ticket" value="([^"]+)"`)

// hold gives the browser the nonce cookie can-core set in it at launch.
func (e *ssoEnv) hold(c *client, ticket string) {
	if n := e.core.Binding(ticket); n != "" {
		c.cookies["__Secure-can_launch_people"] = n
	}
}

// land opens the link from the launcher and posts the interstitial's form, like a browser.
func (e *ssoEnv) land(c *client, ticket string) string {
	e.hold(c, ticket)
	e.t.Helper()
	page := c.get("/login/ticket?t=" + ticket)
	if page.Code != 200 { // refused or unavailable before any form was offered
		c.landed = page
		return ""
	}
	w := c.do("POST", "/login/ticket", url.Values{"csrf": {csrfRe.FindStringSubmatch(page.Body.String())[1]}, "ticket": {hiddenTicketRe.FindStringSubmatch(page.Body.String())[1]}}, "Sec-Fetch-Site", "same-origin")
	if w.Code == 303 {
		c.get("/") // picks up the session csrf token
	}
	c.landed = w
	return w.Header().Get("Location")
}

func hasSession(c *client) bool { _, ok := c.cookies["can_people_session"]; return ok }

func newGoal() url.Values {
	return url.Values{"scope": {"person"}, "title": {"Ship it"}, "period": {"2026-Q1"}, "visibility": {"manager"}, "description": {"d"}}
}

func TestTicketLinkOnlyShowsAFormAndDoesNotConsumeTheTicket(t *testing.T) {
	e := newSSOEnv(t)
	c := e.client(t)
	tk := e.ticket()
	e.hold(c, tk)
	for i := 0; i < 3; i++ { // a link scanner, a prefetch, the person: GET never consumes
		w := c.get("/login/ticket?t=" + tk)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `method="post" action="/login/ticket"`) {
			t.Fatalf("%d %.300s", w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "http-equiv") || strings.Contains(w.Body.String(), "<script") {
			t.Fatal("the page moves on by itself: a scanner would consume the ticket")
		}
		for h, want := range map[string]string{"Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
			if w.Header().Get(h) != want {
				t.Fatalf("%s = %q", h, w.Header().Get(h))
			}
		}
		if _, ok := c.cookies["can_people_login_csrf"]; !ok {
			t.Fatal("no login csrf cookie")
		}
	}
	if e.core.Redeems != 0 {
		t.Fatalf("GET reached can-core %d times", e.core.Redeems)
	}
	if loc := e.land(c, tk); c.landed.Code != 303 || loc != "/" {
		t.Fatalf("the ticket was spent by a GET: %d %.300s", c.landed.Code, c.landed.Body)
	}
}

func TestMalformedTicketLinkAndNoSessionAPI(t *testing.T) {
	e := newSSOEnv(t)
	c := e.client(t)
	if w := c.get("/login/ticket?t=junk"); w.Code != 400 {
		t.Fatalf("junk ticket: %d", w.Code)
	}
	e.ui.Sessions = nil
	if w := c.get("/login/ticket?t=" + e.ticket()); w.Code != 404 {
		t.Fatalf("no can-core: GET %d", w.Code)
	}
	if w := c.do("POST", "/login/ticket", url.Values{"csrf": {"x"}, "ticket": {"x"}}, "Sec-Fetch-Site", "same-origin"); w.Code != 404 {
		t.Fatalf("no can-core: POST %d", w.Code)
	}
}

func TestTicketLoginMakesASessionOfTheModuleAndLandsOnACleanURL(t *testing.T) {
	e := newSSOEnv(t)
	c := e.client(t)
	loc := e.land(c, e.ticket())
	if c.landed.Code != 303 || loc != "/" {
		t.Fatalf("%d %q", c.landed.Code, loc)
	}
	if p := c.get("/"); p.Code != 200 || !strings.Contains(p.Body.String(), "Alice") || strings.Contains(p.Body.String(), "user:alice") {
		t.Fatalf("signed-in page: %d %.300s", p.Code, p.Body)
	}
	for k, v := range c.cookies {
		if strings.Contains(v, "cant_") || strings.Contains(v, "cans_") || strings.Contains(v, "canpat_") {
			t.Fatalf("cookie %s carries a credential", k)
		}
	}
	if _, ok := c.cookies["can_people_login_csrf"]; ok {
		t.Fatal("login csrf cookie kept")
	}
	if w := c.post("/logout", nil); w.Code != 303 {
		t.Fatalf("logout %d", w.Code)
	}
	if p := c.get("/"); p.Code != 303 {
		t.Fatalf("after logout %d", p.Code)
	}
}

func TestTicketReplayForeignAndUnlinkedAreRefusedWithoutASession(t *testing.T) {
	e := newSSOEnv(t)
	tk := e.ticket()
	e.land(e.client(t), tk)
	c := e.client(t)
	e.land(c, tk)
	if c.landed.Code != 401 || !strings.Contains(c.landed.Body.String(), `role="alert"`) || hasSession(c) {
		t.Fatalf("replay: %d %.300s", c.landed.Code, c.landed.Body)
	}
	foreign := e.core.NewTicket(fakecore.Grant{Subject: ssoSubject, Permissions: []string{"people.read"}, Module: "work"})
	c = e.client(t)
	if e.land(c, foreign); c.landed.Code != 401 || hasSession(c) {
		t.Fatalf("a ticket for another module: %d", c.landed.Code)
	}
	// A person without a directory record (or with no people permission) gets no session either.
	for name, g := range map[string]fakecore.Grant{
		"unlinked":      {Subject: "user:nobody", Permissions: []string{"people.read"}},
		"no permission": {Subject: ssoSubject, Permissions: []string{"work.read"}},
		"bad subject":   {Subject: "bad subject!", Permissions: []string{"people.read"}},
	} {
		c = e.client(t)
		if e.land(c, e.core.NewTicket(g)); c.landed.Code != 401 || hasSession(c) {
			t.Fatalf("%s: %d", name, c.landed.Code)
		}
	}
	// Every refusal reads the same.
	c1, c2 := e.client(t), e.client(t)
	e.land(c1, "cant_"+strings.Repeat("Q", 43))
	e.land(c2, tk)
	if c1.landed.Code != c2.landed.Code || len(c1.landed.Body.String()) != len(c2.landed.Body.String()) {
		t.Fatal("refusals differ")
	}
}

func TestTicketPostNeedsTheDoubleSubmitTokenAndSameSite(t *testing.T) {
	e := newSSOEnv(t)
	tk := e.ticket()
	c := e.client(t)
	e.hold(c, tk)
	page := c.get("/login/ticket?t=" + tk)
	csrf := csrfRe.FindStringSubmatch(page.Body.String())[1]
	post := func(c *client, f url.Values, hdr ...string) int {
		return c.do("POST", "/login/ticket", f, append([]string{"Sec-Fetch-Site", "same-origin"}, hdr...)...).Code
	}
	if got := post(c, url.Values{"csrf": {"wrong"}, "ticket": {tk}}); got != 403 {
		t.Fatalf("wrong csrf %d", got)
	}
	if got := post(c, url.Values{"ticket": {tk}}); got != 403 {
		t.Fatalf("missing csrf %d", got)
	}
	if got := c.do("POST", "/login/ticket", url.Values{"csrf": {csrf}, "ticket": {tk}}, "Sec-Fetch-Site", "cross-site").Code; got != 403 {
		t.Fatalf("cross-site %d", got)
	}
	// A forged cross-site login (an attacker's ticket in the victim's browser) has no cookie.
	if got := post(e.client(t), url.Values{"csrf": {csrf}, "ticket": {tk}}); got != 403 {
		t.Fatalf("login CSRF %d", got)
	}
	if e.core.Redeems != 0 {
		t.Fatalf("a refused post reached can-core (%d)", e.core.Redeems)
	}
	if e.land(e.client(t), tk); !strings.HasPrefix(tk, "cant_") || e.core.Redeems != 1 {
		t.Fatalf("the ticket was spent by refused posts (%d redeems)", e.core.Redeems)
	}
}

func TestFailedTicketsAreThrottledButAValidOneNever(t *testing.T) {
	e := newSSOEnv(t)
	c := e.client(t)
	bad := func() int { // posted directly: a bad link is already refused before any form
		page := c.get("/login")
		form := url.Values{"csrf": {csrfRe.FindStringSubmatch(page.Body.String())[1]}, "ticket": {"cant_" + strings.Repeat("Q", 43)}}
		return c.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin").Code
	}
	for i := 0; i < failedLoginBurst; i++ {
		bad()
	}
	if code := bad(); code != 429 {
		t.Fatalf("a flood of bad tickets was not throttled: %d", code)
	}
	if e.land(c, e.ticket()); c.landed.Code != 303 {
		t.Fatalf("a valid ticket was throttled: %d", c.landed.Code)
	}
}

func TestTicketLoginIssuesANewSessionIdWhateverTheBrowserSent(t *testing.T) {
	e := newSSOEnv(t)
	c := e.client(t)
	e.land(c, e.ticket())
	old := c.cookies["can_people_session"]
	if old == "" {
		t.Fatal("no session cookie")
	}
	e.land(c, e.ticket()) // the same browser signs in again: the id changes
	if c.cookies["can_people_session"] == old || c.cookies["can_people_session"] == "" {
		t.Fatal("the session id was reused")
	}
	c = e.client(t)
	c.cookies["can_people_session"] = "planted"
	e.land(c, e.ticket())
	if v := c.cookies["can_people_session"]; v == "planted" || v == "" {
		t.Fatalf("a planted session id was adopted: %q", v)
	}
}

func TestReturnPathIsAnOpenRedirectProofLandingPlace(t *testing.T) {
	e := newSSOEnv(t)
	for ret, want := range map[string]string{
		"/people/pers_x": "/people/pers_x", "/goals": "/goals", "/": "/", "": "/",
		"//evil.example": "/", "/../api": "/", "https://evil.example": "/", "/x?y=z": "/", "\\evil": "/", "/a//b": "/",
	} {
		tk := e.core.NewTicket(fakecore.Grant{Subject: ssoSubject, Permissions: []string{"people.read"}, ReturnPath: ret})
		c := e.client(t)
		if loc := e.land(c, tk); c.landed.Code != 303 || loc != want {
			t.Errorf("return %q -> %d %q, want %q", ret, c.landed.Code, loc, want)
		}
	}
}

func TestPermissionsOfATicketSessionAreExactlyTheModulePermissionsReturned(t *testing.T) {
	e := newSSOEnv(t)
	c := e.client(t)
	e.land(c, e.ticket("people.read"))
	if p := c.get("/people"); p.Code != 200 {
		t.Fatalf("read: %d", p.Code)
	}
	if w := c.post("/goals", newGoal()); w.Code != 403 {
		t.Fatalf("a read-only SSO session created a goal: %d", w.Code)
	}
	c = e.client(t)
	e.land(c, e.ticket("people.read", "performance.read", "performance.write"))
	if w := c.post("/goals", newGoal()); w.Code != 303 {
		t.Fatalf("write: %d %.300s", w.Code, w.Body)
	}
}

func TestTicketSessionFollowsCanCoreAtTheRevalidationInterval(t *testing.T) {
	e := newSSOEnv(t)
	c := e.client(t)
	e.land(c, e.ticket())
	if w := c.post("/goals", newGoal()); w.Code != 303 {
		t.Fatalf("write: %d %.200s", w.Code, w.Body)
	}
	before := e.core.Revalidates
	c.get("/")
	if e.core.Revalidates != before {
		t.Fatal("revalidated on every request")
	}
	// The write permission is taken away: it applies at the next check, not before.
	e.core.SetSessionPermissions(ssoSubject, []string{"people.read"})
	if w := c.post("/goals", newGoal()); w.Code != 303 {
		t.Fatalf("inside the interval the old permissions still apply: %d", w.Code)
	}
	e.now = e.now.Add(e.ui.RecheckEvery + time.Second)
	if w := c.post("/goals", newGoal()); w.Code != 403 {
		t.Fatalf("after the interval write must be gone: %d", w.Code)
	}
	if p := c.get("/"); p.Code != 200 {
		t.Fatalf("read remains: %d", p.Code)
	}
	// The account is disabled in can-core: the next check ends the module session.
	e.core.EndSessions(ssoSubject)
	e.now = e.now.Add(e.ui.RecheckEvery + time.Second)
	if p := c.get("/"); p.Code != 303 || p.Header().Get("Location") != "/login" {
		t.Fatalf("a disabled user kept the session: %d", p.Code)
	}
}

func TestHRTicketSessionsAreRecheckedEveryMinute(t *testing.T) {
	e := newSSOEnv(t)
	c := e.client(t)
	e.land(c, e.ticket("people.read", "people.write", "people.admin"))
	e.now = e.now.Add(time.Minute + time.Second)
	e.core.EndSessions(ssoSubject)
	if p := c.get("/"); p.Code != 303 {
		t.Fatalf("an hr session outlived the one-minute check: %d", p.Code)
	}
	// Losing every people permission ends the session as well.
	c = e.client(t)
	e.land(c, e.ticket("people.read", "people.write"))
	e.core.SetSessionPermissions(ssoSubject, []string{"work.read"})
	e.now = e.now.Add(time.Minute + time.Second)
	if p := c.get("/"); p.Code != 303 {
		t.Fatalf("a session with no people permission stayed: %d", p.Code)
	}
}

func TestCanCoreOutageEndsTicketSessionsAtTheNextCheckAndNeverActivates(t *testing.T) {
	e := newSSOEnv(t)
	c := e.client(t)
	tk := e.ticket()
	e.core.SetDown(true)
	if e.land(c, tk); c.landed.Code != 503 || hasSession(c) {
		t.Fatalf("can-core down at redeem: %d", c.landed.Code)
	}
	e.core.SetDown(false)
	if e.land(c, tk); c.landed.Code != 303 {
		t.Fatalf("the ticket must survive an outage: %d", c.landed.Code)
	}
	e.core.SetDown(true)
	e.now = e.now.Add(e.ui.RecheckEvery + time.Second)
	if p := c.get("/"); p.Code != 303 {
		t.Fatalf("an unverifiable session stayed open: %d", p.Code)
	}
}

func TestPastedTokenLoginCanBeTurnedOff(t *testing.T) {
	e := newSSOEnv(t)
	e.ui.NoTokenLogin = true
	c := e.client(t)
	page := c.get("/login")
	if page.Code != 200 || strings.Contains(page.Body.String(), `name="token"`) || !strings.Contains(page.Body.String(), "Apps page") {
		t.Fatalf("the paste form is still offered: %d %.300s", page.Code, page.Body)
	}
	_, ann := e.person("Ann", "", RoleEmployee)
	csrf := csrfRe.FindStringSubmatch(page.Body.String())
	if csrf == nil {
		csrf = []string{"", "x"}
	}
	w := c.do("POST", "/login", url.Values{"token": {e.token(ann)}, "csrf": {csrf[1]}}, "Sec-Fetch-Site", "same-origin")
	if w.Code != 403 || hasSession(c) {
		t.Fatalf("a pasted token logged in although token_login is off: %d", w.Code)
	}
	if e.land(c, e.ticket()); c.landed.Code != 303 {
		t.Fatalf("%d", c.landed.Code)
	}
	// With it on (the default) the form is there.
	e.ui.NoTokenLogin = false
	if p := e.client(t).get("/login"); !strings.Contains(p.Body.String(), `name="token"`) {
		t.Fatal("token form missing by default")
	}
}

func TestTokenLoginConfigDefaultsToOn(t *testing.T) {
	c, err := ParseConfig([]byte("keyring_file: /k\n"))
	if err != nil || !c.UI.tokenLogin() {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = ParseConfig([]byte("keyring_file: /k\nui:\n  token_login: false\n"))
	if err != nil || c.UI.tokenLogin() {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestTicketNeverReachesLogsOrErrors(t *testing.T) {
	e := newSSOEnv(t)
	var logs bytes.Buffer
	e.ui.Log = slog.New(slog.NewTextHandler(&logs, nil))
	c := e.client(t)
	tk := e.ticket()
	e.land(c, tk)
	c.get("/login/ticket?t=cant_" + strings.Repeat("Z", 43))
	if strings.Contains(logs.String(), tk) || strings.Contains(logs.String(), "cant_") {
		t.Fatalf("a ticket reached the log:\n%s", logs.String())
	}
}

// Login CSRF with two browsers: the attacker launches their own account and sends the link to the
// victim. The victim's browser lacks the launch nonce, so the link does nothing there.
func TestALinkOpenedInAnotherBrowserSignsNobodyIn(t *testing.T) {
	e := newSSOEnv(t)
	_, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "Bob", FamilyName: "Example", WorkEmail: "bob@example.com", SSOSubject: "user:bob", StartDate: "2025-01-06"})
	e.must(err)
	_, err = e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "Mallory", FamilyName: "Example", WorkEmail: "mallory@example.com", SSOSubject: "user:mallory", StartDate: "2025-01-06"})
	e.must(err)
	attacker, victim := e.client(t), e.client(t)
	tk := e.core.NewTicket(fakecore.Grant{Subject: "user:mallory", Name: "Mallory", Permissions: []string{"people.read", "performance.read", "performance.write"}})
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
	if _, ok := victim.cookies["can_people_session"]; ok {
		t.Fatal("the victim got a session")
	}
	if e.core.Redeems != 1 { // without a nonce nothing even reaches can-core
		t.Fatalf("redeem calls: %d", e.core.Redeems)
	}
	// None of that spent the ticket: it still works where it was launched.
	e.land(attacker, tk)
	if _, ok := attacker.cookies["can_people_session"]; !ok {
		t.Fatal("the launching browser could not sign in")
	}
}

func TestAnotherAccountsSessionIsNeverReplacedSilently(t *testing.T) {
	e := newSSOEnv(t)
	_, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "Bob", FamilyName: "Example", WorkEmail: "bob@example.com", SSOSubject: "user:bob", StartDate: "2025-01-06"})
	e.must(err)
	_, err = e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "Mallory", FamilyName: "Example", WorkEmail: "mallory@example.com", SSOSubject: "user:mallory", StartDate: "2025-01-06"})
	e.must(err)
	b := e.client(t)
	e.land(b, e.core.NewTicket(fakecore.Grant{Subject: "user:bob", Name: "Bob Existing", Permissions: []string{"people.read", "performance.read", "performance.write"}}))
	old := b.cookies["can_people_session"]
	if old == "" {
		t.Fatal("no first session")
	}
	tk := e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Name: "Alice Incoming", Permissions: []string{"people.read", "performance.read", "performance.write"}})
	e.hold(b, tk)
	page := b.get("/login/ticket?t=" + tk)
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "Alice Incoming") || !strings.Contains(body, "<code>alice</code>") || !strings.Contains(body, "Bob Existing") || !strings.Contains(body, `name="replace" value="user:alice"`) {
		t.Fatalf("no warning naming both accounts: %d %.900s", page.Code, body)
	}
	form := url.Values{"csrf": {csrfRe.FindStringSubmatch(body)[1]}, "ticket": {tk}}
	before := e.core.Redeems
	r := b.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin")
	if r.Code != 409 || e.core.Redeems != before || b.cookies["can_people_session"] != old {
		t.Fatalf("replaced without confirmation: %d", r.Code)
	}
	if p := b.get("/"); p.Code != 200 {
		t.Fatalf("the existing session was dropped: %d", p.Code)
	}
	form.Set("csrf", csrfRe.FindStringSubmatch(r.Body.String())[1])
	form.Set("replace", "user:alice")
	if r := b.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin"); r.Code != 303 || b.cookies["can_people_session"] == old {
		t.Fatalf("confirmed: %d %.300s", r.Code, r.Body.String())
	}
}

// can-core's session_expires_at (the identity provider's deadline) ends the module session.
func TestTicketSessionNeverOutlivesCanCoresDeadline(t *testing.T) {
	e := newSSOEnv(t)
	g := fakecore.Grant{Subject: ssoSubject, Permissions: []string{"people.read"}, SessionExpires: e.now.Add(30 * time.Second)}
	c := e.client(t)
	e.land(c, e.core.NewTicket(g))
	if p := c.get("/"); p.Code != 200 {
		t.Fatalf("before the deadline: %d %.300s", p.Code, p.Body)
	}
	e.now = e.now.Add(30 * time.Second)
	if p := c.get("/"); p.Code != 303 {
		t.Fatalf("the session outlived the deadline: %d", p.Code)
	}
	g.SessionExpires = e.now.Add(-time.Second)
	c = e.client(t)
	e.land(c, e.core.NewTicket(g))
	if p := c.get("/"); p.Code != 303 {
		t.Fatalf("a session past its deadline was started: %d", p.Code)
	}
}
