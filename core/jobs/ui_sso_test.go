package jobs

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
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

var (
	interviewerPerms = []string{"ats.interview"}
	ssoHiddenRe      = regexp.MustCompile(`name="ticket" value="([^"]+)"`)
)

type ssoEnv struct {
	*env
	core *fakecore.Core
}

func newSSOEnv(t *testing.T, logger *slog.Logger, mut func(*Config)) *ssoEnv {
	t.Helper()
	e := newEnv(t, nil)
	core := fakecore.New(t, "jobs")
	cfg := DefaultConfig()
	dir := t.TempDir()
	cfg.DBPath = filepath.Join(dir, "jobs.db")
	cfg.ResumeDir = filepath.Join(dir, "resumes")
	cfg.Company.Name, cfg.Company.BaseURL = "Example Co", "https://example.test"
	cfg.UI.Enabled, cfg.UI.InsecureCookies = true, true
	cfg.Approval.Required = false
	if mut != nil {
		mut(cfg)
	}
	kr, err := ParseKeyring("k1 " + testKeyHex + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	app, err := NewApp(cfg, Secrets{AdminToken: adminTok, HashKey: "hash-key-0123456789abcdef0123456789abcdef", FormSecret: "form-secret-0123456789abcdef", CanSecret: canSecret},
		Options{Logger: logger, Keyring: kr, Can: core.Client(t), Now: e.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	e.app, e.h = app, app.Handler()
	return &ssoEnv{e, core}
}

func (s *ssoEnv) browser(t *testing.T) *client {
	return &client{t: t, e: s.env, cookies: map[string]string{}}
}

func (s *ssoEnv) ticket(perms ...string) string {
	if len(perms) == 0 {
		perms = recruiterPerms
	}
	return s.core.NewTicket(fakecore.Grant{Subject: "user:alice", Name: "Alice Example", Permissions: perms})
}

// hold gives the browser the nonce cookie can-core set in it at launch.
func (s *ssoEnv) hold(c *client, ticket string) {
	if n := s.core.Binding(ticket); n != "" {
		c.cookies["__Secure-can_launch_jobs"] = n
	}
}

// land opens the link from the launcher and posts the interstitial's form, like a browser.
func (s *ssoEnv) land(c *client, ticket string) *httptest.ResponseRecorder {
	s.hold(c, ticket)
	c.t.Helper()
	page := c.get("/login/ticket?t=" + ticket)
	if page.Code != 200 { // refused or unavailable before any form was offered
		return page
	}
	return c.do("POST", "/login/ticket", url.Values{"csrf": {csrfRe.FindStringSubmatch(page.Body.String())[1]}, "ticket": {ssoHiddenRe.FindStringSubmatch(page.Body.String())[1]}}, "Sec-Fetch-Site", "same-origin")
}

var ssoJobForm = url.Values{"title": {"Staff Engineer"}, "department": {"Engineering"}, "city": {"Austin"}, "region": {"TX"}, "country": {"US"}, "remote": {"hybrid"},
	"employment_type": {"full_time"}, "salary_min": {"100000"}, "salary_max": {"140000"}, "salary_currency": {"USD"}, "salary_period": {"year"},
	"hiring_manager": {"alice"}, "headcount": {"1"}, "section_heading_0": {"About"}, "section_body_0": {"Do good work."}}

func cloneForm(v url.Values) url.Values {
	out := url.Values{}
	for k, x := range v {
		out[k] = append([]string(nil), x...)
	}
	return out
}

func TestTicketLinkOnlyShowsAFormAndDoesNotConsumeTheTicket(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	b := s.browser(t)
	tk := s.ticket()
	s.hold(b, tk)
	for i := 0; i < 3; i++ { // a link scanner, a prefetch, the person: GET never consumes
		r := b.get("/login/ticket?t=" + tk)
		if r.Code != 200 || !strings.Contains(r.Body.String(), `method="post" action="/login/ticket"`) {
			t.Fatalf("%d %.300s", r.Code, r.Body.String())
		}
		if strings.Contains(r.Body.String(), "http-equiv") || strings.Contains(r.Body.String(), "<script") {
			t.Fatal("the page moves on by itself: a scanner would consume the ticket")
		}
		for h, want := range map[string]string{"Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
			if r.Header().Get(h) != want {
				t.Fatalf("%s = %q", h, r.Header().Get(h))
			}
		}
	}
	if s.core.Redeems != 0 {
		t.Fatalf("GET reached can-core %d times", s.core.Redeems)
	}
	if r := s.land(b, tk); r.Code != 303 {
		t.Fatalf("the ticket was spent by a GET: %d %.300s", r.Code, r.Body.String())
	}
	if r := b.get("/login/ticket?t=junk"); r.Code != 400 {
		t.Fatalf("malformed ticket: %d", r.Code)
	}
}

func TestTicketRoutesAre404WithoutCanCore(t *testing.T) {
	u := newUIScenario(t)
	c := u.client(t)
	if r := c.get("/login/ticket?t=cant_" + strings.Repeat("a", 43)); r.Code != 404 {
		t.Fatalf("GET %d", r.Code)
	}
	if r := c.do("POST", "/login/ticket", url.Values{"csrf": {"x"}}, "Sec-Fetch-Site", "same-origin"); r.Code != 404 {
		t.Fatalf("POST %d", r.Code)
	}
}

func TestTicketLoginMakesASessionOfTheModuleAndLandsOnACleanURL(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	b := s.browser(t)
	r := s.land(b, s.ticket())
	if r.Code != 303 || r.Header().Get("Location") != "/" {
		t.Fatalf("%d %q", r.Code, r.Header().Get("Location"))
	}
	if p := b.get("/"); p.Code != 200 || !strings.Contains(p.Body.String(), "Alice Example") || strings.Contains(p.Body.String(), "user:alice") {
		t.Fatalf("signed-in page: %d %.300s", p.Code, p.Body.String())
	}
	for n, v := range b.cookies {
		if strings.Contains(v, "cant_") || strings.Contains(v, "cans_") || strings.Contains(v, "canpat_") {
			t.Fatalf("cookie %s carries a credential", n)
		}
	}
	if r := b.post("/logout", nil); r.Code != 303 {
		t.Fatalf("logout %d", r.Code)
	}
	if p := b.get("/"); p.Code != 303 {
		t.Fatalf("after logout %d", p.Code)
	}
}

func TestTicketReplayAndForeignTicketsAreRefusedWithoutASession(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	tk := s.ticket()
	s.land(s.browser(t), tk)
	b := s.browser(t)
	r := s.land(b, tk)
	if r.Code != 401 || !strings.Contains(r.Body.String(), `role="alert"`) {
		t.Fatalf("replay: %d %.300s", r.Code, r.Body.String())
	}
	foreign := s.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: recruiterPerms, Module: "people"})
	b = s.browser(t)
	if r := s.land(b, foreign); r.Code != 401 {
		t.Fatalf("a ticket for another module: %d", r.Code)
	}
	if _, ok := b.cookies[sessionCookie]; ok {
		t.Fatal("a refused login set a session cookie")
	}
	r1 := s.land(s.browser(t), "cant_"+strings.Repeat("Q", 43))
	r2 := s.land(s.browser(t), tk)
	if r1.Code != r2.Code || r1.Body.Len() != r2.Body.Len() {
		t.Fatal("refusals differ")
	}
	// A person with no role of this module gets no session.
	none := s.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: []string{"jobs.read", "people.read"}})
	if r := s.land(s.browser(t), none); r.Code != 401 {
		t.Fatalf("no role: %d", r.Code)
	}
}

