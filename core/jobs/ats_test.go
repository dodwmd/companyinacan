package jobs

import (
	"strings"
	"testing"
	"time"
)

func TestPipelineMoveRejectHire(t *testing.T) {
	s := newScenario(t)
	app := "/v1/applications/" + s.a1
	// hiring manager of the job may move, but may not hire
	m := s.want(s.do(s.hmAlice, "POST", app+"/move", map[string]any{"stage": "screen"}), 200).m(t)
	if m["stage"] != "screen" {
		t.Errorf("stage = %v", m["stage"])
	}
	if r := s.do(s.hmAlice, "POST", app+"/move", map[string]any{"stage": "nope"}); r.Code != 422 {
		t.Errorf("unknown stage = %d", r.Code)
	}
	if r := s.do(s.recruiter, "POST", app+"/move", map[string]any{"stage": "hired"}); r.Code != 409 {
		t.Errorf("moving straight into hired = %d", r.Code)
	}
	if r := s.do(s.recruiter, "POST", app+"/hire", nil); r.Code != 409 {
		t.Errorf("hire from screen = %d (needs an offer stage)", r.Code)
	}
	s.want(s.do(s.recruiter, "POST", app+"/move", map[string]any{"stage": "offer"}), 200)
	h := s.want(s.do(s.recruiter, "POST", app+"/hire", nil), 200).m(t)
	if h["status"] != "hired" || h["stage"] != "hired" {
		t.Errorf("hired = %v", h)
	}
	if r := s.do(s.recruiter, "POST", app+"/reject", map[string]any{"reason": "not_a_fit"}); r.Code != 409 {
		t.Errorf("reject after hire = %d", r.Code)
	}
	if hist := h["history"].([]any); len(hist) != 4 {
		t.Errorf("history entries = %d", len(hist))
	}
	// rejection: reason vocabulary, sealed note, hidden from the hiring manager
	if r := s.do(s.recruiter, "POST", "/v1/applications/"+s.a2+"/reject", map[string]any{"reason": "because"}); r.Code != 422 {
		t.Errorf("free-text reason = %d", r.Code)
	}
	rj := s.want(s.do(s.recruiter, "POST", "/v1/applications/"+s.a2+"/reject", map[string]any{"reason": "compensation", "note": "wanted 200k, secret detail"}), 200).m(t)
	if rj["status"] != "rejected" || rj["reject_reason"] != "compensation" || rj["reject_note"] != "wanted 200k, secret detail" {
		t.Errorf("reject: %v", rj)
	}
	hm := s.want(s.do(s.hmBob, "GET", "/v1/applications/"+s.a2, nil), 200)
	if strings.Contains(hm.str(), "secret detail") || hm.m(t)["reject_note"] != nil {
		t.Error("the rejection note reached the hiring manager")
	}
	if r := s.do(s.recruiter, "POST", "/v1/applications/"+s.a2+"/move", map[string]any{"stage": "screen"}); r.Code != 409 {
		t.Errorf("move after rejection = %d", r.Code)
	}
}

func TestConfigurableStages(t *testing.T) {
	s := newScenario(t)
	stages := []map[string]string{{"key": "new", "name": "New", "kind": "initial"}, {"key": "phone", "name": "Phone screen", "kind": "active"},
		{"key": "onsite", "name": "Onsite", "kind": "active"}, {"key": "offer", "name": "Offer", "kind": "offer"}, {"key": "done", "name": "Hired", "kind": "hired"}}
	// stages in use cannot disappear
	if r := s.do(adminTok, "PUT", "/v1/stages", map[string]any{"stages": stages}); r.Code != 409 {
		t.Errorf("dropping stages in use = %d: %s", r.Code, r.Body)
	}
	keep := append([]map[string]string{{"key": "applied", "name": "Applied", "kind": "initial"}}, stages[1:]...)
	s.want(s.do(adminTok, "PUT", "/v1/stages", map[string]any{"stages": keep}), 200)
	s.want(s.do(s.recruiter, "POST", "/v1/applications/"+s.a1+"/move", map[string]any{"stage": "onsite"}), 200)
	if r := s.do(s.recruiter, "POST", "/v1/applications/"+s.a1+"/move", map[string]any{"stage": "interview"}); r.Code != 422 {
		t.Errorf("removed stage = %d", r.Code)
	}
	for name, bad := range map[string][]map[string]string{
		"no initial":      {{"key": "a", "name": "A", "kind": "active"}, {"key": "b", "name": "B", "kind": "active"}, {"key": "c", "name": "C", "kind": "hired"}},
		"duplicate key":   {{"key": "a", "name": "A", "kind": "initial"}, {"key": "a", "name": "A2", "kind": "active"}, {"key": "c", "name": "C", "kind": "hired"}},
		"bad key":         {{"key": "A b", "name": "A", "kind": "initial"}, {"key": "b", "name": "B", "kind": "active"}, {"key": "c", "name": "C", "kind": "hired"}},
		"hired in middle": {{"key": "a", "name": "A", "kind": "initial"}, {"key": "b", "name": "B", "kind": "hired"}, {"key": "c", "name": "C", "kind": "hired"}},
	} {
		if r := s.do(adminTok, "PUT", "/v1/stages", map[string]any{"stages": bad}); r.Code != 422 {
			t.Errorf("%s = %d", name, r.Code)
		}
	}
	// every role can read the pipeline
	if r := s.do(s.ivy, "GET", "/v1/stages", nil); r.Code != 200 {
		t.Errorf("interviewer GET stages = %d", r.Code)
	}
}

