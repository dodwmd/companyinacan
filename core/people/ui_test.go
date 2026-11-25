package people

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// All names, ids and values are fake. The UI is exercised through its real handler over the same
// Service the JSON API uses.

type uiEnv struct {
	*env
	st  *StaticTokens
	ui  *UI
	h   http.Handler
	now time.Time
}

func newUIEnv(t *testing.T) *uiEnv {
	t.Helper()
	e := newEnv(t)
	u := &uiEnv{env: e, st: &StaticTokens{}, now: *e.clock}
	u.ui = &UI{Svc: e.s, Auth: MultiAuth{u.st}, Secure: false, Now: func() time.Time { return u.now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	u.h = u.ui.Handler()
	return u
}

// token registers a bearer token for the actor and returns it.
func (u *uiEnv) token(a Actor) string {
	tok := "pat-" + a.Subject + "-0123456789abcdef"
	u.st.Add(tok, Principal{Subject: a.Subject, Roles: a.Roles})
	return tok
}

type client struct {
	t       *testing.T
	u       *uiEnv
	cookies map[string]string
	csrf    string
	landed  *httptest.ResponseRecorder // the answer to the last ticket POST (ssoEnv.land)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (u *uiEnv) client(t *testing.T) *client {
	return &client{t: t, u: u, cookies: map[string]string{}}
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
	c.u.h.ServeHTTP(w, r)
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

func (u *uiEnv) login(t *testing.T, a Actor) *client {
	t.Helper()
	c := u.client(t)
	w := c.get("/login")
	m := csrfRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("no csrf on login page: %s", w.Body.String())
	}
	w = c.do("POST", "/login", url.Values{"token": {u.token(a)}, "csrf": {m[1]}}, "Sec-Fetch-Site", "same-origin")
	if w.Code != 303 {
		t.Fatalf("login = %d %s", w.Code, w.Body.String())
	}
	c.get("/") // picks up the session csrf token
	if c.csrf == "" {
		t.Fatal("no session csrf token on home page")
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

func TestUILoginSessionLifecycle(t *testing.T) {
	u := newUIEnv(t)
	_, ann := u.person("Ann", "", RoleEmployee)
	tok := u.token(ann)

	// anonymous requests go to /login
	c := u.client(t)
	if w := c.get("/"); w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous home = %d %s", w.Code, w.Header().Get("Location"))
	}
	// wrong token: generic 401, no session cookie
	w := c.get("/login")
	csrf := csrfRe.FindStringSubmatch(w.Body.String())[1]
	w = c.do("POST", "/login", url.Values{"token": {"nope"}, "csrf": {csrf}}, "Sec-Fetch-Site", "same-origin")
	if w.Code != 401 || len(c.cookies) > 1 {
		t.Fatalf("bad login = %d cookies=%v", w.Code, c.cookies)
	}
	// login without the csrf double-submit is refused
	if w := c.do("POST", "/login", url.Values{"token": {tok}}, "Sec-Fetch-Site", "same-origin"); w.Code != 403 {
		t.Fatalf("login without csrf = %d", w.Code)
	}
	// a planted session id is never adopted (fixation)
	c.cookies["can_people_session"] = "attacker-chosen"
	w = c.do("POST", "/login", url.Values{"token": {tok}, "csrf": {csrf}}, "Sec-Fetch-Site", "same-origin")
	if w.Code != 303 {
		t.Fatalf("login = %d", w.Code)
	}
	var sc *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == "can_people_session" {
			sc = ck
		}
	}
	if sc == nil || sc.Value == "attacker-chosen" || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode || sc.Path != "/" {
		t.Fatalf("session cookie = %+v", sc)
	}
	if w := c.get("/"); w.Code != 200 {
		t.Fatalf("home after login = %d", w.Code)
	}
	if w := c.get("/"); w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") != "" || w.Header().Get("Last-Modified") != "" {
		t.Fatalf("headers = %v", w.Header())
	}
	csp := c.get("/").Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'none'") || !strings.Contains(csp, "form-action 'self'") {
		t.Fatalf("csp = %q", csp)
	}
	// logout kills the server-side session even if the cookie is replayed
	old := c.cookies["can_people_session"]
	if w := c.post("/logout", nil); w.Code != 303 {
		t.Fatalf("logout = %d", w.Code)
	}
	c.cookies["can_people_session"] = old
	if w := c.get("/"); w.Code != 303 {
		t.Fatalf("replayed session = %d", w.Code)
	}
}

