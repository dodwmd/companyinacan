package jobs

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// All names, ids and values are fake. The UI is exercised through the real application handler
// (the same one that serves the API and the careers pages), signing in with stored tokens.

type uiScenario struct {
	*scenario
}

func newUIScenario(t *testing.T, mut ...func(*Config)) *uiScenario {
	t.Helper()
	e := newEnv(t, func(c *Config) {
		c.UI.Enabled = true
		c.UI.InsecureCookies = true
		for _, m := range mut {
			m(c)
		}
	})
	s := &scenario{env: e}
	s.j1, s.slug1 = e.openJob("Engineer", "alice")
	s.j2, s.slug2 = e.openJob("Designer", "bob")
	s.a1 = e.applyOK(s.slug1, "cand1@example.test", pdf)
	s.a2 = e.applyOK(s.slug2, "cand2@example.test")
	s.c1 = e.want(e.do(adminTok, "GET", "/v1/applications/"+s.a1, nil), 200).m(t)["candidate_id"].(string)
	s.c2 = e.want(e.do(adminTok, "GET", "/v1/applications/"+s.a2, nil), 200).m(t)["candidate_id"].(string)
	s.recruiter = e.mint(RoleRecruiter, "rita")
	s.recruiterJ1 = e.mint(RoleRecruiter, "rob", s.j1)
	s.hmAlice = e.mint(RoleHiringManager, "alice")
	s.hmBob = e.mint(RoleHiringManager, "bob")
	s.ivy = e.mint(RoleInterviewer, "ivy")
	s.ian = e.mint(RoleInterviewer, "ian")
	e.want(e.do(s.recruiter, "PUT", "/v1/applications/"+s.a1+"/interviewers/ivy", nil), 204)
	return &uiScenario{s}
}

type client struct {
	t       *testing.T
	e       *env
	cookies map[string]string
	csrf    string
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (u *uiScenario) client(t *testing.T) *client {
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

// login signs in with a token and returns a client holding the session.
func (u *uiScenario) login(t *testing.T, tok string) *client {
	t.Helper()
	c := u.client(t)
	m := csrfRe.FindStringSubmatch(c.get("/login").Body.String())
	if m == nil {
		t.Fatal("no csrf on the login page")
	}
	if w := c.do("POST", "/login", url.Values{"token": {tok}, "csrf": {m[1]}}, "Sec-Fetch-Site", "same-origin"); w.Code != 303 {
		t.Fatalf("login = %d %s", w.Code, w.Body.String())
	}
	c.get("/")
	if c.csrf == "" {
		t.Fatal("no session csrf token on the home page")
	}
	return c
}

func body(w *httptest.ResponseRecorder) string { return w.Body.String() }

var reqID = regexp.MustCompile(`<code>[0-9a-f]+</code>`)

// plain returns the page text with HTML escapes undone, for assertions on user data.
func plain(w *httptest.ResponseRecorder) string { return html.UnescapeString(w.Body.String()) }

func wantCode(t *testing.T, w *httptest.ResponseRecorder, code int, what string) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("%s = %d, want %d: %.300s", what, w.Code, code, w.Body.String())
	}
}

// ---- sessions ------------------------------------------------------------------------------

func TestUILoginSessionLifecycle(t *testing.T) {
	u := newUIScenario(t)
	anon := u.client(t)
	if w := anon.get("/"); w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous / = %d %q", w.Code, w.Header().Get("Location"))
	}
	login := anon.get("/login")
	wantCode(t, login, 200, "login page")
	for h, want := range map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY"} {
		if login.Header().Get(h) != want {
			t.Errorf("%s = %q", h, login.Header().Get(h))
		}
	}
	csp := login.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'none'") || strings.Contains(csp, "data:") {
		t.Errorf("CSP = %q", csp)
	}
	tok := csrfRe.FindStringSubmatch(body(login))[1]
	// an unknown token and a revoked token get the same refusal: no oracle
	revoked := u.mint(RoleRecruiter, "temp")
	u.want(u.do(adminTok, "GET", "/v1/tokens", nil), 200)
	var id string
	for _, it := range u.want(u.do(adminTok, "GET", "/v1/tokens", nil), 200).m(t)["items"].([]any) {
		if it.(map[string]any)["subject"] == "temp" {
			id = it.(map[string]any)["id"].(string)
		}
	}
	u.want(u.do(adminTok, "DELETE", "/v1/tokens/"+id, nil), 204)
	var msgs []string
	for _, bad := range []string{"cjt_nope", revoked} {
		w := anon.do("POST", "/login", url.Values{"token": {bad}, "csrf": {tok}}, "Sec-Fetch-Site", "same-origin")
		wantCode(t, w, 401, "bad token login")
		msgs = append(msgs, regexp.MustCompile(`role="alert">([^<]*)<`).FindStringSubmatch(body(w))[1])
	}
	if msgs[0] != msgs[1] {
		t.Errorf("unknown and revoked tokens are told apart: %q vs %q", msgs[0], msgs[1])
	}
	// login without the double-submit token is refused, a cross-site post is refused
	if w := anon.do("POST", "/login", url.Values{"token": {u.recruiter}, "csrf": {"wrong"}}, "Sec-Fetch-Site", "same-origin"); w.Code != 403 {
		t.Errorf("login with a bad csrf = %d", w.Code)
	}
	if w := anon.do("POST", "/login", url.Values{"token": {u.recruiter}, "csrf": {tok}}, "Sec-Fetch-Site", "cross-site"); w.Code != 403 {
		t.Errorf("cross-site login = %d", w.Code)
	}
	// success: HttpOnly SameSite=Strict cookie, never the token
	w := anon.do("POST", "/login", url.Values{"token": {u.recruiter}, "csrf": {tok}}, "Sec-Fetch-Site", "same-origin")
	wantCode(t, w, 303, "login")
	var sess *http.Cookie
	for _, ck := range w.Result().Cookies() {
		if ck.Name == sessionCookie {
			sess = ck
		}
	}
	if sess == nil || !sess.HttpOnly || sess.SameSite != http.SameSiteStrictMode || strings.Contains(sess.Value, u.recruiter) {
		t.Fatalf("session cookie = %+v", sess)
	}
	wantCode(t, anon.get("/"), 200, "home with a session")
	// a stolen copy of the cookie dies with logout
	copy := &client{t: t, e: u.env, cookies: map[string]string{sessionCookie: sess.Value}}
	wantCode(t, anon.post("/logout", nil), 303, "logout")
	if w := copy.get("/"); w.Code != 303 {
		t.Errorf("a logged-out session id still works: %d", w.Code)
	}
}

