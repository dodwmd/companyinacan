package jobs

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// scenario is two open jobs with one application each and one token per role.
type scenario struct {
	*env
	j1, j2, slug1, slug2                             string
	a1, a2                                           string
	c1, c2                                           string
	recruiter, recruiterJ1, hmAlice, hmBob, ivy, ian string
}

func newScenario(t *testing.T) *scenario {
	e := newEnv(t, nil)
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
	return s
}

func TestAuthentication(t *testing.T) {
	e := newEnv(t, nil)
	if r := e.do("", "GET", "/v1/me", nil); r.Code != 401 {
		t.Errorf("no token = %d", r.Code)
	}
	if r := e.do("nope", "GET", "/v1/me", nil); r.Code != 401 {
		t.Errorf("bad token = %d", r.Code)
	}
	if r := e.do("cjt_0123456789abcdef_"+strings.Repeat("x", 43), "GET", "/v1/me", nil); r.Code != 401 {
		t.Errorf("unknown stored token = %d", r.Code)
	}
	r := e.want(e.do(adminTok, "GET", "/v1/me", nil), 200)
	if m := r.m(t); m["role"] != RoleAdmin || m["subject"] != bootstrapSubject {
		t.Errorf("me = %v", m)
	}
	if r.Hdr.Get("Cache-Control") != "no-store" || r.Hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("security headers missing: %v", r.Hdr)
	}
}

// Valid credentials are never throttled, however many wrong guesses share their address.
func TestFailedAuthThrottleNeverBlocksValidCredentials(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.mint(RoleRecruiter, "rita")
	const ip = "203.0.113.9:1111"
	saw429 := false
	for i := 0; i < 12; i++ {
		if r := e.doFrom("wrong-"+strings.Repeat("a", i+1), ip, "GET", "/v1/me", nil); r.Code == 429 {
			saw429 = true
			if r.Hdr.Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
		}
	}
	if !saw429 {
		t.Error("a flood of failures from one address was never throttled")
	}
	for i := 0; i < 50; i++ {
		if r := e.doFrom(tok, ip, "GET", "/v1/me", nil); r.Code != 200 {
			t.Fatalf("valid token throttled on request %d: %d", i, r.Code)
		}
		if r := e.doFrom(adminTok, ip, "GET", "/v1/me", nil); r.Code != 200 {
			t.Fatalf("admin token throttled on request %d: %d", i, r.Code)
		}
	}
}

func TestTokenIssuanceRules(t *testing.T) {
	s := newScenario(t)
	// only an unscoped admin issues tokens
	for name, tok := range map[string]string{"recruiter": s.recruiter, "hm": s.hmAlice, "interviewer": s.ivy} {
		if r := s.do(tok, "POST", "/v1/tokens", map[string]any{"kind": "service", "subject": "x", "name": "x", "role": "interviewer"}); r.Code != 403 {
			t.Errorf("%s minted a token: %d", name, r.Code)
		}
	}
	// a scoped admin has no global powers
	scopedAdmin := s.mint(RoleAdmin, "ann", s.j1)
	for _, p := range []string{"/v1/tokens", "/v1/audit", "/v1/erasures", "/v1/eeo-report", "/v1/events/dead"} {
		if r := s.do(scopedAdmin, "GET", p, nil); r.Code != 403 {
			t.Errorf("scoped admin GET %s = %d, want 403", p, r.Code)
		}
	}
	if r := s.do(scopedAdmin, "POST", "/v1/candidates/"+s.c1+"/erasure", nil); r.Code != 403 {
		t.Errorf("scoped admin erased: %d", r.Code)
	}
	// validation
	for name, body := range map[string]map[string]any{
		"reserved subject": {"kind": "service", "subject": bootstrapSubject, "name": "x", "role": "admin"},
		"bad role":         {"kind": "service", "subject": "x", "name": "x", "role": "root"},
		"unknown job":      {"kind": "service", "subject": "x", "name": "x", "role": "recruiter", "jobs": []string{"job_0000000000000000"}},
		"bad job id":       {"kind": "service", "subject": "x", "name": "x", "role": "recruiter", "jobs": []string{"x' OR 1=1"}},
		"long personal":    {"kind": "personal", "subject": "x", "name": "x", "role": "recruiter", "expires_at": "2099-01-01T00:00:00Z"},
	} {
		if r := s.do(adminTok, "POST", "/v1/tokens", body); r.Code != 422 {
			t.Errorf("%s = %d: %s", name, r.Code, r.Body)
		}
	}
	// secrets are never listed; revoked tokens stop working at once
	list := s.want(s.do(adminTok, "GET", "/v1/tokens", nil), 200)
	if strings.Contains(list.str(), "secret") {
		t.Error("token list exposes secrets")
	}
	var id string
	for _, it := range list.m(t)["items"].([]any) {
		if it.(map[string]any)["subject"] == "ian" {
			id = it.(map[string]any)["id"].(string)
		}
	}
	s.want(s.do(adminTok, "DELETE", "/v1/tokens/"+id, nil), 204)
	if r := s.do(s.ian, "GET", "/v1/me", nil); r.Code != 401 {
		t.Errorf("revoked token = %d", r.Code)
	}
}