func TestUISessionTimeouts(t *testing.T) {
	u := newUIEnv(t)
	_, ann := u.person("Ann", "", RoleEmployee)
	u.ui.IdleTimeout, u.ui.AbsoluteTimeout = 30*time.Minute, 2*time.Hour
	c := u.login(t, ann)
	u.now = u.now.Add(20 * time.Minute)
	if w := c.get("/"); w.Code != 200 {
		t.Fatalf("within idle = %d", w.Code)
	}
	u.now = u.now.Add(31 * time.Minute)
	if w := c.get("/"); w.Code != 303 {
		t.Fatalf("after idle = %d", w.Code)
	}
	c = u.login(t, ann)
	for i := 0; i < 5; i++ { // keeps itself alive, but not past the absolute limit
		u.now = u.now.Add(25 * time.Minute)
		c.get("/")
	}
	if w := c.get("/"); w.Code != 303 {
		t.Fatalf("after absolute = %d", w.Code)
	}
}

func TestUICSRFAndFetchMetadata(t *testing.T) {
	u := newUIEnv(t)
	_, ann := u.person("Ann", "", RoleEmployee)
	c := u.login(t, ann)
	if w := c.do("POST", "/me", url.Values{"preferred_name": {"X"}}, "Sec-Fetch-Site", "same-origin"); w.Code != 403 {
		t.Fatalf("no csrf = %d", w.Code)
	}
	if w := c.do("POST", "/me", url.Values{"preferred_name": {"X"}, "csrf": {"wrong"}}, "Sec-Fetch-Site", "same-origin"); w.Code != 403 {
		t.Fatalf("wrong csrf = %d", w.Code)
	}
	if w := c.do("POST", "/me", url.Values{"preferred_name": {"X"}, "csrf": {c.csrf}}, "Sec-Fetch-Site", "cross-site"); w.Code != 403 {
		t.Fatalf("cross-site = %d", w.Code)
	}
	if w := c.post("/me", url.Values{"preferred_name": {"Annie"}, "work_phone": {"+61 400 000 000"}}); w.Code != 303 {
		t.Fatalf("good post = %d %s", w.Code, w.Body.String())
	}
	mustHave(t, c.get("/me"), "Annie", "400 000 000")
}

func TestUIHostileTextIsEscaped(t *testing.T) {
	u := newUIEnv(t)
	boss, bossA := u.person("Boss", "", RoleEmployee, RoleManager)
	hostile := `<script>alert(1)</script>"><img src=x onerror=alert(2)>`
	p, err := u.s.CreatePerson(u.ctx, u.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: hostile, FamilyName: "Z", SSOSubject: "sso-evil", ManagerID: boss.ID, StartDate: "2025-01-06"})
	u.must(err)
	c := u.login(t, bossA)
	for _, path := range []string{"/", "/people?q=Z", "/people/" + p.ID, "/team", "/org"} {
		w := c.get(path)
		if w.Code != 200 {
			t.Fatalf("%s = %d", path, w.Code)
		}
		mustNot(t, w, "<script>alert", "<img src=x")
	}
	mustHave(t, c.get("/people/"+p.ID), "&lt;script&gt;")
}