func TestUISessionTimeoutsAndRecheck(t *testing.T) {
	u := newUIScenario(t)
	c := u.login(t, u.recruiter)
	u.clk.Add(31 * time.Minute) // idle timeout (30 minutes)
	if w := c.get("/"); w.Code != 303 {
		t.Errorf("idle session = %d", w.Code)
	}
	// revocation is noticed within the re-check interval, at most a minute for admin roles
	tok := u.mint(RoleAdmin, "ada-admin")
	c = u.login(t, tok)
	var id string
	for _, it := range u.want(u.do(adminTok, "GET", "/v1/tokens", nil), 200).m(t)["items"].([]any) {
		if it.(map[string]any)["subject"] == "ada-admin" {
			id = it.(map[string]any)["id"].(string)
		}
	}
	u.want(u.do(adminTok, "DELETE", "/v1/tokens/"+id, nil), 204)
	wantCode(t, c.get("/"), 200, "before the recheck interval")
	u.clk.Add(61 * time.Second)
	if w := c.get("/"); w.Code != 303 {
		t.Errorf("a revoked admin token still has a session a minute later: %d", w.Code)
	}
	// absolute timeout
	c = u.login(t, u.recruiter)
	for i := 0; i < 20; i++ {
		u.clk.Add(25 * time.Minute)
		c.get("/")
	}
	if w := c.get("/"); w.Code != 303 {
		t.Errorf("session older than 8 hours = %d", w.Code)
	}
}

func TestUIPerSubjectSessionCap(t *testing.T) {
	u := newUIScenario(t)
	first := u.login(t, u.recruiter)
	for i := 0; i < 6; i++ {
		u.clk.Add(time.Second)
		u.login(t, u.recruiter)
	}
	if w := first.get("/"); w.Code != 303 {
		t.Errorf("the oldest of more than five sessions still works: %d", w.Code)
	}
}

func TestUICSRFOnEveryPost(t *testing.T) {
	u := newUIScenario(t)
	c := u.login(t, u.recruiter)
	for _, p := range []string{"/jobs", "/jobs/" + u.j1 + "/transition", "/applications/" + u.a1 + "/move", "/applications/" + u.a1 + "/reject", "/applications/" + u.a1 + "/hire",
		"/applications/" + u.a1 + "/interviewers", "/applications/" + u.a1 + "/reveal", "/applications/" + u.a1 + "/scorecard", "/logout"} {
		if w := c.do("POST", p, url.Values{"csrf": {"nope"}}, "Sec-Fetch-Site", "same-origin"); w.Code != 403 {
			t.Errorf("POST %s with a wrong csrf = %d", p, w.Code)
		}
		if w := c.do("POST", p, url.Values{"csrf": {c.csrf}}, "Sec-Fetch-Site", "cross-site"); w.Code != 403 {
			t.Errorf("cross-site POST %s = %d", p, w.Code)
		}
	}
	wantCode(t, c.get("/"), 200, "session survives refused posts")
}

// ---- scope and roles -----------------------------------------------------------------------