func TestScorecardVisibilityRules(t *testing.T) {
	s := newScenario(t)
	s.want(s.do(s.recruiter, "PUT", "/v1/applications/"+s.a1+"/interviewers/ian", nil), 204)
	app := "/v1/applications/" + s.a1
	card := func(tok string, rating int, comment string) *resp {
		return s.do(tok, "PUT", app+"/scorecards/mine", map[string]any{"rating": rating, "recommendation": "yes", "comments": comment})
	}
	items := func(r *resp) (n, hidden int, body string) {
		m := r.m(t)
		h := 0
		if m["others_hidden"] == true {
			h = 1
		}
		if _, ok := m["hidden_until_submitted"]; ok {
			t.Error("the count of hidden scorecards leaks how many colleagues have finished")
		}
		return len(m["items"].([]any)), h, r.str()
	}
	// ivy saves a draft; nobody else sees it
	s.want(card(s.ivy, 4, "IVY-PRIVATE-NOTE"), 200)
	if n, _, body := items(s.want(s.do(s.ian, "GET", app+"/scorecards", nil), 200)); n != 0 || strings.Contains(body, "IVY-PRIVATE") {
		t.Errorf("a peer sees a draft: %s", body)
	}
	if n, _, body := items(s.want(s.do(s.hmAlice, "GET", app+"/scorecards", nil), 200)); n != 0 || strings.Contains(body, "IVY-PRIVATE") {
		t.Errorf("the hiring manager sees a draft: %s", body)
	}
	if n, _, _ := items(s.want(s.do(s.ivy, "GET", app+"/scorecards", nil), 200)); n != 1 {
		t.Error("the author cannot see their own draft")
	}
	s.want(s.do(s.ivy, "POST", app+"/scorecards/mine/submit", nil), 200)
	// ian has not submitted: ivy's submitted card is hidden from him, but he is told it exists
	n, hidden, body := items(s.want(s.do(s.ian, "GET", app+"/scorecards", nil), 200))
	if n != 0 || hidden != 1 || strings.Contains(body, "IVY-PRIVATE") {
		t.Errorf("blind review broken: items=%d hidden=%d %s", n, hidden, body)
	}
	// the hiring manager (not an interviewer here) sees submitted feedback
	if n, _, body := items(s.want(s.do(s.hmAlice, "GET", app+"/scorecards", nil), 200)); n != 1 || !strings.Contains(body, "IVY-PRIVATE-NOTE") {
		t.Errorf("hiring manager cannot read submitted feedback: %s", body)
	}
	// a recruiter who is also assigned stays blind until submitting
	s.want(s.do(s.recruiter, "PUT", app+"/interviewers/rita", nil), 204)
	if n, hidden, _ := items(s.want(s.do(s.recruiter, "GET", app+"/scorecards", nil), 200)); n != 0 || hidden != 1 {
		t.Errorf("an assigned recruiter saw feedback before submitting: %d %d", n, hidden)
	}
	// ian submits; now he sees ivy's
	s.want(card(s.ian, 2, "ian says"), 200)
	s.want(s.do(s.ian, "POST", app+"/scorecards/mine/submit", nil), 200)
	if n, hidden, body := items(s.want(s.do(s.ian, "GET", app+"/scorecards", nil), 200)); n != 2 || hidden != 0 || !strings.Contains(body, "IVY-PRIVATE-NOTE") {
		t.Errorf("after submitting ian sees %d (hidden %d)", n, hidden)
	}
	// submitted is final
	if r := card(s.ivy, 1, "changed my mind"); r.Code != 409 {
		t.Errorf("editing a submitted scorecard = %d", r.Code)
	}
	if r := s.do(s.ivy, "POST", app+"/scorecards/mine/submit", nil); r.Code != 409 {
		t.Errorf("double submit = %d", r.Code)
	}
	// validation, and only assigned people can write
	if r := card(s.hmAlice, 3, "x"); r.Code != 404 {
		t.Errorf("an unassigned hiring manager wrote a scorecard: %d", r.Code)
	}
	for name, body := range map[string]map[string]any{"rating 0": {"rating": 0, "recommendation": "yes"}, "rating 5": {"rating": 5, "recommendation": "yes"}, "bad rec": {"rating": 3, "recommendation": "maybe"}} {
		if r := s.do(s.ian, "PUT", app+"/scorecards/mine", body); r.Code != 422 {
			t.Errorf("%s = %d", name, r.Code)
		}
	}
	// comments are sealed at rest
	if strings.Contains(string(s.rawDBBytes()), "IVY-PRIVATE-NOTE") {
		t.Error("scorecard comments are stored in the clear")
	}
}

