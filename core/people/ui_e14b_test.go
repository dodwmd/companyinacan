package people

import (
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// UI of time off and reviews (E14b services). All names and values are fake.

func newUIFor(e *env) *uiEnv {
	u := &uiEnv{env: e, st: &StaticTokens{}, now: *e.clock}
	u.ui = &UI{Svc: e.s, Auth: MultiAuth{u.st}, Now: func() time.Time { return u.now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	u.h = u.ui.Handler()
	return u
}

func firstMatch(t *testing.T, re string, w interface{ String() string }) string {
	t.Helper()
	m := regexp.MustCompile(re).FindStringSubmatch(w.String())
	if m == nil {
		t.Fatalf("no match for %s in %s", re, w.String())
	}
	return m[1]
}

func TestUITimeOffFlowAndMaskedCalendar(t *testing.T) {
	x := newToEnv(t)
	u := newUIFor(x.env)
	team, err := x.s.CreateOrgUnit(x.ctx, x.hr, "teams", OrgInput{Name: "Core"})
	x.must(err)
	for _, id := range []string{x.ann.ID, x.ben.ID, x.mgr.ID} {
		_, err := x.s.UpdatePerson(x.ctx, x.hr, id, PersonPatch{TeamID: &team.ID})
		x.must(err)
	}
	ca, cm, cb := u.login(t, x.annA), u.login(t, x.mgrA), u.login(t, x.benA)

	w := ca.get("/timeoff")
	mustHave(t, w, "Vacation", "Sick leave")
	// a half-day request, submitted at once
	w = ca.post("/timeoff", url.Values{"type_id": {x.vac.ID}, "start_date": {"2026-03-23"}, "start_part": {"pm"}, "end_date": {"2026-03-24"}, "end_part": {"am"}, "note": {"dentist PRIVATE-NOTE"}, "action": {"submit"}})
	if w.Code != 303 {
		t.Fatalf("request = %d %s", w.Code, w.Body.String())
	}
	loc, _, _ := strings.Cut(w.Header().Get("Location"), "?")
	mustHave(t, ca.get(loc), "PRIVATE-NOTE", "Pending")
	// the manager sees it in the queue and on the detail page, approves it
	mustHave(t, cm.get("/timeoff/queue"), "Ann")
	mustHave(t, cm.get(loc), "PRIVATE-NOTE")
	if w := ca.post(loc+"/decide", url.Values{"decision": {"approve"}}); w.Code == 303 {
		t.Fatal("requester approved their own leave")
	}
	if w := cb.get(loc); w.Code != 404 {
		t.Fatalf("colleague reads a request = %d", w.Code)
	}
	if w := cm.post(loc+"/decide", url.Values{"decision": {"approve"}}); w.Code != 303 {
		t.Fatalf("approve = %d %s", w.Code, w.Body.String())
	}
	mustHave(t, ca.get(loc), "Approved")
	mustNot(t, cm.get("/timeoff/queue"), "dentist")

	// a sensitive-type request, approved
	sick := x.submit(x.annA, x.sick, "2026-03-25", "2026-03-25")
	_, err = x.s.DecideLeaveRequest(x.ctx, x.mgrA, sick.ID, true)
	x.must(err)
	// the colleague's calendar: only "out", never type, note or status
	w = cb.get("/timeoff/calendar?from=2026-03-20&days=14")
	mustHave(t, w, "Ann")
	mustNot(t, w, "Sick leave", "Vacation", "dentist", "PRIVATE-NOTE", "Pending", "Approved", "lvr_")
	// the person sees their own types
	mustHave(t, ca.get("/timeoff/calendar?from=2026-03-20&days=14"), "Sick leave")
	// a draft is private
	d := x.draft(x.annA, x.vac, "2026-05-04", "2026-05-05")
	if w := cm.get("/timeoff/" + d.ID); w.Code != 404 {
		t.Fatalf("manager reads a draft = %d", w.Code)
	}
	mustNot(t, cm.get("/timeoff/queue"), "May")
	// cancel
	if w := ca.post("/timeoff/"+d.ID+"/cancel", nil); w.Code != 303 {
		t.Fatalf("cancel = %d %s", w.Code, w.Body.String())
	}
	// leave masking: Ann's long leave still reads active in the directory for colleagues
	mustNot(t, cb.get("/people/"+x.ann.ID), "On leave")
}

func TestUITimeOffNeedsItsPermission(t *testing.T) {
	x := newToEnv(t)
	u := newUIFor(x.env)
	u.st.Add("pat-readonly-0123456789abcdef", Principal{Subject: x.annA.Subject, Roles: []string{RoleEmployee}, Perms: []string{"people.read", "people.timeoff.read"}})
	c := u.client(t)
	csrf := csrfRe.FindStringSubmatch(c.get("/login").Body.String())[1]
	if w := c.do("POST", "/login", url.Values{"token": {"pat-readonly-0123456789abcdef"}, "csrf": {csrf}}, "Sec-Fetch-Site", "same-origin"); w.Code != 303 {
		t.Fatalf("login = %d", w.Code)
	}
	c.get("/")
	w := c.post("/timeoff", url.Values{"type_id": {x.vac.ID}, "start_date": {"2026-03-23"}, "end_date": {"2026-03-23"}, "action": {"draft"}})
	if w.Code != 403 {
		t.Fatalf("request without people.timeoff.write = %d", w.Code)
	}
	if w := c.get("/reviews"); w.Code != 200 {
		t.Fatalf("reviews list = %d", w.Code)
	}
}

func TestUIReviewFlowCalibrationNeverShownToSubject(t *testing.T) {
	r := newRevEnv(t)
	u := newUIFor(r.env)
	_, rid := r.openCycle("anonymous")
	ca, cm, chr := u.login(t, r.annA), u.login(t, r.mgrA), u.login(t, r.hr2)

	w := ca.get("/reviews")
	mustHave(t, w, "Ann")
	w = ca.get("/reviews/" + rid)
	mustHave(t, w, "Delivers results", "/reviews/"+rid+"/self")
	mustNot(t, w, "/manager")
	// self-assessment: submit
	if w := ca.post("/reviews/"+rid+"/self", url.Values{"r_q1": {"4"}, "t_q2": {"SELF-ANSWER"}, "submit": {"1"}}); w.Code != 303 {
		t.Fatalf("self = %d %s", w.Code, w.Body.String())
	}
	mustHave(t, ca.get("/reviews/"+rid), "SELF-ANSWER")
	if w := ca.post("/reviews/"+rid+"/self", url.Values{"r_q1": {"5"}, "submit": {"1"}}); w.Code != 409 && w.Code != 403 {
		t.Fatalf("second self submit = %d", w.Code)
	}
	// manager review: the manager sees the form and the self-assessment, the report never sees the manager form
	wm := cm.get("/reviews/" + rid)
	mustHave(t, wm, "SELF-ANSWER", "/reviews/"+rid+"/manager")
	if w := cm.post("/reviews/"+rid+"/manager", url.Values{"r_q1": {"3"}, "t_q2": {"MGR-ANSWER"}, "rating": {"4"}, "summary": {"MGR-SUMMARY"}, "submit": {"1"}}); w.Code != 303 {
		t.Fatalf("manager = %d %s", w.Code, w.Body.String())
	}
	mustNot(t, ca.get("/reviews/"+rid), "MGR-ANSWER", "MGR-SUMMARY")
	if w := ca.post("/reviews/"+rid+"/manager", url.Values{"rating": {"5"}, "submit": {"1"}}); w.Code != 403 && w.Code != 404 {
		t.Fatalf("report wrote the manager review = %d", w.Code)
	}
	// calibration, release and acknowledgement through the service; the UI only renders
	_, err := r.s.SetCycleState(r.ctx, r.hr, r.cycle.ID, "calibration")
	r.must(err)
	r.must(r.s.Calibrate(r.ctx, r.hr2, rid, 2, "CALIBRATION-SECRET"))
	mustHave(t, chr.get("/reviews/"+rid), "CALIBRATION-SECRET")
	mustNot(t, ca.get("/reviews/"+rid), "CALIBRATION-SECRET", "calibration")
	r.must(r.s.RequestRelease(r.ctx, r.hr2, rid))
	hrB := r.hr
	hrB.Roles = []string{RoleHR, RoleAdmin}
	_ = hrB
	_, other := r.person("Hugo", "", RoleEmployee, RoleHR)
	r.must(r.s.DecideRelease(r.ctx, other, rid, true, ""))
	wa := ca.get("/reviews/" + rid)
	mustNot(t, wa, "CALIBRATION-SECRET")
	mustHave(t, wa, "/reviews/"+rid+"/acknowledge")
	if w := ca.post("/reviews/"+rid+"/acknowledge", url.Values{"comment": {"thanks"}}); w.Code != 303 {
		t.Fatalf("ack = %d %s", w.Code, w.Body.String())
	}
	if w := ca.post("/reviews/"+rid+"/acknowledge", url.Values{"comment": {"again"}}); w.Code != 409 {
		t.Fatalf("second ack = %d", w.Code)
	}
	// a colleague cannot find the review
	if w := u.login(t, r.benA).get("/reviews/" + rid); w.Code != 404 {
		t.Fatalf("colleague = %d", w.Code)
	}
}

func TestUIAnonymousFeedbackShowsNoCountsOrReviewerStatusWhileOpen(t *testing.T) {
	r := newRevEnv(t)
	u := newUIFor(r.env)
	_, rid := r.openCycle("anonymous")
	ca, cm := u.login(t, r.annA), u.login(t, r.mgrA)

	// Ann proposes three peers; the manager approves
	for _, p := range []*Person{r.ben, r.cat, r.dan} {
		if w := ca.post("/reviews/"+rid+"/nominations", url.Values{"reviewer_id": {p.ID}, "kind": {"peer"}}); w.Code != 303 {
			t.Fatalf("nominate = %d %s", w.Code, w.Body.String())
		}
	}
	wm := cm.get("/reviews/" + rid)
	noms := regexp.MustCompile(`/nominations/([a-z0-9_]+)/decide`).FindAllStringSubmatch(wm.Body.String(), -1)
	if len(noms) != 3 {
		t.Fatalf("manager sees %d proposals: %s", len(noms), wm.Body.String())
	}
	for _, n := range noms {
		if w := cm.post("/nominations/"+n[1]+"/decide", url.Values{"decision": {"approve"}}); w.Code != 303 {
			t.Fatalf("decide = %d %s", w.Code, w.Body.String())
		}
	}
	// the three reviewers answer through their inbox; Ben answers twice (the second is refused)
	var reviewers = []Actor{r.benA, r.catA, r.danA}
	for i, ra := range reviewers {
		c := u.login(t, ra)
		inbox := c.get("/feedback")
		mustHave(t, inbox, "Ann")
		mustNot(t, inbox, "Mgr Example", "proposed by", "nominated")
		fid := firstMatch(t, `/feedback/(frq_[0-9a-f]+)`, inbox.Body)
		form := c.get("/feedback/" + fid)
		mustHave(t, form, "Delivers results")
		if w := c.post("/feedback/"+fid, url.Values{"r_q1": {"4"}, "t_q2": {"PEER-COMMENT-" + string(rune('A'+i))}}); w.Code != 303 {
			t.Fatalf("submit = %d %s", w.Code, w.Body.String())
		}
		if i == 0 {
			if w := c.post("/feedback/"+fid, url.Values{"r_q1": {"1"}}); w.Code != 409 && w.Code != 404 {
				t.Fatalf("second answer = %d", w.Code)
			}
		}
	}
	// while the cycle is open nobody sees how many answered or who did
	for name, c := range map[string]*client{"subject": ca, "manager": cm, "hr": u.login(t, r.hr2)} {
		w := c.get("/reviews/" + rid)
		if w.Code != 200 {
			t.Fatalf("%s = %d", name, w.Code)
		}
		body := w.Body.String()
		for _, bad := range []string{"1 of 3", "2 of 3", "3 of 3", "1 response", "2 responses", "3 responses", "Responses:", "answered", "submitted feedback"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s page shows %q while the cycle is open", name, bad)
			}
		}
		for _, nm := range []string{"Ben Example", "Cat Example", "Dan Example"} {
			// reviewers appear only as nominees to the people who may see nominations, never with a response state
			idx := strings.Index(body, nm)
			if idx >= 0 && regexp.MustCompile(`(?i)(done|answered|submitted|pending response)`).MatchString(body[idx:min(len(body), idx+200)]) {
				t.Errorf("%s page shows a response state next to %s", name, nm)
			}
		}
	}
	mustNot(t, ca.get("/reviews/"+rid), "PEER-COMMENT")
	// an answered inbox no longer lists the request, and says nothing else about it
	cb := u.login(t, r.benA)
	if w := cb.get("/feedback"); strings.Contains(w.Body.String(), "/feedback/frq_") {
		t.Error("answered request still in the inbox")
	}
}

func TestUIReviewIsInvisibleToOthersAnd404Uniform(t *testing.T) {
	r := newRevEnv(t)
	u := newUIFor(r.env)
	_, rid := r.openCycle("attributed")
	cb := u.login(t, r.benA)
	var bodies []string
	for _, path := range []string{"/reviews/" + rid, "/reviews/rev_" + strings.Repeat("0", 24), "/reviews/bogus", "/feedback/frq_" + strings.Repeat("0", 24), "/timeoff/lvr_" + strings.Repeat("0", 24)} {
		w := cb.get(path)
		if w.Code != 404 {
			t.Errorf("%s = %d", path, w.Code)
		}
		bodies = append(bodies, regexp.MustCompile(`value="[^"]*"|Request id[^<]*<code>[^<]*`).ReplaceAllString(w.Body.String(), ""))
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Error("404 pages differ, existence is observable")
		}
	}
}