func TestUIScopeHiringManagerAndInterviewer(t *testing.T) {
	u := newUIScenario(t)
	alice := u.login(t, u.hmAlice)
	wantCode(t, alice.get("/jobs/"+u.j1), 200, "own job")
	other := alice.get("/jobs/" + u.j2)
	wantCode(t, other, 404, "another manager's job")
	missing := alice.get("/jobs/job_0000000000000000")
	if reqID.ReplaceAllString(body(other), "") != reqID.ReplaceAllString(body(missing), "") || other.Code != missing.Code {
		t.Error("a job that exists but is out of scope is told apart from one that does not exist")
	}
	if strings.Contains(body(alice.get("/jobs")), "Designer") {
		t.Error("the job list shows another manager's requisition")
	}
	wantCode(t, alice.get("/applications/"+u.a2), 404, "application of another manager's job")
	wantCode(t, alice.get("/talent"), 403, "talent pool for a hiring manager")
	wantCode(t, alice.get("/reports/eeo"), 403, "eeo report for a hiring manager")
	wantCode(t, alice.get("/jobs/new"), 403, "job form for a hiring manager")

	ivy := u.login(t, u.ivy)
	wantCode(t, ivy.get("/applications/"+u.a1), 200, "assigned application")
	notYours := ivy.get("/applications/" + u.a2)
	wantCode(t, notYours, 404, "unassigned application")
	wantCode(t, ivy.get("/jobs"), 403, "jobs list for an interviewer")
	wantCode(t, ivy.get("/jobs/"+u.j1), 403, "job page for an interviewer")
	wantCode(t, ivy.get("/talent"), 403, "talent pool for an interviewer")
	ian := u.login(t, u.ian)
	wantCode(t, ian.get("/applications/"+u.a1), 404, "an interviewer who was never assigned")

	// a token scoped to one job keeps that scope in the UI
	rob := u.login(t, u.recruiterJ1)
	wantCode(t, rob.get("/jobs/"+u.j1), 200, "scoped recruiter, own job")
	wantCode(t, rob.get("/jobs/"+u.j2), 404, "scoped recruiter, other job")
	wantCode(t, rob.get("/reports/eeo"), 403, "eeo report for a recruiter")
}

func TestUIRoleHomePages(t *testing.T) {
	u := newUIScenario(t)
	rec := body(u.login(t, u.recruiter).get("/"))
	if !strings.Contains(rec, "Engineer") || !strings.Contains(rec, "Designer") || !strings.Contains(rec, "Applied") {
		t.Errorf("recruiter dashboard lacks jobs or funnel: %.400s", rec)
	}
	hm := body(u.login(t, u.hmAlice).get("/"))
	if !strings.Contains(hm, "Engineer") || strings.Contains(hm, "Designer") {
		t.Errorf("hiring manager dashboard is not limited to own jobs")
	}
	iv := body(u.login(t, u.ivy).get("/"))
	if !strings.Contains(iv, "/applications/"+u.a1) {
		t.Errorf("interviewer home lacks the assigned application")
	}
}

// ---- candidate data ------------------------------------------------------------------------

func TestUISensitiveFieldsOnlyOnAuditedReveal(t *testing.T) {
	u := newUIScenario(t)
	c := u.login(t, u.recruiter)
	page := body(c.get("/applications/" + u.a1))
	for _, leak := range []string{"cand1@example.test", "+1 555 0100", "I like building things", "RESUME-MARKER"} {
		if strings.Contains(page, leak) {
			t.Errorf("the application page shows %q without a reveal", leak)
		}
	}
	if !strings.Contains(page, "Ada Example") || !strings.Contains(page, "ada.pdf") {
		t.Error("the application page lacks the candidate name or resume metadata")
	}
	before := u.auditCount(t, "candidate:"+u.c1)
	rev := c.post("/applications/"+u.a1+"/reveal", nil)
	wantCode(t, rev, 200, "reveal")
	for _, want := range []string{"cand1@example.test", "+1 555 0100", "I like building things"} {
		if !strings.Contains(plain(rev), want) {
			t.Errorf("the reveal lacks %q", want)
		}
	}
	if u.auditCount(t, "candidate:"+u.c1) <= before {
		t.Error("the reveal was not audited")
	}
	if rev.Header().Get("Cache-Control") != "no-store" {
		t.Error("revealed page is cacheable")
	}
	// a hiring manager reveals only what the role may read
	hm := u.login(t, u.hmAlice)
	hr := plain(hm.post("/applications/"+u.a1+"/reveal", nil))
	if !strings.Contains(hr, "cand1@example.test") || strings.Contains(hr, "+1 555 0100") {
		t.Errorf("hiring manager reveal exposes the wrong fields: %.500s", hr)
	}
	// an interviewer cannot reveal at all
	iv := u.login(t, u.ivy)
	if w := iv.post("/applications/"+u.a1+"/reveal", nil); w.Code != 403 {
		t.Errorf("interviewer reveal = %d", w.Code)
	}
	// reveals are rate limited per subject
	limited := false
	for i := 0; i < 15; i++ {
		if c.post("/applications/"+u.a1+"/reveal", nil).Code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Error("reveals are not rate limited")
	}
}

func (u *uiScenario) auditCount(t *testing.T, resource string) int {
	t.Helper()
	n := 0
	var cur int64
	for {
		es, next, err := u.app.Store().ListAudit(context.Background(), cur, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Resource == resource {
				n++
			}
		}
		if next == 0 {
			return n
		}
		cur = next
	}
}