// Who may call what, regardless of data: 403 comes from the role matrix.
func TestRoleMatrix(t *testing.T) {
	s := newScenario(t)
	type call struct{ method, path string }
	app := "/v1/applications/" + s.a1
	forbidden := map[string][]call{
		s.ivy: {{"GET", "/v1/jobs"}, {"POST", "/v1/jobs"}, {"GET", "/v1/candidates"}, {"GET", "/v1/candidates/" + s.c1}, {"GET", "/v1/talent-pool"},
			{"POST", app + "/move"}, {"POST", app + "/reject"}, {"POST", app + "/hire"}, {"PUT", app + "/interviewers/ian"}, {"POST", "/v1/applications"},
			{"GET", "/v1/audit"}, {"GET", "/v1/tokens"}, {"GET", "/v1/eeo-report"}, {"PUT", "/v1/stages"}, {"POST", "/v1/candidates/" + s.c1 + "/erasure"}},
		s.hmAlice: {{"POST", "/v1/jobs"}, {"POST", "/v1/jobs/" + s.j1 + "/transitions"}, {"POST", app + "/hire"}, {"GET", "/v1/talent-pool"}, {"POST", "/v1/applications"},
			{"GET", "/v1/audit"}, {"GET", "/v1/tokens"}, {"GET", "/v1/eeo-report"}, {"PUT", "/v1/stages"}, {"POST", "/v1/candidates/" + s.c1 + "/erasure"},
			{"PATCH", "/v1/candidates/" + s.c1}, {"POST", "/v1/jobs/" + s.j1 + "/approval"}},
		s.recruiter: {{"GET", "/v1/audit"}, {"GET", "/v1/tokens"}, {"POST", "/v1/tokens"}, {"GET", "/v1/eeo-report"}, {"PUT", "/v1/stages"}, {"POST", "/v1/candidates/" + s.c1 + "/erasure"},
			{"PUT", "/v1/candidates/" + s.c1 + "/legal-hold"}, {"POST", "/v1/purge"}, {"POST", "/v1/retention/run"}, {"GET", "/v1/erasures"}, {"POST", "/v1/jobs/" + s.j1 + "/approval"},
			{"GET", "/v1/events/dead"}},
	}
	for tok, calls := range forbidden {
		for _, c := range calls {
			if r := s.do(tok, c.method, c.path, map[string]any{}); r.Code != 403 {
				t.Errorf("%s %s with %s... = %d, want 403: %s", c.method, c.path, tok[:8], r.Code, r.Body)
			}
		}
	}
	// the same endpoints work for the roles that own them
	for _, c := range []struct {
		tok string
		call
	}{
		{s.recruiter, call{"GET", "/v1/jobs"}}, {s.hmAlice, call{"GET", "/v1/jobs"}}, {s.recruiter, call{"GET", "/v1/candidates"}},
		{s.recruiter, call{"GET", "/v1/talent-pool"}}, {adminTok, call{"GET", "/v1/audit"}}, {adminTok, call{"GET", "/v1/eeo-report"}}, {s.ivy, call{"GET", app}},
	} {
		if r := s.do(c.tok, c.method, c.path, nil); r.Code != 200 {
			t.Errorf("%s %s = %d: %s", c.method, c.path, r.Code, r.Body)
		}
	}
}