func TestUISensitiveScreenAuditedPerField(t *testing.T) {
	u := newUIEnv(t)
	boss, bossA := u.person("Boss", "", RoleEmployee, RoleManager)
	ann, annA := u.person("Ann", boss.ID, RoleEmployee)
	_, benA := u.person("Ben", boss.ID, RoleEmployee)
	u.must(u.s.PutSensitive(u.ctx, u.hr, ann.ID, map[string]json.RawMessage{FieldDOB: raw("1990-05-17"), FieldNationalID: raw("QQ123456C"), FieldAddress: raw("1 Example Street"), FieldPhone: raw("+61 411 222 333"), FieldSalary: raw(Salary{Amount: 9000000, Currency: "USD", Period: "year"})}))

	secrets := []string{"1990-05-17", "QQ123456C", "1 Example Street", "411 222 333", "90000"}
	// no page other than the sensitive screen ever carries them, for anyone
	for _, a := range []Actor{annA, bossA, benA} {
		c := u.login(t, a)
		for _, path := range []string{"/", "/me", "/people/" + ann.ID, "/people?q=Ann", "/org", "/team"} {
			mustNot(t, c.get(path), secrets...)
		}
	}
	// self: the GET page is a notice only (no data, no audit); the POST reveals and audits
	c := u.login(t, annA)
	w := c.get("/me/sensitive")
	mustNot(t, w, secrets...)
	if u.aud.find("read", "person:"+ann.ID, "ok") != nil {
		t.Fatal("GET must not read sensitive data")
	}
	w = c.post("/me/sensitive", nil)
	mustHave(t, w, "1990-05-17", "QQ123456C", "1 Example Street", "411 222 333", "90000")
	if ev := u.aud.find("read", "person:"+ann.ID, "ok"); ev == nil || len(ev.Fields) != 5 {
		t.Fatalf("audit = %+v", ev)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive page must be no-store")
	}
	// the manager (direct or not) is refused on every route
	cb := u.login(t, bossA)
	path := "/people/" + ann.ID + "/sensitive"
	if w := cb.get(path); w.Code != 403 && w.Code != 404 {
		t.Fatalf("manager GET %s = %d", path, w.Code)
	}
	w = cb.post(path, nil)
	if w.Code != 403 && w.Code != 404 {
		t.Fatalf("manager POST %s = %d", path, w.Code)
	}
	mustNot(t, w, secrets...)
	// a colleague likewise
	cc := u.login(t, benA)
	w = cc.post("/people/"+ann.ID+"/sensitive", nil)
	mustNot(t, w, secrets...)
	if w.Code != 403 && w.Code != 404 {
		t.Fatalf("colleague = %d", w.Code)
	}
}

func TestUILeaveMaskedConsistently(t *testing.T) {
	u := newUIEnv(t)
	boss, bossA := u.person("Boss", "", RoleEmployee, RoleManager)
	ann, annA := u.person("Ann", boss.ID, RoleEmployee)
	_, benA := u.person("Ben", boss.ID, RoleEmployee)
	_, err := u.s.ChangeStatus(u.ctx, u.hr, ann.ID, StLeave, "2026-03-10", "")
	u.must(err)
	cb := u.login(t, benA)
	for _, path := range []string{"/people?q=Ann", "/people/" + ann.ID, "/org/" + boss.ID} {
		w := cb.get(path)
		mustHave(t, w, "Ann")
		mustNot(t, w, "On leave", "status-leave")
	}
	// the person and the manager chain see the truth
	mustHave(t, u.login(t, annA).get("/people/"+ann.ID), "On leave")
	mustHave(t, u.login(t, bossA).get("/people/"+ann.ID), "On leave")
}