// Interviewers see a name, the stage and the resume file; no contact data, no EEO, no other
// applications, in every representation.
func TestInterviewerSeesNoPersonalDataBeyondName(t *testing.T) {
	s := newScenario(t)
	s.want(s.do(s.recruiter, "PATCH", "/v1/candidates/"+s.c1, map[string]any{"notes": "NOTE-XYZ", "address": "1 Secret St", "salary_expectation": "999k"}), 200)
	for _, path := range []string{"/v1/applications/" + s.a1, "/v1/applications", "/v1/applications/" + s.a1 + "/scorecards"} {
		r := s.want(s.do(s.ivy, "GET", path, nil), 200)
		for _, secret := range []string{"cand1@example.test", "+1 555 0100", "I like building things", "NOTE-XYZ", "1 Secret St", "999k", "gender", "ethnicity"} {
			if strings.Contains(r.str(), secret) {
				t.Errorf("interviewer GET %s exposes %q", path, secret)
			}
		}
	}
	m := s.want(s.do(s.ivy, "GET", "/v1/applications/"+s.a1, nil), 200).m(t)
	if m["candidate_name"] != "Ada Example" || m["resume"] == nil {
		t.Errorf("interviewer lost the data they need: %v", m)
	}
	// hiring manager: email and cover letter yes, phone/address/salary/notes no
	h := s.want(s.do(s.hmAlice, "GET", "/v1/candidates/"+s.c1, nil), 200).m(t)
	if h["email"] != "cand1@example.test" || h["phone"] != nil || h["address"] != nil || h["notes"] != nil || h["salary_expectation"] != nil {
		t.Errorf("hiring manager candidate view: %v", h)
	}
	if rf, _ := h["restricted_fields"].([]any); len(rf) != 4 {
		t.Errorf("restricted_fields = %v", h["restricted_fields"])
	}
	r := s.want(s.do(s.recruiter, "GET", "/v1/candidates/"+s.c1, nil), 200).m(t)
	if r["phone"] != "+1 555 0100" || r["notes"] != "NOTE-XYZ" || r["salary_expectation"] != "999k" {
		t.Errorf("recruiter candidate view: %v", r)
	}
}

func TestEventsCarryNoPersonalData(t *testing.T) {
	s := newScenario(t)
	s.want(s.do(s.recruiter, "POST", "/v1/applications/"+s.a1+"/move", map[string]any{"stage": "offer"}), 200)
	s.want(s.do(s.recruiter, "POST", "/v1/applications/"+s.a1+"/hire", nil), 200)
	if n, err := s.app.dispatcher.DeliverDue(ctxBG()); err != nil || n < 5 {
		t.Fatalf("delivered %d events: %v", n, err)
	}
	seen := map[string]int{}
	for _, ev := range s.hook.all() {
		seen[ev.Type]++
		raw := string(ev.Data)
		for _, pii := range []string{"Ada", "cand1@example.test", "example.test", "555 0100", "cover"} {
			if strings.Contains(raw, pii) {
				t.Errorf("%s carries %q: %s", ev.Type, pii, raw)
			}
		}
		if ev.Source != "jobs" || !strings.HasPrefix(ev.ID, "evt_") {
			t.Errorf("envelope: %+v", ev)
		}
	}
	for _, typ := range []string{EventJobOpened, EventApplicationRecv, EventApplicationStage, EventCandidateHired} {
		if seen[typ] == 0 {
			t.Errorf("no %s event", typ)
		}
	}
	if seen[EventJobOpened] != 2 || seen[EventApplicationRecv] != 2 {
		t.Errorf("counts: %v", seen)
	}
}