// Anything outside a caller's scope is a 404, indistinguishable from an id that does not
// exist, on every endpoint that takes an id.
func TestIDORTable(t *testing.T) {
	s := newScenario(t)
	type ep struct {
		method, path string
		body         any
	}
	eps := func(j, a, c string) []ep {
		return []ep{
			{"GET", "/v1/jobs/" + j, nil}, {"PATCH", "/v1/jobs/" + j, map[string]any{"title": "Hijacked"}},
			{"POST", "/v1/jobs/" + j + "/transitions", map[string]any{"to": "paused"}}, {"GET", "/v1/jobs/" + j + "/syndication", nil},
			{"POST", "/v1/jobs/" + j + "/approval-requests", nil},
			{"GET", "/v1/applications/" + a, nil}, {"GET", "/v1/applications/" + a + "/resume", nil}, {"GET", "/v1/applications/" + a + "/scorecards", nil},
			{"POST", "/v1/applications/" + a + "/move", map[string]any{"stage": "screen"}}, {"POST", "/v1/applications/" + a + "/reject", map[string]any{"reason": "not_a_fit"}},
			{"PUT", "/v1/applications/" + a + "/interviewers/ian", nil}, {"DELETE", "/v1/applications/" + a + "/interviewers/ivy", nil},
			{"PUT", "/v1/applications/" + a + "/scorecards/mine", map[string]any{"rating": 3, "recommendation": "yes"}}, {"POST", "/v1/applications/" + a + "/scorecards/mine/submit", nil},
			{"GET", "/v1/candidates/" + c, nil}, {"PATCH", "/v1/candidates/" + c, map[string]any{"notes": "x"}}, {"PUT", "/v1/candidates/" + c + "/talent-pool", map[string]any{"member": true}},
		}
	}
	// 1. objects of job 2 against everyone who may not reach job 2
	for name, tok := range map[string]string{"recruiter scoped to J1": s.recruiterJ1, "hm alice": s.hmAlice, "interviewer ivy": s.ivy, "interviewer ian": s.ian} {
		for _, e := range eps(s.j2, s.a2, s.c2) {
			r := s.do(tok, e.method, e.path, e.body)
			if r.Code != 404 && r.Code != 403 {
				t.Errorf("%s %s %s = %d, want 404 (or 403 from the role): %s", name, e.method, e.path, r.Code, r.Body)
			}
			if r.Code == 200 || r.Code == 201 || r.Code == 204 {
				t.Errorf("%s reached %s %s", name, e.method, e.path)
			}
		}
	}
	// 2. objects of job 1 against people outside it: 404, not 403, wherever the role would allow the verb
	for _, tok := range []string{s.hmBob, s.ian} {
		for _, e := range eps(s.j1, s.a1, s.c1) {
			if r := s.do(tok, e.method, e.path, e.body); r.Code == 200 || r.Code == 201 || r.Code == 204 {
				t.Errorf("outsider reached %s %s: %d", e.method, e.path, r.Code)
			}
		}
	}
	// 3. a nonexistent id looks the same as an existing one out of scope
	a := s.do(s.hmBob, "GET", "/v1/applications/"+s.a1, nil)
	b := s.do(s.hmBob, "GET", "/v1/applications/app_0000000000000000", nil)
	if a.Code != 404 || b.Code != 404 || a.str() != b.str() {
		t.Errorf("out-of-scope and missing differ: %d %q vs %d %q", a.Code, a.Body, b.Code, b.Body)
	}
	// 4. legitimate access still works
	for _, c := range []struct {
		tok, path string
	}{
		{s.recruiterJ1, "/v1/applications/" + s.a1}, {s.hmAlice, "/v1/applications/" + s.a1}, {s.hmAlice, "/v1/jobs/" + s.j1}, {s.ivy, "/v1/applications/" + s.a1},
		{s.ivy, "/v1/applications/" + s.a1 + "/resume"}, {s.hmAlice, "/v1/candidates/" + s.c1}, {s.recruiterJ1, "/v1/candidates/" + s.c1}, {s.hmBob, "/v1/applications/" + s.a2},
	} {
		if r := s.do(c.tok, "GET", c.path, nil); r.Code != 200 {
			t.Errorf("legitimate GET %s = %d: %s", c.path, r.Code, r.Body)
		}
	}
}