func TestUIInvisibleRecordsAre404(t *testing.T) {
	u := newUIEnv(t)
	boss, bossA := u.person("Boss", "", RoleEmployee, RoleManager)
	ann, annA := u.person("Ann", boss.ID, RoleEmployee)
	_, benA := u.person("Ben", boss.ID, RoleEmployee)
	ser, err := u.s.CreateSeries(u.ctx, bossA, ann.ID, "weekly")
	u.must(err)
	m, err := u.s.ScheduleMeeting(u.ctx, bossA, ser.ID, u.now.Add(24*time.Hour))
	u.must(err)
	g, err := u.s.CreateGoal(u.ctx, annA, GoalInput{Scope: "person", Title: "Private goal", Visibility: "private"})
	u.must(err)
	cb := u.login(t, benA)
	var bodies []string
	for _, path := range []string{"/oneonones/" + ser.ID, "/meetings/" + m.ID, "/goals/" + g.ID, "/people/pers_" + strings.Repeat("0", 24), "/people/bogus"} {
		w := cb.get(path)
		if w.Code != 404 {
			t.Errorf("%s = %d", path, w.Code)
		}
		mustNot(t, w, "Private goal")
		bodies = append(bodies, regexp.MustCompile(`value="[^"]*"|Request id[^<]*<code>[^<]*`).ReplaceAllString(w.Body.String(), ""))
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Error("404 pages differ, existence is observable")
		}
	}
	for _, path := range []string{"/meetings/" + m.ID + "/notes", "/meetings/" + m.ID + "/agenda"} {
		if w := cb.post(path, url.Values{"text": {"x"}}); w.Code != 404 {
			t.Errorf("POST %s = %d", path, w.Code)
		}
	}
}

func TestUIOneOnOnePrivateNotesOnlyForManager(t *testing.T) {
	u := newUIEnv(t)
	boss, bossA := u.person("Boss", "", RoleEmployee, RoleManager)
	ann, annA := u.person("Ann", boss.ID, RoleEmployee)
	cb := u.login(t, bossA)
	w := cb.post("/oneonones", url.Values{"report_id": {ann.ID}, "cadence": {"weekly"}})
	if w.Code != 303 {
		t.Fatalf("create series = %d %s", w.Code, w.Body.String())
	}
	sloc := w.Header().Get("Location")
	cb.get(sloc)
	w = cb.post(sloc+"/meetings", url.Values{"scheduled_at": {u.now.Add(48 * time.Hour).Format("2006-01-02T15:04")}})
	if w.Code != 303 {
		t.Fatalf("schedule = %d %s", w.Code, w.Body.String())
	}
	mloc := w.Header().Get("Location")
	cb.get(mloc)
	cb.post(mloc+"/private-notes", url.Values{"text": {"MANAGER-EYES-ONLY"}})
	cb.post(mloc+"/notes", url.Values{"text": {"shared SHARED-NOTE"}})
	ca := u.login(t, annA)
	ca.get(mloc)
	if w := ca.post(mloc+"/agenda", url.Values{"text": {"report topic AGENDA-ONE"}}); w.Code != 303 {
		t.Fatalf("agenda = %d", w.Code)
	}
	wm := cb.get(mloc)
	mustHave(t, wm, "MANAGER-EYES-ONLY", "SHARED-NOTE", "AGENDA-ONE")
	wa := ca.get(mloc)
	mustHave(t, wa, "SHARED-NOTE", "AGENDA-ONE")
	mustNot(t, wa, "MANAGER-EYES-ONLY", "private-notes")
	if w := ca.post(mloc+"/private-notes", url.Values{"text": {"x"}}); w.Code != 403 && w.Code != 404 {
		t.Fatalf("report writing private notes = %d", w.Code)
	}
	// action item
	if w := cb.post(mloc+"/actions", url.Values{"text": {"Send the doc"}, "assignee_id": {ann.ID}, "due_date": {"2026-04-01"}}); w.Code != 303 {
		t.Fatalf("action = %d", w.Code)
	}
	mustHave(t, ca.get(mloc), "Send the doc")
	mustHave(t, cb.get("/team"), "Ann")
}