func TestTicketPostNeedsTheDoubleSubmitTokenAndSameSite(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	tk := s.ticket()
	b := s.browser(t)
	s.hold(b, tk)
	page := b.get("/login/ticket?t=" + tk)
	csrf := csrfRe.FindStringSubmatch(page.Body.String())[1]
	same := []string{"Sec-Fetch-Site", "same-origin"}
	if r := b.do("POST", "/login/ticket", url.Values{"csrf": {"wrong"}, "ticket": {tk}}, same...); r.Code != 403 {
		t.Fatalf("wrong csrf %d", r.Code)
	}
	if r := b.do("POST", "/login/ticket", url.Values{"ticket": {tk}}, same...); r.Code != 403 {
		t.Fatalf("missing csrf %d", r.Code)
	}
	if r := b.do("POST", "/login/ticket", url.Values{"csrf": {csrf}, "ticket": {tk}}, "Sec-Fetch-Site", "cross-site"); r.Code != 403 {
		t.Fatalf("cross-site %d", r.Code)
	}
	fresh := s.browser(t) // a forged cross-site login has no cookie
	if r := fresh.do("POST", "/login/ticket", url.Values{"csrf": {csrf}, "ticket": {tk}}, same...); r.Code != 403 {
		t.Fatalf("login CSRF %d", r.Code)
	}
	if s.core.Redeems != 0 {
		t.Fatalf("a refused post reached can-core (%d)", s.core.Redeems)
	}
	if r := s.land(s.browser(t), tk); r.Code != 303 {
		t.Fatalf("the ticket was spent by refused posts: %d", r.Code)
	}
}