func TestReadsAreAuditedAndFailClosed(t *testing.T) {
	s := newScenario(t)
	rec := s.recruiter
	s.want(s.do(rec, "GET", "/v1/candidates/"+s.c1, nil), 200)
	s.want(s.do(rec, "GET", "/v1/applications/"+s.a1+"/resume", nil), 200)
	s.want(s.do(rec, "GET", "/v1/candidates?email=cand1@example.test", nil), 200)
	s.want(s.do(rec, "GET", "/v1/applications", nil), 200)
	log := s.want(s.do(adminTok, "GET", "/v1/audit?limit=200", nil), 200).str()
	for _, want := range []string{`"resource":"candidate:` + s.c1, `"resource":"application:` + s.a1, `"resource":"candidates:list"`, `"resource":"applications:list"`, `"action":"read"`, "email", "phone", `"actor":"rita"`} {
		if !strings.Contains(log, want) {
			t.Errorf("audit log lacks %q", want)
		}
	}
	if strings.Contains(log, "cand1@example.test") || strings.Contains(log, "Ada Example") {
		t.Error("the audit log contains candidate data")
	}
	if err := s.app.store.VerifyAudit(ctxBG()); err != nil {
		t.Errorf("audit chain: %v", err)
	}
	// append-only at the database level
	if _, err := s.app.store.db.Exec(`UPDATE audit_log SET actor = 'x'`); err == nil {
		t.Error("audit rows can be updated")
	}
	if _, err := s.app.store.db.Exec(`DELETE FROM audit_log`); err == nil {
		t.Error("audit rows can be deleted")
	}
	// denials are audited too
	s.do(s.ivy, "GET", "/v1/candidates/"+s.c1, nil)
	if !strings.Contains(s.want(s.do(adminTok, "GET", "/v1/audit?limit=5", nil), 200).str(), `"outcome":"denied"`) {
		t.Error("a role denial was not audited")
	}
	// fail closed: when the audit write cannot happen, no candidate data is returned
	if _, err := s.app.store.db.Exec(`ALTER TABLE audit_log RENAME TO audit_log_x`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/candidates/" + s.c1, "/v1/applications/" + s.a1, "/v1/applications/" + s.a1 + "/resume", "/v1/applications", "/v1/candidates", "/v1/talent-pool", "/v1/applications/" + s.a1 + "/scorecards"} {
		r := s.do(rec, "GET", path, nil)
		if r.Code != 500 || strings.Contains(r.str(), "cand1@example.test") || strings.Contains(r.str(), "Ada") || strings.Contains(r.str(), "RESUME-MARKER") {
			t.Errorf("GET %s with a broken audit log = %d %s", path, r.Code, r.Body)
		}
	}
}