func TestUIGoalsFlow(t *testing.T) {
	u := newUIEnv(t)
	boss, bossA := u.person("Boss", "", RoleEmployee, RoleManager)
	_, annA := u.person("Ann", boss.ID, RoleEmployee)
	c := u.login(t, annA)
	w := c.post("/goals", url.Values{"scope": {"person"}, "title": {"Ship it"}, "period": {"2026-Q1"}, "visibility": {"manager"}, "description": {"d"}})
	if w.Code != 303 {
		t.Fatalf("create goal = %d %s", w.Code, w.Body.String())
	}
	loc, _, _ := strings.Cut(w.Header().Get("Location"), "?")
	c.get(loc)
	if w := c.post(loc+"/key-results", url.Values{"title": {"Pages"}, "unit": {"pages"}, "start_value": {"0"}, "target_value": {"10"}}); w.Code != 303 {
		t.Fatalf("add kr = %d %s", w.Code, w.Body.String())
	}
	wg := c.get(loc)
	mustHave(t, wg, "Pages", "Ship it")
	id := regexp.MustCompile(`/key-results/([a-z0-9_]+)/progress`).FindStringSubmatch(wg.Body.String())
	if id == nil {
		t.Fatalf("no progress form: %s", wg.Body.String())
	}
	if w := c.post("/key-results/"+id[1]+"/progress", url.Values{"value": {"5"}, "comment": {"half way"}, "goal": {loc}}); w.Code != 303 {
		t.Fatalf("progress = %d %s", w.Code, w.Body.String())
	}
	mustHave(t, c.get(loc), "50%", "half way")
	if w := c.post("/key-results/"+id[1]+"/progress", url.Values{"value": {"NaN"}}); w.Code != 400 {
		t.Fatalf("NaN progress = %d", w.Code)
	}
	// manager-visible goal is readable by the manager, not by an unrelated colleague
	mustHave(t, u.login(t, bossA).get(loc), "Ship it")
	_, benA := u.person("Ben", "", RoleEmployee)
	if w := u.login(t, benA).get(loc); w.Code != 404 {
		t.Fatalf("colleague on manager-visible goal = %d", w.Code)
	}
}

type fakeApprovals []ApprovalItem

func (f fakeApprovals) Waiting(context.Context, string) ([]ApprovalItem, error) { return f, nil }

func TestUIHomeShowsTasksAndApprovalsOnlyWhenWired(t *testing.T) {
	u := newUIEnv(t)
	boss, bossA := u.person("Boss", "", RoleEmployee, RoleManager)
	ann, annA := u.person("Ann", boss.ID, RoleEmployee)
	tpl, err := u.s.CreateTemplate(u.ctx, u.hr, TemplateInput{Name: "Onboarding", Kind: "onboarding", Items: []TemplateItem{{Title: "Read the handbook", AssigneeRole: "self", DueOffsetDays: 3}}})
	u.must(err)
	_, err = u.s.CreateChecklist(u.ctx, u.hr, ann.ID, tpl.ID, "2026-03-10")
	u.must(err)
	c := u.login(t, annA)
	w := c.get("/")
	mustHave(t, w, "Read the handbook")
	mustNot(t, w, "Approvals waiting")
	task := regexp.MustCompile(`/tasks/([a-z0-9_]+)/complete`).FindStringSubmatch(w.Body.String())
	if task == nil {
		t.Fatalf("no complete form: %s", w.Body.String())
	}
	if w := c.post("/tasks/"+task[1]+"/complete", nil); w.Code != 303 {
		t.Fatalf("complete = %d", w.Code)
	}
	mustNot(t, c.get("/"), "Read the handbook")
	u.ui.Approvals = fakeApprovals{{Title: "Laptop request", URL: "http://admin.localhost/approvals/7"}}
	u.ui.NotificationsURL = "http://admin.localhost/notifications"
	w = u.login(t, bossA).get("/")
	mustHave(t, w, "Approvals waiting", "Laptop request", "http://admin.localhost/notifications")
}