// The token scope applies to the whole query, whatever filters are added.
func TestListScopeAndQueryInjection(t *testing.T) {
	s := newScenario(t)
	ids := func(r *resp) []string {
		var out []string
		for _, it := range r.m(t)["items"].([]any) {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		return out
	}
	for name, path := range map[string]string{
		"job_id of another job": "/v1/applications?job_id=" + s.j2,
		"candidate of another":  "/v1/applications?candidate_id=" + s.c2,
		"status injection":      "/v1/applications?status=active'+OR+'1'='1",
		"stage injection":       "/v1/applications?stage=x'+OR+1=1--",
		"or-ish stage":          "/v1/applications?stage=applied%27%20OR%20%271%27=%271",
		"limit abuse":           "/v1/applications?limit=1000000",
		"cursor injection":      "/v1/applications?cursor=1+OR+1=1",
	} {
		for who, tok := range map[string]string{"recruiterJ1": s.recruiterJ1, "hmAlice": s.hmAlice} {
			r := s.do(tok, "GET", path, nil)
			if r.Code != 200 && r.Code != 422 && r.Code != 400 {
				t.Errorf("%s/%s = %d", name, who, r.Code)
				continue
			}
			if r.Code == 200 {
				for _, id := range ids(r) {
					if id == s.a2 {
						t.Errorf("%s/%s leaked application of the other job", name, who)
					}
				}
			}
		}
	}
	// by-email lookup finds only reachable candidates
	r := s.want(s.do(s.recruiterJ1, "GET", "/v1/candidates?email=cand2@example.test", nil), 200)
	if len(r.m(t)["items"].([]any)) != 0 {
		t.Error("email lookup crossed the token scope")
	}
	r = s.want(s.do(s.recruiter, "GET", "/v1/candidates?email=cand2@example.test", nil), 200)
	if len(r.m(t)["items"].([]any)) != 1 {
		t.Error("unscoped recruiter cannot find a duplicate by email")
	}
	r = s.want(s.do(s.recruiter, "GET", "/v1/candidates?name=%25", nil), 200)
	if n := len(r.m(t)["items"].([]any)); n != 0 {
		t.Errorf("LIKE wildcard in name widened the result: %d candidates", n)
	}
	// job lists
	r = s.want(s.do(s.hmAlice, "GET", "/v1/jobs?status=open", nil), 200)
	if got := ids(r); len(got) != 1 || got[0] != s.j1 {
		t.Errorf("hm alice sees jobs %v", got)
	}
	r = s.want(s.do(s.recruiterJ1, "GET", "/v1/jobs", nil), 200)
	if got := ids(r); len(got) != 1 || got[0] != s.j1 {
		t.Errorf("scoped recruiter sees jobs %v", got)
	}
	r = s.want(s.do(s.ivy, "GET", "/v1/applications", nil), 200)
	if got := ids(r); len(got) != 1 || got[0] != s.a1 {
		t.Errorf("interviewer ivy sees applications %v", got)
	}
	r = s.want(s.do(s.ian, "GET", "/v1/applications", nil), 200)
	if got := ids(r); len(got) != 0 {
		t.Errorf("unassigned interviewer sees applications %v", got)
	}
	// a scoped token cannot create a job (it could not then reach it)
	if r := s.do(s.recruiterJ1, "POST", "/v1/jobs", jobBody("New", "x")); r.Code != 403 {
		t.Errorf("scoped recruiter created a job: %d", r.Code)
	}
	if r := s.do(s.recruiterJ1, "POST", "/v1/applications", map[string]any{"job_id": s.j2, "name": "X Y", "email": "x@example.test", "consent_recorded": true}); r.Code != 404 {
		t.Errorf("scoped recruiter added a candidate to a job outside its scope: %d", r.Code)
	}
}

func TestJobLifecycleAndValidation(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Approval.Required = true })
	body := jobBody("Platform Engineer", "alice")
	e.app.store.approver = pendingApprover{}
	m := e.want(e.do(adminTok, "POST", "/v1/jobs", body), 201).m(t)
	id := m["id"].(string)
	if m["status"] != "draft" || m["approval"] != "none" || e.do(adminTok, "GET", "/v1/jobs/"+id, nil).Hdr.Get("ETag") != `"1"` {
		t.Errorf("new job: %v", m)
	}
	// draft is invisible to the public
	if r := e.do("", "GET", "/careers/"+m["slug"].(string), nil); r.Code != 404 {
		t.Errorf("draft visible publicly: %d", r.Code)
	}
	// cannot open without approval
	if r := e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "open"}); r.Code != 409 {
		t.Errorf("open without approval = %d", r.Code)
	}
	e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/approval-requests", nil), 200)
	// the requester cannot decide their own request
	if r := e.do(adminTok, "POST", "/v1/jobs/"+id+"/approval", map[string]any{"decision": "approve"}); r.Code != 409 {
		t.Errorf("self approval = %d: %s", r.Code, r.Body)
	}
	admin2 := e.mint(RoleAdmin, "boss")
	e.want(e.do(admin2, "POST", "/v1/jobs/"+id+"/approval", map[string]any{"decision": "approve"}), 200)
	// optimistic concurrency
	r := e.do(adminTok, "GET", "/v1/jobs/"+id, nil)
	ver := r.Hdr.Get("ETag")
	req := map[string]any{"title": "Staff Platform Engineer"}
	e.want(e.doIfMatch(adminTok, "PATCH", "/v1/jobs/"+id, `"1"`, req), 412)
	e.want(e.doIfMatch(adminTok, "PATCH", "/v1/jobs/"+id, ver, req), 200)
	e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "open"}), 200)
	// illegal moves
	if r := e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "draft"}); r.Code != 409 {
		t.Errorf("open -> draft = %d", r.Code)
	}
	if r := e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "closed"}); r.Code != 422 {
		t.Errorf("close without reason = %d", r.Code)
	}
	e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "paused"}), 200)
	e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "open"}), 200)
	e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "closed", "reason": "filled"}), 200)
	if r := e.do(adminTok, "PATCH", "/v1/jobs/"+id, map[string]any{"title": "x"}); r.Code != 409 {
		t.Errorf("edit closed = %d", r.Code)
	}
	if r := e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "open"}); r.Code != 409 {
		t.Errorf("reopen closed = %d", r.Code)
	}
	// validation reports every problem at once
	bad := e.want(e.do(adminTok, "POST", "/v1/jobs", map[string]any{"title": "", "remote": "sometimes", "employment_type": "x", "headcount": 0, "sections": []any{}}), 422).m(t)
	if len(bad["errors"].([]any)) < 4 {
		t.Errorf("validation: %v", bad)
	}
	for name, mod := range map[string]func(map[string]any){
		"salary max < min": func(b map[string]any) {
			b["salary"] = map[string]any{"min": 5, "max": 1, "currency": "USD", "period": "year"}
		},
		"publish without pay":   func(b map[string]any) { delete(b, "salary"); b["publish_salary"] = true },
		"past valid_through":    func(b map[string]any) { b["valid_through"] = "2020-01-01" },
		"bad country":           func(b map[string]any) { b["location"] = map[string]any{"country": "USA"} },
		"unknown field":         func(b map[string]any) { b["status"] = "open" },
		"control char in title": func(b map[string]any) { b["title"] = "a\x00b" },
	} {
		b := jobBody("X", "alice")
		mod(b)
		if r := e.do(adminTok, "POST", "/v1/jobs", b); r.Code != 422 && r.Code != 400 {
			t.Errorf("%s = %d", name, r.Code)
		}
	}
}