func TestTalentPoolAndConsent(t *testing.T) {
	s := newScenario(t)
	if l := s.want(s.do(s.recruiter, "GET", "/v1/talent-pool", nil), 200).m(t)["items"].([]any); len(l) != 0 {
		t.Errorf("pool = %d", len(l))
	}
	c := s.want(s.do(s.recruiter, "PUT", "/v1/candidates/"+s.c2+"/talent-pool", map[string]any{"member": true}), 200).m(t)
	if c["talent_pool"] != true || c["talent_consent_at"] == nil {
		t.Errorf("candidate: %v", c)
	}
	s.want(s.do(s.recruiter, "PATCH", "/v1/candidates/"+s.c2, map[string]any{"tags": []string{"go", "senior"}}), 200)
	if l := s.want(s.do(s.recruiter, "GET", "/v1/talent-pool?tag=go", nil), 200).m(t)["items"].([]any); len(l) != 1 {
		t.Errorf("tagged pool = %d", len(l))
	}
	if l := s.want(s.do(s.recruiter, "GET", "/v1/talent-pool?tag=rust", nil), 200).m(t)["items"].([]any); len(l) != 0 {
		t.Errorf("wrong tag pool = %d", len(l))
	}
	if r := s.do(s.recruiter, "PATCH", "/v1/candidates/"+s.c2, map[string]any{"tags": []string{"Bad Tag"}}); r.Code != 422 {
		t.Errorf("bad tag = %d", r.Code)
	}
	// a scoped recruiter's pool excludes candidates outside the scope
	if l := s.want(s.do(s.recruiterJ1, "GET", "/v1/talent-pool", nil), 200).m(t)["items"].([]any); len(l) != 0 {
		t.Errorf("scoped pool leaks %d", len(l))
	}
	s.want(s.do(s.recruiter, "PUT", "/v1/candidates/"+s.c2+"/talent-pool", map[string]any{"member": false}), 200)
	if l := s.want(s.do(s.recruiter, "GET", "/v1/talent-pool", nil), 200).m(t)["items"].([]any); len(l) != 0 {
		t.Errorf("withdrawn candidate still in the pool")
	}
	// consent is recorded at intake
	c1 := s.want(s.do(s.recruiter, "GET", "/v1/candidates/"+s.c1, nil), 200).m(t)
	if c1["consent_version"] != "v1" || c1["consent_at"] == nil {
		t.Errorf("consent not recorded: %v", c1)
	}
}