func TestUILeaverLosesEveryRoute(t *testing.T) {
	u := newUIEnv(t)
	boss, bossA := u.person("Boss", "", RoleEmployee, RoleManager)
	ann, _ := u.person("Ann", boss.ID, RoleEmployee)
	c := u.login(t, bossA)
	mustHave(t, c.get("/team"), "Ann")
	_, err := u.s.ChangeStatus(u.ctx, u.hr, boss.ID, StOffboarding, "2026-03-10", "")
	u.must(err)
	_, err = u.s.ChangeStatus(u.ctx, u.hr, boss.ID, StFormer, "2026-03-10", "")
	u.must(err)
	for _, path := range []string{"/", "/team", "/people/" + ann.ID, "/goals", "/oneonones", "/me"} {
		w := c.get(path)
		if w.Code == 200 {
			t.Errorf("former manager still gets %s", path)
		}
		mustNot(t, w, "Ann Example")
	}
}

func TestUIOrgChartBoundedAndAcyclic(t *testing.T) {
	u := newUIEnv(t)
	a, aA := u.person("Aa", "", RoleEmployee, RoleManager)
	b, _ := u.person("Bb", a.ID, RoleEmployee, RoleManager)
	cc, _ := u.person("Cc", b.ID, RoleEmployee)
	// corrupt the store into a cycle behind the service's back
	_, err := u.s.db.Exec(`UPDATE people SET manager_id = ? WHERE id = ?`, cc.ID, a.ID)
	u.must(err)
	c := u.login(t, aA)
	for _, path := range []string{"/org", "/org/" + cc.ID, "/team"} {
		done := make(chan int, 1)
		go func() { done <- c.get(path).Code }()
		select {
		case code := <-done:
			if code != 200 {
				t.Errorf("%s = %d", path, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not terminate", path)
		}
	}
}

func TestUIFailedLoginsLimitedValidNeverThrottled(t *testing.T) {
	u := newUIEnv(t)
	_, ann := u.person("Ann", "", RoleEmployee)
	tok := u.token(ann)
	c := u.client(t)
	csrf := csrfRe.FindStringSubmatch(c.get("/login").Body.String())[1]
	limited := false
	for i := 0; i < 60; i++ {
		w := c.do("POST", "/login", url.Values{"token": {"bad"}, "csrf": {csrf}}, "Sec-Fetch-Site", "same-origin")
		if w.Code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Fatal("failed logins are not rate limited")
	}
	w := c.do("POST", "/login", url.Values{"token": {tok}, "csrf": {csrf}}, "Sec-Fetch-Site", "same-origin")
	if w.Code != 303 {
		t.Fatalf("valid login after flood = %d", w.Code)
	}
}

func TestUIStaticAndNoInlineScript(t *testing.T) {
	u := newUIEnv(t)
	c := u.client(t)
	w := c.get("/static/people.css")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("css = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := c.get("/static/..%2fui.go"); w.Code != 404 {
		t.Fatalf("traversal = %d", w.Code)
	}
	w = c.get("/login")
	mustNot(t, w, "<script", " onclick=", " onerror=", "style=\"")
	if w := c.get("/nope"); w.Code != 404 {
		t.Fatalf("404 = %d", w.Code)
	}
}

func TestUISourceHasNoSecondAuthorizationPath(t *testing.T) {
	// The UI must never decide access itself: it calls the Service, which asks the policy layer.
	for _, f := range []string{"ui.go", "ui_pages.go", "ui_session.go", "ui_pages_e14b.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`(^|[^A-Za-z])Decide\(`, `FieldAccess\(`, `\.has\(`, `\.hrAdmin\(`, `s\.db\.`, `Svc\.db`} {
			if regexp.MustCompile(bad).Match(b) {
				t.Errorf("%s contains %q: the UI must go through Service methods only", f, bad)
			}
		}
	}
}