func TestUIDiversityDataNeverOnCandidatePages(t *testing.T) {
	u := newUIScenario(t, func(c *Config) { c.EEO.MinCell = 1 })
	f := goodApplication("eeo-person@example.test")
	f["eeo_gender"], f["eeo_ethnicity"] = "female", "asian"
	if r := u.apply(u.slug1, f); r.Code != 303 {
		t.Fatalf("apply = %d", r.Code)
	}
	c := u.login(t, u.recruiter)
	pages := body(c.get("/")) + body(c.get("/jobs/"+u.j1)) + body(c.get("/talent")) + body(c.get("/applications/"+u.a1)) + body(c.post("/applications/"+u.a1+"/reveal", nil)) + body(c.get("/candidates/"+u.c1))
	for _, bad := range []string{"female", "asian", "Ethnicity", "ethnicity", "Gender", "diversity"} {
		if strings.Contains(pages, bad) {
			t.Errorf("a candidate-facing staff page mentions %q", bad)
		}
	}
	// the report is admin only, closed months only
	wantCode(t, c.get("/reports/eeo"), 403, "recruiter eeo report")
	admin := u.login(t, adminTok)
	rep := admin.get("/reports/eeo")
	wantCode(t, rep, 200, "admin eeo report")
	if strings.Contains(strings.ToLower(body(rep)), "female") {
		t.Error("the open month is in the report")
	}
	u.clk.Add(40 * 24 * time.Hour) // the month closes
	admin = u.login(t, adminTok)
	if !strings.Contains(strings.ToLower(body(admin.get("/reports/eeo"))), "female") {
		t.Errorf("a closed month is missing from the report: %.600s", body(admin.get("/reports/eeo")))
	}
}

func TestUISuppressionIsTheServices(t *testing.T) {
	u := newUIScenario(t) // min cell 5
	f := goodApplication("eeo2@example.test")
	f["eeo_gender"] = "female"
	u.apply(u.slug1, f)
	u.clk.Add(40 * 24 * time.Hour)
	admin := u.login(t, adminTok)
	rep := strings.ToLower(body(admin.get("/reports/eeo")))
	if strings.Contains(rep, ">1<") || strings.Contains(rep, "female") {
		t.Errorf("a cell below the minimum is shown: %.600s", rep)
	}
}

func TestUIResumeDownloadIsAnAttachment(t *testing.T) {
	u := newUIScenario(t)
	c := u.login(t, u.recruiter)
	w := c.get("/applications/" + u.a1 + "/resume")
	wantCode(t, w, 200, "resume download")
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", w.Header())
	}
	if !strings.Contains(body(w), "RESUME-MARKER-123") {
		t.Error("the file was not served")
	}
	page := body(c.get("/applications/" + u.a1))
	if strings.Contains(page, "<iframe") || strings.Contains(page, "<object") || strings.Contains(page, "<embed") {
		t.Error("the resume is rendered inline")
	}
	if strings.Contains(page, "resume_key") || regexp.MustCompile(`[0-9a-f]{32}`).MatchString(page) {
		t.Error("the page exposes the storage key")
	}
	if !strings.Contains(page, "Virus scan") {
		t.Error("the scan status is not shown")
	}
	// anyone outside the scope gets the same 404
	wantCode(t, u.login(t, u.ian).get("/applications/"+u.a1+"/resume"), 404, "resume for an unassigned interviewer")
	wantCode(t, u.login(t, u.hmBob).get("/applications/"+u.a1+"/resume"), 404, "resume for another manager")
	wantCode(t, u.login(t, u.ivy).get("/applications/"+u.a1+"/resume"), 200, "resume for the assigned interviewer")
	// an infected file is never offered
	inf := u.applyOK(u.slug1, "bad@example.test", upload{field: "resume", name: "x.pdf", ctype: "application/pdf", data: []byte("%PDF-1.7 fine")})
	if _, err := u.app.Store().db.Exec(`UPDATE applications SET resume_scan = 'infected' WHERE id = ?`, inf); err != nil {
		t.Fatal(err)
	}
	wantCode(t, c.get("/applications/"+inf+"/resume"), 404, "infected resume")
	if strings.Contains(body(c.get("/applications/"+inf)), "/applications/"+inf+"/resume") {
		t.Error("a download link is offered for an infected file")
	}
}

func TestUICandidateNamesAreEscaped(t *testing.T) {
	u := newUIScenario(t)
	r := u.want(u.do(u.recruiter, "POST", "/v1/applications", map[string]any{"job_id": u.j1, "name": `<script>alert(1)</script> "x" & <b>`, "email": "xss@example.test", "consent_recorded": true}), 201).m(t)
	id := r["id"].(string)
	c := u.login(t, u.recruiter)
	for _, p := range []string{"/applications/" + id, "/jobs/" + u.j1, "/", "/candidates/" + r["candidate_id"].(string), "/talent?q=%3Cscript"} {
		if b := body(c.get(p)); strings.Contains(b, "<script>alert") || strings.Contains(b, "<b>") {
			t.Errorf("%s renders a candidate name unescaped", p)
		}
	}
	if !strings.Contains(body(c.get("/applications/"+id)), "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the hostile name is not shown escaped")
	}
}

