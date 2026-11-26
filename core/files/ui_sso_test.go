package files

import (
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

func newSSOEnv(t *testing.T, mods ...func(*Config, *StoreOptions)) *ssoEnv {
	t.Helper()
	core := fakecore.New(t, "files")
	u := newUIEnvWith(t, uiOpts{sessions: core.Client(t), mods: mods})
	core.Now = u.now
	return &ssoEnv{u, core}
}

func (e *ssoEnv) ticket(perms ...string) string {
	if len(perms) == 0 {
		perms = []string{PermRead, PermWrite}
	}
	return e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Name: "Alice", Permissions: perms})
}

var hiddenTicketRe = regexp.MustCompile(`name="ticket" value="([^"]+)"`)

// hold gives the browser the nonce cookie can-core set in it at launch (the compose stack and these
// tests serve plain http, so the name has no __Secure- prefix).
func (e *ssoEnv) hold(c *client, ticket string) {
	if n := e.core.Binding(ticket); n != "" {
		c.cookies["can_launch_files"] = n
	}
}

// land opens the link from the launcher and posts the interstitial's form, like a browser.
func (e *ssoEnv) land(c *client, ticket string) *httptest.ResponseRecorder {
	c.t.Helper()
	e.hold(c, ticket)
	page := c.do("GET", "/login/ticket?t="+ticket, nil)
	if page.Code != 200 {
		return page
	}
	return c.do("POST", "/login/ticket", url.Values{"csrf": {csrfRe.FindStringSubmatch(page.Body.String())[1]},
		"ticket": {hiddenTicketRe.FindStringSubmatch(page.Body.String())[1]}}, "Sec-Fetch-Site", "same-origin")
}

func (e *ssoEnv) signIn(t *testing.T, g fakecore.Grant) *client {
	t.Helper()
	c := e.client(t)
	if r := e.land(c, e.core.NewTicket(g)); r.Code != 303 {
		t.Fatalf("sign in: %d %s", r.Code, r.Body)
	}
	return c
}

func TestUITicketSignIn(t *testing.T) {
	e := newSSOEnv(t)
	c := e.client(t)
	tk := e.ticket()
	e.hold(c, tk)
	for i := 0; i < 3; i++ { // a link scanner, a prefetch, the person: GET never consumes
		want(t, c.get("/login/ticket?t="+tk), 200, `method="post" action="/login/ticket"`, "Alice")
	}
	if e.core.Redeems != 0 {
		t.Fatal("GET reached can-core")
	}
	r := e.land(c, tk)
	if r.Code != 303 || r.Header().Get("Location") != "/" {
		t.Fatal(r.Code, r.Header())
	}
	want(t, c.get("/"), 200, "Spaces")
	for n, v := range c.cookies {
		if strings.Contains(v, "cant_") || strings.Contains(v, "cans_") {
			t.Fatalf("cookie %s carries a credential", n)
		}
	}
	sp := created(t, c.post("/spaces", url.Values{"name": {"From SSO"}}))
	created(t, c.upload("/spaces/"+sp+"/files", "a.pdf", doc))

	// Replay, a ticket for another module and an unknown ticket all look the same.
	for _, bad := range []string{tk, e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{PermWrite}, Module: "people"}), "cant_" + strings.Repeat("Q", 43)} {
		other := e.client(t)
		if r := e.land(other, bad); r.Code != 401 {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		if _, ok := other.cookies[uiSessionCookie]; ok {
			t.Fatal("a refused sign-in set a session")
		}
	}
	// Nothing of the ticket in the log.
	if strings.Contains(e.logs.String(), "cant_") {
		t.Fatal("a ticket reached the log")
	}
}