func (e *env) doIfMatch(tok, method, path, etag string, body any) *resp {
	e.t.Helper()
	return e.doReq(tok, "192.0.2.10:4000", method, path, map[string]string{"If-Match": etag}, body)
}

type pendingApprover struct{}

func (pendingApprover) Request(context.Context, *Job, string) (string, string, error) {
	return "ref-1", ApprovalPending, nil
}

func TestApprovalHookCanBePending(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Approval.Required = true })
	e.app.store.approver = pendingApprover{}
	id := e.want(e.do(adminTok, "POST", "/v1/jobs", jobBody("Analyst", "alice")), 201).m(t)["id"].(string)
	m := e.want(e.do(adminTok, "POST", "/v1/jobs/"+id+"/approval-requests", nil), 200).m(t)
	if m["approval"] != "pending" {
		t.Errorf("approval = %v", m["approval"])
	}
	if r := e.do(adminTok, "POST", "/v1/jobs/"+id+"/approval-requests", nil); r.Code != 409 {
		t.Errorf("second request = %d", r.Code)
	}
	boss := e.mint(RoleAdmin, "boss")
	e.want(e.do(boss, "POST", "/v1/jobs/"+id+"/approval", map[string]any{"decision": "reject"}), 200)
	if r := e.do(adminTok, "POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "open"}); r.Code != 409 {
		t.Errorf("open after rejection = %d", r.Code)
	}
}

var _ = http.StatusOK