// ---- pipeline ------------------------------------------------------------------------------

func TestUIMoveNeedsAReason(t *testing.T) {
	u := newUIScenario(t)
	c := u.login(t, u.recruiter)
	c.get("/applications/" + u.a1)
	if w := c.post("/applications/"+u.a1+"/move", url.Values{"to": {"screen"}, "note": {""}}); w.Code != 400 {
		t.Errorf("move without a reason = %d", w.Code)
	}
	if w := c.post("/applications/"+u.a1+"/move", url.Values{"to": {"screen"}, "note": {strings.Repeat("a", MoveNoteMax+1)}}); w.Code != 400 {
		t.Errorf("move with an over-long reason = %d", w.Code)
	}
	if w := c.post("/applications/"+u.a1+"/move", url.Values{"to": {"nope"}, "note": {"ok"}}); w.Code != 400 {
		t.Errorf("move to an unknown stage = %d", w.Code)
	}
	if w := c.post("/applications/"+u.a1+"/move", url.Values{"to": {"hired"}, "note": {"ok"}}); w.Code != 409 {
		t.Errorf("move into hired = %d", w.Code)
	}
	w := c.post("/applications/"+u.a1+"/move", url.Values{"to": {"screen"}, "note": {"good call, MOVE-REASON-1"}})
	wantCode(t, w, 303, "move")
	page := body(c.get("/applications/" + u.a1))
	if !strings.Contains(page, "MOVE-REASON-1") || !strings.Contains(page, "Screen") {
		t.Errorf("the history lacks the move and its reason: %.600s", page)
	}
	// the hiring manager moves too, but does not read recruiter-only reasons
	hm := u.login(t, u.hmAlice)
	if strings.Contains(body(hm.get("/applications/"+u.a1)), "MOVE-REASON-1") {
		t.Error("the hiring manager sees a reason the service seals from them")
	}
	wantCode(t, hm.post("/applications/"+u.a1+"/move", url.Values{"to": {"interview"}, "note": {"panel next"}}), 303, "hiring manager move")
	// an interviewer may not move
	wantCode(t, u.login(t, u.ivy).post("/applications/"+u.a1+"/move", url.Values{"to": {"interview"}, "note": {"x"}}), 403, "interviewer move")
}