func TestEEOIsSeparateAndAggregateOnly(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.EEO.MinCell = 3 })
	_, slug := e.openJob("Engineer", "alice")
	answers := []map[string]string{
		{"eeo_gender": "female", "eeo_ethnicity": "asian"}, {"eeo_gender": "female", "eeo_ethnicity": "white"}, {"eeo_gender": "female", "eeo_ethnicity": "white"},
		{"eeo_gender": "male", "eeo_ethnicity": "white"}, {"eeo_gender": "male", "eeo_ethnicity": "white"}, {"eeo_gender": "male", "eeo_ethnicity": "white"},
		{"eeo_gender": "self-described-garbage", "eeo_ethnicity": "white"},
	}
	for i, a := range answers {
		f := goodApplication("eeo" + string(rune('a'+i)) + "@example.test")
		for k, v := range a {
			f[k] = v
		}
		e.want(e.apply(slug, f), 303)
	}
	// no table links a response to a person
	cols := func(table string) string {
		rows, err := e.app.store.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			out = append(out, n)
		}
		return strings.Join(out, ",")
	}
	// Regression (QA): no diversity table may have a per-response row, a monotonic id or a
	// timestamp finer than a month, or row order would line responses up with applications.
	var eeoTables []string
	trs, _ := e.app.store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'eeo%'`)
	for trs.Next() {
		var n string
		_ = trs.Scan(&n)
		eeoTables = append(eeoTables, n)
	}
	_ = trs.Close()
	if len(eeoTables) != 1 || eeoTables[0] != "eeo_counts" {
		t.Fatalf("diversity tables = %v, want only the counter table", eeoTables)
	}
	if c := cols("eeo_counts"); c != "month,field,value,n" {
		t.Errorf("eeo_counts columns = %s (no id, no job, no timestamp finer than a month)", c)
	}
	var ddl string
	_ = e.app.store.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'eeo_counts'`).Scan(&ddl)
	if strings.Contains(strings.ToUpper(ddl), "AUTOINCREMENT") || !strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID") {
		t.Errorf("eeo_counts has an insertion sequence: %s", ddl)
	}
	var fks int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_list('eeo_counts')`).Scan(&fks)
	if fks != 0 {
		t.Error("eeo_counts has a foreign key")
	}
	var rows int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM eeo_counts WHERE field = '_responses'`).Scan(&rows)
	if rows != 1 {
		t.Errorf("responses are stored per response, not per month: %d counter rows", rows)
	}
	// Only closed months are reported (live-polling differencing), so roll the month over.
	e.clk.Add(45 * 24 * time.Hour)
	rep := e.want(e.do(adminTok, "GET", "/v1/eeo-report", nil), 200).m(t)
	if rep["responses"] != float64(7) {
		t.Errorf("responses = %v", rep["responses"])
	}
	g := rep["fields"].(map[string]any)["gender"].([]any)
	got := map[string]any{}
	for _, c := range g {
		cm := c.(map[string]any)
		got[cm["value"].(string)] = cm["count"]
	}
	if got["female"] != float64(3) || got["male"] != float64(3) {
		t.Errorf("gender cells: %v", got)
	}
	// complementary suppression: one small ethnicity cell (asian = 1) hides the whole question,
	// otherwise it could be recovered as total minus the others
	for _, c := range rep["fields"].(map[string]any)["ethnicity"].([]any) {
		if c.(map[string]any)["count"] != nil {
			t.Errorf("a question with a suppressed cell still shows counts: %v", c)
		}
	}
	// per-job reports are not offered (a global and a per-job report could be subtracted)
	pj := e.want(e.do(adminTok, "GET", "/v1/eeo-report?job_id=job_0000000000000000", nil), 200).m(t)
	if string(mustJSON(pj)) != string(mustJSON(rep)) {
		t.Error("job_id changes the diversity report; per-job differencing would be possible")
	}
	if _, ok := got["self-described-garbage"]; ok {
		t.Error("free text stored as an answer")
	}
	// nothing in any other API shows diversity data, for any role
	rec := e.mint(RoleRecruiter, "rita")
	appID := e.want(e.do(rec, "GET", "/v1/applications", nil), 200).m(t)["items"].([]any)[0].(map[string]any)["id"].(string)
	for _, tok := range []string{rec, adminTok} {
		for _, p := range []string{"/v1/applications/" + appID, "/v1/applications", "/v1/candidates"} {
			body := e.want(e.do(tok, "GET", p, nil), 200).str()
			if strings.Contains(body, "female") || strings.Contains(body, "asian") || strings.Contains(body, "eeo_") {
				t.Errorf("GET %s exposes diversity data", p)
			}
		}
	}
	// below the minimum overall: everything suppressed
	e2 := newEnv(t, func(c *Config) { c.EEO.MinCell = 5 })
	_, slug2 := e2.openJob("Engineer", "alice")
	f := goodApplication("one@example.test")
	f["eeo_gender"] = "female"
	e2.want(e2.apply(slug2, f), 303)
	e2.clk.Add(45 * 24 * time.Hour)
	rep = e2.want(e2.do(adminTok, "GET", "/v1/eeo-report", nil), 200).m(t)
	if rep["responses"] != nil || strings.Contains(string(mustJSON(rep)), `"count":1`) {
		t.Errorf("a lone response is visible: %v", rep)
	}
	// the form only shows the questions when the operator turned collection on
	off := newEnv(t, func(c *Config) { c.Apply.CollectEEO = false })
	_, slug3 := off.openJob("Engineer", "alice")
	if strings.Contains(off.want(off.do("", "GET", "/careers/"+slug3, nil), 200).str(), "eeo_gender") {
		t.Error("EEO questions shown although collection is off")
	}
	f = goodApplication("two@example.test")
	f["eeo_gender"] = "female"
	off.want(off.apply(slug3, f), 303)
	off.clk.Add(45 * 24 * time.Hour)
	if r := off.want(off.do(adminTok, "GET", "/v1/eeo-report", nil), 200).m(t); r["responses"] != nil {
		t.Errorf("diversity answers stored although collection is off: %v", r)
	}
}

func TestGuardsOnInput(t *testing.T) {
	s := newScenario(t)
	// unknown JSON fields are rejected (mass assignment)
	if r := s.do(adminTok, "PATCH", "/v1/jobs/"+s.j1, map[string]any{"status": "closed", "created_by": "x"}); r.Code != 400 {
		t.Errorf("mass assignment = %d", r.Code)
	}
	// malformed ids
	for _, p := range []string{"/v1/applications/x", "/v1/candidates/%27%20OR%201=1", "/v1/jobs/%00"} {
		if r := s.do(s.recruiter, "GET", p, nil); r.Code != 404 {
			t.Errorf("GET %s = %d", p, r.Code)
		}
	}
	// body size limit
	big := strings.Repeat("a", 2<<20)
	if r := s.do(s.recruiter, "PATCH", "/v1/candidates/"+s.c1, map[string]any{"notes": big}); r.Code != 413 {
		t.Errorf("oversize body = %d", r.Code)
	}
	_ = time.Second
}