func TestTicketLoginIssuesANewSessionIdWhateverTheBrowserSent(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	first := s.browser(t)
	s.land(first, s.ticket())
	old := first.cookies[sessionCookie]
	if old == "" {
		t.Fatal("no session cookie")
	}
	s.land(first, s.ticket())
	if first.cookies[sessionCookie] == old || first.cookies[sessionCookie] == "" {
		t.Fatal("the session id was reused")
	}
	b := s.browser(t)
	b.cookies[sessionCookie] = "planted"
	s.land(b, s.ticket())
	if v := b.cookies[sessionCookie]; v == "planted" || v == "" {
		t.Fatalf("a planted session id was adopted: %q", v)
	}
}

func TestReturnPathIsAnOpenRedirectProofLandingPlace(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	for ret, want := range map[string]string{
		"/jobs": "/jobs", "/": "/", "": "/", "/interviews": "/interviews",
		"//evil.example": "/", "/../api": "/", "https://evil.example": "/", "/login": "/", "/x?y=z": "/", "\\evil": "/",
	} {
		tk := s.core.NewTicket(fakecore.Grant{Subject: "user:alice", Permissions: recruiterPerms, ReturnPath: ret})
		r := s.land(s.browser(t), tk)
		if r.Code != 303 || r.Header().Get("Location") != want {
			t.Errorf("return %q -> %d %q, want %q", ret, r.Code, r.Header().Get("Location"), want)
		}
	}
}

func TestAReadOnlyRoleCannotWrite(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	b := s.browser(t)
	s.land(b, s.ticket(interviewerPerms...))
	b.get("/")
	if r := b.post("/jobs", cloneForm(ssoJobForm)); r.Code != 403 {
		t.Fatalf("an interviewer session created a job: %d", r.Code)
	}
	w := s.browser(t)
	s.land(w, s.ticket())
	w.get("/")
	if r := w.post("/jobs", cloneForm(ssoJobForm)); r.Code != 303 {
		t.Fatalf("a recruiter session could not create a job: %d %.200s", r.Code, r.Body.String())
	}
}