// Permissions are those can-core returns, followed at each recheck; a disabled account loses the
// session.
func TestUITicketSessionFollowsCanCore(t *testing.T) {
	e := newSSOEnv(t)
	c := e.signIn(t, fakecore.Grant{Subject: "user:alice", Permissions: []string{PermRead, "work.admin"}})
	page := want(t, c.get("/"), 200)
	if strings.Contains(page, "New space") || strings.Contains(page, "Holds and retention") {
		t.Fatal("more than files.read")
	}
	want(t, c.post("/spaces", url.Values{"name": {"x"}, "csrf": {csrfRe.FindStringSubmatch(page)[1]}}), 403)
	e.core.SetSessionPermissions("user:alice", []string{PermWrite})
	e.advance(uiRecheckDefault + time.Second)
	want(t, c.get("/"), 200, "New space")
	e.core.EndSessions("user:alice")
	e.advance(uiRecheckDefault + time.Second)
	if r := c.get("/"); r.Code != 303 || r.Header().Get("Location") != "/login" {
		t.Fatal("the session survived", r.Code)
	}
	if r := e.land(e.client(t), e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{"people.read"}})); r.Code != 401 {
		t.Fatal("a ticket without a files permission", r.Code)
	}
	if r := e.land(e.client(t), e.core.NewTicket(fakecore.Grant{Subject: "svc:static", Permissions: []string{PermAdmin}})); r.Code != 401 {
		t.Fatal("a ticket for a service", r.Code)
	}
	// can-core's deadline ends the session whatever the timeouts say.
	c = e.signIn(t, fakecore.Grant{Subject: "user:alice", Permissions: []string{PermRead}, SessionExpires: e.now().Add(2 * time.Minute)})
	want(t, c.get("/"), 200)
	e.advance(2 * time.Minute)
	if r := c.get("/"); r.Code != 303 {
		t.Fatal("outlived the deadline", r.Code)
	}
}

func TestUITicketNeedsTheLaunchingBrowserAndConfirmsAnAccountSwitch(t *testing.T) {
	e := newSSOEnv(t)
	attacker, victim := e.client(t), e.client(t)
	tk := e.core.NewTicket(fakecore.Grant{Subject: "user:mallory", Name: "Mallory", Permissions: []string{PermWrite}})
	e.hold(attacker, tk)
	if r := victim.get("/login/ticket?t=" + tk); r.Code != 401 || strings.Contains(r.Body.String(), "mallory") {
		t.Fatal("another browser was offered the account", r.Code)
	}
	if r := e.land(attacker, tk); r.Code != 303 {
		t.Fatal(r.Code)
	}

	c := e.signIn(t, fakecore.Grant{Subject: "user:alice", Name: "Alice", Permissions: []string{PermWrite}})
	bob := e.core.NewTicket(fakecore.Grant{Subject: "user:bob", Name: "Bob", Permissions: []string{PermWrite}})
	e.hold(c, bob)
	page := want(t, c.get("/login/ticket?t="+bob), 200, "already signed in as", "Alice")
	form := url.Values{"csrf": {csrfRe.FindStringSubmatch(page)[1]}, "ticket": {bob}}
	if r := c.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin"); r.Code != 409 {
		t.Fatal("replaced without asking", r.Code)
	}
	form.Set("replace", "user:bob")
	if r := c.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin"); r.Code != 303 {
		t.Fatal(r.Code, r.Body)
	}
	form.Del("csrf")
	if r := c.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin"); r.Code != 403 {
		t.Fatal("no double-submit token", r.Code)
	}
}

func TestUITicketLandingAndFailures(t *testing.T) {
	e := newSSOEnv(t)
	sp := "/spaces/spc_000000000000000000000000"
	for ret, wantLoc := range map[string]string{sp: sp, "": "/", "//evil.example": "/", "https://evil.example": "/", "/a?b=c": "/", "/../x": "/"} {
		c := e.client(t)
		r := e.land(c, e.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{PermRead}, ReturnPath: ret}))
		if r.Header().Get("Location") != wantLoc {
			t.Errorf("%q landed on %q", ret, r.Header().Get("Location"))
		}
	}
	tk := e.ticket()
	c := e.client(t)
	e.hold(c, tk)
	e.core.SetDown(true)
	if r := c.get("/login/ticket?t=" + tk); r.Code != 503 {
		t.Fatal(r.Code)
	}
	e.core.SetDown(false)
	want(t, c.get("/login/ticket?t=junk"), 400)

	// Without can-core there is no ticket route at all.
	plain := newUIEnv(t)
	want(t, plain.client(t).get("/login/ticket?t="+tk), 404)
}