// Regression (QA major): blindness depends on "has ever been assigned", so unassigning
// yourself, reading, and re-assigning must not expose colleagues' feedback.
func TestBlindFeedbackSurvivesUnassignAndReassign(t *testing.T) {
	s := newScenario(t)
	app := "/v1/applications/" + s.a1
	s.want(s.do(s.recruiter, "PUT", app+"/interviewers/alice", nil), 204) // alice is the hiring manager of job 1
	s.want(s.do(s.ivy, "PUT", app+"/scorecards/mine", map[string]any{"rating": 4, "recommendation": "yes", "comments": "IVY-SECRET"}), 200)
	s.want(s.do(s.ivy, "POST", app+"/scorecards/mine/submit", nil), 200)
	read := func(tok string) (n int, hidden bool, body string) {
		r := s.want(s.do(tok, "GET", app+"/scorecards", nil), 200)
		m := r.m(t)
		return len(m["items"].([]any)), m["others_hidden"] == true, r.str()
	}
	if n, hidden, _ := read(s.hmAlice); n != 0 || !hidden {
		t.Fatalf("assigned manager is not blind: %d %v", n, hidden)
	}
	// 1. removing your own assignment before submitting is refused
	if r := s.do(s.hmAlice, "DELETE", app+"/interviewers/alice", nil); r.Code != 409 {
		t.Errorf("self-unassign before submitting = %d", r.Code)
	}
	// 2. someone else removes her: she is no longer an interviewer but stays blind
	s.want(s.do(s.recruiter, "DELETE", app+"/interviewers/alice", nil), 204)
	if n, hidden, body := read(s.hmAlice); n != 0 || !hidden || strings.Contains(body, "IVY-SECRET") {
		t.Errorf("unassigned (previously assigned) manager can read feedback: %d %v", n, hidden)
	}
	// 3. re-assigning changes nothing
	s.want(s.do(s.recruiter, "PUT", app+"/interviewers/alice", nil), 204)
	if n, hidden, _ := read(s.hmAlice); n != 0 || !hidden {
		t.Errorf("re-assigned manager can read feedback: %d %v", n, hidden)
	}
	// 4. a draft scorecard row alone also keeps you blind
	s.want(s.do(s.hmAlice, "PUT", app+"/scorecards/mine", map[string]any{"rating": 2, "recommendation": "no"}), 200)
	s.want(s.do(s.recruiter, "DELETE", app+"/interviewers/alice", nil), 204)
	if n, hidden, body := read(s.hmAlice); n != 1 || !hidden || strings.Contains(body, "IVY-SECRET") {
		t.Errorf("a manager with a draft card can read feedback: %d %v", n, hidden) // n=1 is her own draft
	}
	// 5. the same holds for recruiters and admins who were ever assigned
	for name, tok := range map[string]string{"recruiter": s.recruiter, "admin": adminTok} {
		subj := map[string]string{"recruiter": "rita", "admin": bootstrapSubject}[name]
		other := map[string]string{"recruiter": adminTok, "admin": s.recruiter}[name]
		s.want(s.do(other, "PUT", app+"/interviewers/"+subj, nil), 204)
		s.want(s.do(other, "DELETE", app+"/interviewers/"+subj, nil), 204)
		if n, hidden, _ := read(tok); n != 0 || !hidden {
			t.Errorf("a previously assigned %s can read feedback: %d %v", name, n, hidden)
		}
	}
	// 6. once she submits, she sees it, and may then remove herself
	s.want(s.do(s.recruiter, "PUT", app+"/interviewers/alice", nil), 204)
	s.want(s.do(s.hmAlice, "POST", app+"/scorecards/mine/submit", nil), 200)
	if n, hidden, body := read(s.hmAlice); n != 2 || hidden || !strings.Contains(body, "IVY-SECRET") {
		t.Errorf("after submitting: %d %v", n, hidden)
	}
	s.want(s.do(s.hmAlice, "DELETE", app+"/interviewers/alice", nil), 204)
	// assignments are audited with the subject
	log := s.want(s.do(adminTok, "GET", "/v1/audit?limit=200", nil), 200).str()
	if !strings.Contains(log, `"action":"interviewer.remove"`) || !strings.Contains(log, `"action":"interviewer.assign"`) || !strings.Contains(log, "subject=alice") {
		t.Error("assign and unassign are not audited")
	}
	// a removed interviewer can no longer reach the application as an interviewer
	if r := s.do(s.ivy, "GET", app, nil); r.Code != 200 {
		t.Errorf("ivy still assigned: %d", r.Code)
	}
	s.want(s.do(s.recruiter, "DELETE", app+"/interviewers/ivy", nil), 204)
	if r := s.do(s.ivy, "GET", app, nil); r.Code != 404 {
		t.Errorf("a removed interviewer still reaches the application: %d", r.Code)
	}
}