func TestTicketSessionFollowsCanCoreAtTheRecheckInterval(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	b := s.browser(t)
	s.land(b, s.ticket())
	b.get("/")
	before := s.core.Revalidates
	b.get("/")
	if s.core.Revalidates != before {
		t.Fatal("revalidated on every request")
	}
	s.core.SetSessionPermissions("user:alice", interviewerPerms)
	if r := b.post("/jobs", cloneForm(ssoJobForm)); r.Code != 303 {
		t.Fatalf("inside the interval the old permissions still apply: %d", r.Code)
	}
	s.clk.Add(61 * time.Second) // the interval of a session that writes
	if r := b.post("/jobs", cloneForm(ssoJobForm)); r.Code != 403 {
		t.Fatalf("after the interval write must be gone: %d", r.Code)
	}
	if s.core.Revalidates != before+1 {
		t.Fatalf("revalidations = %d", s.core.Revalidates-before)
	}
	// An interviewer-only session re-checks at the longer interval.
	s.clk.Add(61 * time.Second)
	n := s.core.Revalidates
	b.get("/")
	if s.core.Revalidates != n {
		t.Fatal("a session that neither writes nor reads candidates rechecked early")
	}
	s.core.EndSessions("user:alice")
	s.clk.Add(5*time.Minute + time.Second)
	if p := b.get("/"); p.Code != 303 {
		t.Fatalf("a disabled user kept the session: %d", p.Code)
	}
	// a role-less answer also ends it
	c := s.browser(t)
	s.land(c, s.ticket())
	s.core.SetSessionPermissions("user:alice", []string{"people.read"})
	s.clk.Add(61 * time.Second)
	if p := c.get("/"); p.Code != 303 {
		t.Fatalf("no role left, session kept: %d", p.Code)
	}
}

func TestCanCoreOutageIs503AtRedeemAndEndsSessionsAtTheNextCheck(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	b := s.browser(t)
	tk := s.ticket()
	s.hold(b, tk)
	page := b.get("/login/ticket?t=" + tk)
	form := url.Values{"csrf": {csrfRe.FindStringSubmatch(page.Body.String())[1]}, "ticket": {tk}}
	s.core.SetDown(true)
	if r := b.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin"); r.Code != 503 {
		t.Fatalf("can-core down at redeem: %d", r.Code)
	}
	s.core.SetDown(false)
	if r := s.land(b, tk); r.Code != 303 {
		t.Fatalf("the ticket must survive an outage: %d", r.Code)
	}
	s.core.SetDown(true)
	s.clk.Add(61 * time.Second)
	if p := b.get("/"); p.Code != 303 {
		t.Fatalf("an unverifiable session stayed open: %d", p.Code)
	}
}

func TestPastedTokenLoginCanBeTurnedOff(t *testing.T) {
	off := false
	s := newSSOEnv(t, nil, func(c *Config) { c.UI.TokenLogin = &off })
	s.core.AddToken(jobsPAT, fakecore.Token{Subject: "user:alice", Permissions: recruiterPerms})
	b := s.browser(t)
	page := b.get("/login")
	if page.Code != 200 || strings.Contains(page.Body.String(), `name="token"`) {
		t.Fatalf("the paste form is still offered: %d", page.Code)
	}
	csrf := csrfRe.FindStringSubmatch(page.Body.String())
	if csrf == nil {
		csrf = []string{"", "x"}
	}
	if r := b.do("POST", "/login", url.Values{"token": {jobsPAT}, "csrf": {csrf[1]}}, "Sec-Fetch-Site", "same-origin"); r.Code != 403 {
		t.Fatalf("a pasted token was considered although token_login is off: %d", r.Code)
	}
	if r := s.land(b, s.ticket()); r.Code != 303 {
		t.Fatalf("%d", r.Code)
	}
}

func TestPastedTokenLoginStillWorksByDefaultUnderCanCore(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	s.core.AddToken(jobsPAT, fakecore.Token{Subject: "user:alice", Permissions: recruiterPerms})
	b := s.browser(t)
	page := b.get("/login")
	if !strings.Contains(page.Body.String(), `name="token"`) {
		t.Fatal("no token form")
	}
	if r := b.do("POST", "/login", url.Values{"token": {jobsPAT}, "csrf": {csrfRe.FindStringSubmatch(page.Body.String())[1]}}, "Sec-Fetch-Site", "same-origin"); r.Code != 303 {
		t.Fatalf("%d", r.Code)
	}
}