func TestUIRejectAndHireFlow(t *testing.T) {
	u := newUIScenario(t)
	c := u.login(t, u.recruiter)
	c.get("/applications/" + u.a2)
	if w := c.post("/applications/"+u.a2+"/reject", url.Values{"reason": {"because"}}); w.Code != 400 {
		t.Errorf("free-text reject reason = %d", w.Code)
	}
	if w := c.post("/applications/"+u.a2+"/reject", url.Values{"reason": {"compensation"}, "note": {strings.Repeat("n", 2001)}}); w.Code != 400 {
		t.Errorf("over-long reject note = %d", w.Code)
	}
	wantCode(t, c.post("/applications/"+u.a2+"/reject", url.Values{"reason": {"compensation"}, "note": {"too high"}}), 303, "reject")
	if !strings.Contains(body(c.get("/applications/"+u.a2)), "Rejected") {
		t.Error("the page does not show the rejection")
	}
	if w := c.post("/applications/"+u.a2+"/move", url.Values{"to": {"screen"}, "note": {"again"}}); w.Code != 409 {
		t.Errorf("moving a rejected application = %d", w.Code)
	}
	// hire: not from the screen stage, a hiring manager may not, a recruiter may from offer
	if w := c.post("/applications/"+u.a1+"/hire", nil); w.Code != 409 {
		t.Errorf("hire outside an offer stage = %d", w.Code)
	}
	wantCode(t, c.post("/applications/"+u.a1+"/move", url.Values{"to": {"offer"}, "note": {"make an offer"}}), 303, "move to offer")
	hm := u.login(t, u.hmAlice)
	if w := hm.post("/applications/"+u.a1+"/hire", nil); w.Code != 403 {
		t.Errorf("hiring manager hire = %d", w.Code)
	}
	if strings.Contains(body(hm.get("/applications/"+u.a1)), "/applications/"+u.a1+"/hire") {
		t.Error("a hiring manager is offered the hire action")
	}
	wantCode(t, c.post("/applications/"+u.a1+"/hire", nil), 303, "hire")
	var n int
	if err := u.app.Store().db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE type = ?`, EventCandidateHired).Scan(&n); err != nil || n != 1 {
		t.Errorf("candidate.hired events = %d, %v", n, err)
	}
}

// ---- blind feedback ------------------------------------------------------------------------

func TestUIBlindFeedback(t *testing.T) {
	u := newUIScenario(t)
	u.want(u.do(u.recruiter, "PUT", "/v1/applications/"+u.a1+"/interviewers/ian", nil), 204)
	u.want(u.do(u.ian, "PUT", "/v1/applications/"+u.a1+"/scorecards/mine", map[string]any{"rating": 4, "recommendation": "strong_yes", "comments": "IAN-SECRET-COMMENT"}), 200)
	u.want(u.do(u.ian, "POST", "/v1/applications/"+u.a1+"/scorecards/mine/submit", nil), 200)

	ivy := u.login(t, u.ivy)
	page := body(ivy.get("/applications/" + u.a1))
	if strings.Contains(page, "IAN-SECRET-COMMENT") || regexp.MustCompile(`\bian\b`).MatchString(page) {
		t.Errorf("a colleague's feedback or identity reaches an interviewer who has not submitted: %.800s", page)
	}
	if !regexp.MustCompile(`(?i)after you submit`).MatchString(page) {
		t.Error("the page does not say feedback is released after submitting")
	}
	// an interviewer with only a draft is still blind
	wantCode(t, ivy.post("/applications/"+u.a1+"/scorecard", url.Values{"rating": {"3"}, "recommendation": {"yes"}, "comments": {"IVY-DRAFT"}, "action": {"save"}}), 303, "save draft")
	page = body(ivy.get("/applications/" + u.a1))
	if strings.Contains(page, "IAN-SECRET-COMMENT") || !strings.Contains(page, "IVY-DRAFT") {
		t.Errorf("draft state: %.800s", page)
	}
	// bad input
	if w := ivy.post("/applications/"+u.a1+"/scorecard", url.Values{"rating": {"9"}, "recommendation": {"yes"}, "action": {"save"}}); w.Code != 400 {
		t.Errorf("rating 9 = %d", w.Code)
	}
	wantCode(t, ivy.post("/applications/"+u.a1+"/scorecard", url.Values{"rating": {"3"}, "recommendation": {"yes"}, "comments": {"IVY-FINAL"}, "action": {"submit"}}), 303, "submit")
	page = body(ivy.get("/applications/" + u.a1))
	if !strings.Contains(page, "IAN-SECRET-COMMENT") || !strings.Contains(page, "IVY-FINAL") {
		t.Errorf("feedback is not released after submitting: %.800s", page)
	}
	if w := ivy.post("/applications/"+u.a1+"/scorecard", url.Values{"rating": {"1"}, "recommendation": {"no"}, "action": {"save"}}); w.Code != 409 {
		t.Errorf("editing a submitted scorecard = %d", w.Code)
	}
}

func TestUIRemovedInterviewerStaysBlindAndCannotSubmit(t *testing.T) {
	u := newUIScenario(t)
	u.want(u.do(u.recruiter, "PUT", "/v1/applications/"+u.a1+"/interviewers/alice", nil), 204)
	u.want(u.do(u.ivy, "PUT", "/v1/applications/"+u.a1+"/scorecards/mine", map[string]any{"rating": 4, "recommendation": "yes", "comments": "IVY-SECRET-COMMENT"}), 200)
	u.want(u.do(u.ivy, "POST", "/v1/applications/"+u.a1+"/scorecards/mine/submit", nil), 200)
	alice := u.login(t, u.hmAlice)
	u.want(u.do(u.recruiter, "DELETE", "/v1/applications/"+u.a1+"/interviewers/alice", nil), 204)
	page := body(alice.get("/applications/" + u.a1))
	if strings.Contains(page, "IVY-SECRET-COMMENT") {
		t.Error("a removed interviewer reads colleagues' feedback")
	}
	if w := alice.post("/applications/"+u.a1+"/scorecard", url.Values{"rating": {"3"}, "recommendation": {"yes"}, "action": {"save"}}); w.Code == 303 {
		t.Error("a removed interviewer can write a scorecard")
	}
}

func TestUISelfUnassignIsRefusedByTheService(t *testing.T) {
	u := newUIScenario(t)
	alice := u.login(t, u.hmAlice)
	wantCode(t, alice.post("/applications/"+u.a1+"/interviewers", url.Values{"subject": {"alice"}}), 303, "assign self")
	w := alice.post("/applications/"+u.a1+"/interviewers/remove", url.Values{"subject": {"alice"}})
	wantCode(t, w, 409, "self-unassign before submitting")
	if !strings.Contains(body(w), "cannot remove your own assignment") {
		t.Errorf("the refusal does not say why: %.400s", body(w))
	}
	if w := alice.post("/applications/"+u.a1+"/interviewers", url.Values{"subject": {"bad subject!"}}); w.Code != 400 {
		t.Errorf("assign with an invalid subject = %d", w.Code)
	}
	wantCode(t, alice.post("/applications/"+u.a1+"/interviewers/remove", url.Values{"subject": {"ivy"}}), 303, "remove somebody else")
}

func TestUIMyInterviews(t *testing.T) {
	u := newUIScenario(t)
	ivy := u.login(t, u.ivy)
	page := body(ivy.get("/interviews"))
	if !strings.Contains(page, "/applications/"+u.a1) || strings.Contains(page, u.a2) {
		t.Errorf("my interviews = %.500s", page)
	}
	wantCode(t, u.login(t, u.ian).get("/interviews"), 200, "interviews of someone with none")
	if strings.Contains(body(u.login(t, u.ian).get("/interviews")), u.a1) {
		t.Error("an unassigned interviewer sees an application on their interviews page")
	}
}

// ---- requisitions --------------------------------------------------------------------------

func TestUIJobLifecycle(t *testing.T) {
	u := newUIScenario(t, func(c *Config) { c.Approval.Required = false })
	c := u.login(t, u.recruiter)
	form := url.Values{"title": {"Staff Engineer"}, "department": {"Engineering"}, "city": {"Austin"}, "region": {"TX"}, "country": {"US"}, "remote": {"hybrid"},
		"employment_type": {"full_time"}, "salary_min": {"100000"}, "salary_max": {"140000"}, "salary_currency": {"USD"}, "salary_period": {"year"},
		"hiring_manager": {"alice"}, "headcount": {"1"}, "section_heading_0": {"About"}, "section_body_0": {"Do good work."}}
	c.get("/jobs/new")
	bad := url.Values{}
	for k, v := range form {
		bad[k] = v
	}
	bad.Set("title", "")
	if w := c.post("/jobs", bad); w.Code != 400 || !strings.Contains(body(w), "title") {
		t.Errorf("job without a title = %d", w.Code)
	}
	w := c.post("/jobs", form)
	wantCode(t, w, 303, "create job")
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/jobs/job_") {
		t.Fatalf("Location = %q", loc)
	}
	id := strings.TrimPrefix(strings.SplitN(loc, "?", 2)[0], "/jobs/")
	page := body(c.get(loc))
	if !strings.Contains(page, "Staff Engineer") || !strings.Contains(page, "Draft") {
		t.Errorf("job page: %.500s", page)
	}
	if strings.Contains(page, "/careers/staff-engineer") || !strings.Contains(page, "not public") {
		t.Error("a draft job offers a live careers link")
	}
	// edit: needs the current version, salary publish toggle
	edit := body(c.get("/jobs/" + id + "/edit"))
	ver := regexp.MustCompile(`name="version" value="(\d+)"`).FindStringSubmatch(edit)
	if ver == nil {
		t.Fatalf("no version field in the edit form: %.400s", edit)
	}
	form.Set("version", ver[1])
	form.Set("title", "Staff Engineer II")
	form.Set("publish_salary", "on")
	wantCode(t, c.post("/jobs/"+id, form), 303, "edit job")
	if w := c.post("/jobs/"+id, form); w.Code != 409 { // same (now stale) version
		t.Errorf("a stale edit = %d", w.Code)
	}
	if !strings.Contains(body(c.get("/jobs/"+id)), "Staff Engineer II") {
		t.Error("the edit was not saved")
	}
	// lifecycle draft -> open -> paused -> open -> closed (reason required)
	wantCode(t, c.post("/jobs/"+id+"/transition", url.Values{"to": {"open"}}), 303, "open")
	slug := u.want(u.do(u.recruiter, "GET", "/v1/jobs/"+id, nil), 200).m(t)["slug"].(string)
	if !strings.Contains(body(c.get("/jobs/"+id)), "https://example.test/careers/"+slug) {
		t.Error("an open job has no careers preview link")
	}
	wantCode(t, c.post("/jobs/"+id+"/transition", url.Values{"to": {"paused"}}), 303, "pause")
	wantCode(t, c.post("/jobs/"+id+"/transition", url.Values{"to": {"open"}}), 303, "reopen")
	if w := c.post("/jobs/"+id+"/transition", url.Values{"to": {"closed"}}); w.Code != 400 {
		t.Errorf("closing without a reason = %d", w.Code)
	}
	wantCode(t, c.post("/jobs/"+id+"/transition", url.Values{"to": {"closed"}, "reason": {"filled"}}), 303, "close")
	if w := c.post("/jobs/"+id+"/transition", url.Values{"to": {"open"}}); w.Code != 409 {
		t.Errorf("reopening a closed job = %d", w.Code)
	}
	// hiring managers cannot write
	hm := u.login(t, u.hmAlice)
	wantCode(t, hm.post("/jobs", form), 403, "hiring manager creates a job")
	wantCode(t, hm.post("/jobs/"+u.j1+"/transition", url.Values{"to": {"paused"}}), 403, "hiring manager transitions a job")
}

func TestUIApprovalStatusAndRequest(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.UI.Enabled, c.UI.InsecureCookies, c.Approval.Required = true, true, true })
	u := &uiScenario{&scenario{env: e}}
	u.recruiter = e.mint(RoleRecruiter, "rita")
	c := u.login(t, u.recruiter)
	id := u.want(u.do(u.recruiter, "POST", "/v1/jobs", jobBody("Needs approval", "alice")), 201).m(t)["id"].(string)
	page := body(c.get("/jobs/" + id))
	if !strings.Contains(page, "Approval") || !strings.Contains(page, "/jobs/"+id+"/approval") {
		t.Errorf("no approval status or request on a draft: %.500s", page)
	}
	if w := c.post("/jobs/"+id+"/transition", url.Values{"to": {"open"}}); w.Code != 409 {
		t.Errorf("opening before approval = %d", w.Code)
	}
}

func TestUIPipelineBoardIsBounded(t *testing.T) {
	u := newUIScenario(t)
	for i := 0; i < boardLimit+5; i++ {
		u.want(u.do(u.recruiter, "POST", "/v1/applications", map[string]any{"job_id": u.j1, "name": "Bulk Person", "email": "bulk" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@example.test", "consent_recorded": true}), 201)
	}
	page := body(u.login(t, u.recruiter).get("/jobs/" + u.j1))
	if n := strings.Count(page, "Bulk Person"); n > boardLimit {
		t.Errorf("the board draws %d cards in a stage, limit is %d", n, boardLimit)
	}
	if !strings.Contains(page, "more") {
		t.Error("the board does not say that cards are left out")
	}
}

func TestUITalentPool(t *testing.T) {
	u := newUIScenario(t)
	c := u.login(t, u.recruiter)
	u.want(u.do(u.recruiter, "PUT", "/v1/candidates/"+u.c1+"/talent-pool", map[string]any{"member": true}), 200)
	page := body(c.get("/talent"))
	if !strings.Contains(page, "Ada Example") || strings.Contains(page, "cand1@example.test") {
		t.Errorf("talent pool page: %.500s", page)
	}
	wantCode(t, c.get("/talent?tag="+url.QueryEscape("Bad Tag!")), 400, "invalid tag")
	wantCode(t, c.get("/candidates/"+u.c1), 200, "candidate page")
	wantCode(t, u.login(t, u.hmBob).get("/candidates/"+u.c1), 404, "candidate of another manager's job")
	wantCode(t, u.login(t, u.ivy).get("/candidates/"+u.c1), 403, "candidate page for an interviewer")
	c.get("/candidates/" + u.c1)
	wantCode(t, c.post("/candidates/"+u.c1+"/tags", url.Values{"tags": {"golang, remote"}}), 303, "tags")
	if !strings.Contains(body(c.get("/talent?tag=golang")), "Ada Example") {
		t.Error("tag search does not find the candidate")
	}
	wantCode(t, c.post("/candidates/"+u.c1+"/talent-pool", url.Values{"member": {"off"}}), 303, "leave talent pool")
}

func TestUICareersPreviewAndSettings(t *testing.T) {
	u := newUIScenario(t)
	c := u.login(t, u.recruiter)
	if !strings.Contains(body(c.get("/jobs/"+u.j1)), "https://example.test/careers/"+u.slug1) {
		t.Error("an open job has no careers link")
	}
	s := body(c.get("/settings"))
	for _, st := range []string{"Applied", "Screen", "Interview", "Offer", "Hired"} {
		if !strings.Contains(s, st) {
			t.Errorf("settings lacks stage %q", st)
		}
	}
	if strings.Contains(s, "CAN_JOBS") || strings.Contains(s, "keyring") {
		t.Error("settings exposes secrets or paths")
	}
}

// ---- bounded work, throttling, misc --------------------------------------------------------

func TestUIDeadlineGives503Page(t *testing.T) {
	u := newUIScenario(t)
	c := u.login(t, u.recruiter)
	u.app.ui.Timeout = time.Nanosecond
	w := c.get("/jobs")
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Errorf("an expired deadline = %d", w.Code)
	}
}

func TestUIValidCredentialsNeverThrottled(t *testing.T) {
	u := newUIScenario(t)
	anon := u.client(t)
	tok := csrfRe.FindStringSubmatch(body(anon.get("/login")))[1]
	saw429 := false
	for i := 0; i < 30; i++ {
		if w := anon.do("POST", "/login", url.Values{"token": {"wrong-" + strings.Repeat("x", i+1)}, "csrf": {tok}}, "Sec-Fetch-Site", "same-origin"); w.Code == 429 {
			saw429 = true
		}
	}
	if !saw429 {
		t.Error("failed logins are never throttled")
	}
	if w := anon.do("POST", "/login", url.Values{"token": {u.recruiter}, "csrf": {tok}}, "Sec-Fetch-Site", "same-origin"); w.Code != 303 {
		t.Errorf("a valid token was throttled: %d", w.Code)
	}
}

func TestUIDisabledByDefault(t *testing.T) {
	e := newEnv(t, nil)
	r := httptest.NewRecorder()
	e.h.ServeHTTP(r, httptest.NewRequest("GET", "/login", nil))
	if r.Code != 404 {
		t.Errorf("/login with the UI off = %d", r.Code)
	}
	// the API and the careers pages are unaffected when it is on
	u := newUIScenario(t)
	u.want(u.do(adminTok, "GET", "/v1/me", nil), 200)
	if r := u.do("", "GET", "/careers", nil); r.Code != 200 {
		t.Errorf("/careers with the UI on = %d", r.Code)
	}
	if r := u.do("", "GET", "/v1/nope", nil); strings.Contains(r.str(), "<html") {
		t.Errorf("unknown /v1 path answers the UI: %s", r.str())
	}
}