// The report publishes closed months only. Otherwise an admin polling it (or any two reads a
// few minutes apart) could difference the counters and tie a moved cell to a single new
// application, which is the link the counter design exists to remove.
func TestEEOReportCoversClosedMonthsOnly(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.EEO.MinCell = 1 })
	_, slug := e.openJob("Engineer", "alice")
	f := goodApplication("m1@example.test")
	f["eeo_gender"] = "female"
	e.want(e.apply(slug, f), 303)
	rep := e.want(e.do(adminTok, "GET", "/v1/eeo-report", nil), 200).m(t)
	if rep["responses"] != nil && rep["responses"] != float64(0) {
		t.Fatalf("the open month is published: %v", rep)
	}
	for _, c := range rep["fields"].(map[string]any)["gender"].([]any) {
		t.Errorf("open-month cell published: %v", c)
	}
	// the counters themselves are still being written
	var n int
	_ = e.app.store.db.QueryRow(`SELECT COALESCE(SUM(n),0) FROM eeo_counts WHERE field = '_responses'`).Scan(&n)
	if n != 1 {
		t.Fatalf("counter = %d", n)
	}
	e.clk.Add(45 * 24 * time.Hour)
	f2 := goodApplication("m2@example.test")
	f2["eeo_gender"] = "male"
	e.want(e.apply(slug, f2), 303)
	rep = e.want(e.do(adminTok, "GET", "/v1/eeo-report", nil), 200).m(t)
	if rep["responses"] != float64(1) {
		t.Errorf("only the closed month should count: %v", rep["responses"])
	}
	for _, c := range rep["fields"].(map[string]any)["gender"].([]any) {
		if c.(map[string]any)["value"] == "male" {
			t.Errorf("the new open month leaked: %v", c)
		}
	}
}

// Regression (QA minor): a removed interviewer must not be able to finalise a draft they wrote
// while assigned; recruiters and admins keep that right through their role.
func TestSubmitScorecardNeedsActiveAssignmentOrStaffRole(t *testing.T) {
	s := newScenario(t)
	app := "/v1/applications/" + s.a1
	s.want(s.do(s.recruiter, "PUT", app+"/interviewers/ivy", nil), 204)
	s.want(s.do(s.ivy, "PUT", app+"/scorecards/mine", map[string]any{"rating": 3, "recommendation": "yes"}), 200)
	s.want(s.do(s.recruiter, "DELETE", app+"/interviewers/ivy", nil), 204)
	if r := s.do(s.ivy, "POST", app+"/scorecards/mine/submit", nil); r.Code == 200 {
		t.Errorf("a removed interviewer submitted a scorecard: %d", r.Code)
	}
	s.want(s.do(s.recruiter, "PUT", app+"/interviewers/ivy", nil), 204)
	s.want(s.do(s.ivy, "POST", app+"/scorecards/mine/submit", nil), 200)
	// a hiring manager (whose job scope still reaches the application) who was removed
	s.want(s.do(s.recruiter, "PUT", app+"/interviewers/alice", nil), 204)
	s.want(s.do(s.hmAlice, "PUT", app+"/scorecards/mine", map[string]any{"rating": 2, "recommendation": "no"}), 200)
	s.want(s.do(s.recruiter, "DELETE", app+"/interviewers/alice", nil), 204)
	if r := s.do(s.hmAlice, "POST", app+"/scorecards/mine/submit", nil); r.Code == 200 {
		t.Errorf("a removed hiring manager submitted a scorecard: %d", r.Code)
	}
	// a recruiter who was assigned, drafted and removed may still submit
	s.want(s.do(adminTok, "PUT", app+"/interviewers/rita", nil), 204)
	s.want(s.do(s.recruiter, "PUT", app+"/scorecards/mine", map[string]any{"rating": 2, "recommendation": "no"}), 200)
	s.want(s.do(adminTok, "DELETE", app+"/interviewers/rita", nil), 204)
	s.want(s.do(s.recruiter, "POST", app+"/scorecards/mine/submit", nil), 200)
}