func TestUIPastedTokenLoginCanBeTurnedOff(t *testing.T) {
	off := false
	e := newSSOEnv(t, func(c *Config, _ *StoreOptions) { c.UI.TokenLogin = &off })
	c := e.client(t)
	page := want(t, c.get("/login"), 200, "Open files from the Apps page")
	if strings.Contains(page, `name="token"`) {
		t.Fatal("the token form is offered")
	}
	if r := c.do("POST", "/login", url.Values{"token": {patAlice}}, "Sec-Fetch-Site", "same-origin"); r.Code != 403 {
		t.Fatal(r.Code)
	}
	e.signIn(t, fakecore.Grant{Subject: "user:alice", Permissions: []string{PermRead}})
}

// A ticket session has no token to type again. Its step-up for the irreversible erasure is a recent
// sign-in at the admin UI plus a live check with can-core at that moment.
func TestUITicketSessionErasureNeedsARecentAdminSignInAndALiveCheck(t *testing.T) {
	e := newSSOEnv(t)
	owner := e.signIn(t, fakecore.Grant{Subject: "user:alice", Permissions: []string{PermWrite}})
	owner.get("/")
	sp := created(t, owner.post("/spaces", url.Values{"name": {"S"}}))
	upload := func(subject string) {
		owner.get("/")
		created(t, owner.upload("/spaces/"+sp+"/files", "a.pdf", doc, "subject", subject))
	}
	upload("cand_one")
	upload("cand_two")
	admin := []string{PermAdmin}
	login := func(g fakecore.Grant) *client {
		g.Subject, g.Permissions = "user:root", admin
		c := e.signIn(t, g)
		c.get("/")
		return c
	}
	erase := func(c *client, subject string) int {
		return c.post("/admin/erasure", url.Values{"subject": {subject}, "confirm": {"ERASE"}}).Code
	}

	c := login(fakecore.Grant{})
	page := want(t, c.get("/admin/erasure?subject=cand_one"), 200, "last 5 minutes")
	if strings.Contains(page, `name="token"`) {
		t.Fatal("a ticket session is asked for a token it does not have")
	}
	if code := erase(c, "cand_one"); code != 200 {
		t.Fatal("just after signing in", code)
	}

	for name, g := range map[string]fakecore.Grant{
		"old sign-in":   {AuthenticatedAt: e.now().Add(-time.Hour)},
		"unknown time":  {NoAuthTime: true},
		"in the future": {AuthenticatedAt: e.now().Add(time.Hour)},
	} {
		c := login(g)
		if r := c.post("/admin/erasure", url.Values{"subject": {"cand_two"}, "confirm": {"ERASE"}, "token": {patRoot}}); r.Code != 422 || !strings.Contains(r.Body.String(), "sign in again") {
			t.Fatalf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	c = login(fakecore.Grant{})
	e.advance(uiStepUpMaxAge + time.Second)
	if code := erase(c, "cand_two"); code == 200 {
		t.Fatal("a sign-in grown old")
	}
	// The permission is gone at can-core: refused at once, not at the next periodic check.
	c = login(fakecore.Grant{})
	e.core.SetSessionPermissions("user:root", []string{PermRead})
	if code := erase(c, "cand_two"); code == 200 {
		t.Fatal("erased without files.admin")
	}
	e.core.SetSessionPermissions("user:root", admin)
	c = login(fakecore.Grant{})
	e.core.SetDown(true)
	if code := erase(c, "cand_two"); code == 200 {
		t.Fatal("erased with can-core unreachable")
	}
	e.core.SetDown(false)
	if n := e.count(`SELECT COUNT(*) FROM files WHERE status = 'active'`); n != 1 {
		t.Fatal("files left", n)
	}
}