func TestTicketNeverReachesLogs(t *testing.T) {
	var logs bytes.Buffer
	s := newSSOEnv(t, slog.New(slog.NewTextHandler(&logs, nil)), nil)
	b := s.browser(t)
	tk := s.ticket()
	s.land(b, tk)
	b.get("/login/ticket?t=cant_" + strings.Repeat("Z", 43))
	if strings.Contains(logs.String(), tk) || strings.Contains(logs.String(), "cant_") {
		t.Fatalf("a ticket reached the log:\n%s", logs.String())
	}
}

// Login CSRF with two browsers: the attacker launches their own account and sends the link to the
// victim. The victim's browser lacks the launch nonce, so the link does nothing there.
func TestALinkOpenedInAnotherBrowserSignsNobodyIn(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	attacker, victim := s.browser(t), s.browser(t)
	tk := s.core.NewTicket(fakecore.Grant{Subject: "user:mallory", Name: "Mallory", Permissions: recruiterPerms})
	s.hold(attacker, tk)
	r := victim.get("/login/ticket?t=" + tk)
	if r.Code != 401 || strings.Contains(r.Body.String(), "<code>mallory</code>") || ssoHiddenRe.MatchString(r.Body.String()) {
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
	s.hold(victim, s.ticket())
	if code := post(); code != 401 {
		t.Fatalf("post with another launch's nonce: %d", code)
	}
	if _, ok := victim.cookies[sessionCookie]; ok {
		t.Fatal("the victim got a session")
	}
	if s.core.Redeems != 1 { // without a nonce nothing even reaches can-core
		t.Fatalf("redeem calls: %d", s.core.Redeems)
	}
	// None of that spent the ticket: it still works where it was launched.
	s.land(attacker, tk)
	if _, ok := attacker.cookies[sessionCookie]; !ok {
		t.Fatal("the launching browser could not sign in")
	}
}

func TestAnotherAccountsSessionIsNeverReplacedSilently(t *testing.T) {
	s := newSSOEnv(t, nil, nil)
	b := s.browser(t)
	s.land(b, s.core.NewTicket(fakecore.Grant{Subject: "user:bob", Name: "Bob Existing", Permissions: recruiterPerms}))
	old := b.cookies[sessionCookie]
	if old == "" {
		t.Fatal("no first session")
	}
	tk := s.core.NewTicket(fakecore.Grant{Subject: "user:alice", Name: "Alice Incoming", Permissions: recruiterPerms})
	s.hold(b, tk)
	page := b.get("/login/ticket?t=" + tk)
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "Alice Incoming") || !strings.Contains(body, "<code>alice</code>") || !strings.Contains(body, "Bob Existing") || !strings.Contains(body, `name="replace" value="user:alice"`) {
		t.Fatalf("no warning naming both accounts: %d %.900s", page.Code, body)
	}
	form := url.Values{"csrf": {csrfRe.FindStringSubmatch(body)[1]}, "ticket": {tk}}
	before := s.core.Redeems
	r := b.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin")
	if r.Code != 409 || s.core.Redeems != before || b.cookies[sessionCookie] != old {
		t.Fatalf("replaced without confirmation: %d", r.Code)
	}
	if p := b.get("/"); p.Code != 200 {
		t.Fatalf("the existing session was dropped: %d", p.Code)
	}
	form.Set("csrf", csrfRe.FindStringSubmatch(r.Body.String())[1])
	form.Set("replace", "user:alice")
	if r := b.do("POST", "/login/ticket", form, "Sec-Fetch-Site", "same-origin"); r.Code != 303 || b.cookies[sessionCookie] == old {
		t.Fatalf("confirmed: %d %.300s", r.Code, r.Body.String())
	}
}
