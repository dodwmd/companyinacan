package people

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// All names, ids and values in these tests are fake.

type revEnv struct {
	*env
	boss, mgr, ann, ben, cat, dan, eve *Person
	bossA, mgrA, annA, benA, catA      Actor
	danA, eveA                         Actor
	hr2                                Actor // a second hr person
	cycle                              *Cycle
}

func newRevEnv(t *testing.T) *revEnv {
	e := newEnv(t)
	r := &revEnv{env: e}
	r.boss, r.bossA = e.person("Boss", "", RoleEmployee, RoleManager)
	r.mgr, r.mgrA = e.person("Mgr", r.boss.ID, RoleEmployee, RoleManager)
	r.ann, r.annA = e.person("Ann", r.mgr.ID, RoleEmployee)
	r.ben, r.benA = e.person("Ben", r.mgr.ID, RoleEmployee)
	r.cat, r.catA = e.person("Cat", r.mgr.ID, RoleEmployee)
	r.dan, r.danA = e.person("Dan", r.mgr.ID, RoleEmployee)
	r.eve, r.eveA = e.person("Eve", r.mgr.ID, RoleEmployee)
	_, r.hr2 = e.person("Hera", "", RoleEmployee, RoleHR)
	return r
}

var testQuestions = []Question{
	{ID: "q1", Text: "Delivers results", Type: "rating", Audience: "all"},
	{ID: "q2", Text: "What went well?", Type: "text", Audience: "all"},
}

// openCycle creates a template and a cycle with Ann as participant and opens it.
func (r *revEnv) openCycle(anonymity string) (*Cycle, string) {
	r.t.Helper()
	tpl, err := r.s.CreateReviewTemplate(r.ctx, r.hr, TemplateIn{Name: "annual-" + anonymity, Questions: testQuestions})
	r.must(err)
	cy, err := r.s.CreateCycle(r.ctx, r.hr, CycleIn{Name: "2026-" + anonymity, TemplateID: tpl.ID, Anonymity: anonymity})
	r.must(err)
	ids, err := r.s.AddParticipants(r.ctx, r.hr, cy.ID, []string{r.ann.ID})
	r.must(err)
	cy, err = r.s.SetCycleState(r.ctx, r.hr, cy.ID, "open")
	r.must(err)
	r.cycle = cy
	return cy, ids[0]
}

func rating(n int) *int { return &n }

func TestCycleLifecycleAndEvents(t *testing.T) {
	r := newRevEnv(t)
	tpl, err := r.s.CreateReviewTemplate(r.ctx, r.hr, TemplateIn{Name: "t", Questions: testQuestions})
	r.must(err)
	if _, err := r.s.CreateReviewTemplate(r.ctx, r.mgrA, TemplateIn{Name: "t2", Questions: testQuestions}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager created a template: %v", err)
	}
	// Questions are validated.
	for _, bad := range [][]Question{
		nil,
		{{ID: "q1", Text: "x", Type: "essay", Audience: "all"}},
		{{ID: "q1", Text: "x", Type: "rating", Audience: "all"}, {ID: "q1", Text: "y", Type: "text", Audience: "all"}},
		{{ID: "Q 1", Text: "x", Type: "text", Audience: "all"}},
		{{ID: "q1", Text: "", Type: "text", Audience: "all"}},
	} {
		if _, err := r.s.CreateReviewTemplate(r.ctx, r.hr, TemplateIn{Name: "bad", Questions: bad}); err == nil {
			t.Errorf("accepted bad questions %v", bad)
		}
	}
	// Anonymity threshold floor is 3.
	if _, err := r.s.CreateCycle(r.ctx, r.hr, CycleIn{Name: "c", TemplateID: tpl.ID, Anonymity: "anonymous", MinResponses: 2}); err == nil {
		t.Fatal("min_responses below 3 accepted")
	}
	cy, err := r.s.CreateCycle(r.ctx, r.hr, CycleIn{Name: "c", TemplateID: tpl.ID, Anonymity: "anonymous"})
	r.must(err)
	if cy.State != "draft" || cy.MinResponses != 3 {
		t.Fatalf("cycle = %+v", cy)
	}
	if _, err := r.s.SetCycleState(r.ctx, r.hr, cy.ID, "open"); !errors.Is(err, ErrConflict) {
		t.Fatalf("opened an empty cycle: %v", err)
	}
	if _, err := r.s.AddParticipants(r.ctx, r.mgrA, cy.ID, []string{r.ann.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager added participants: %v", err)
	}
	if _, err := r.s.AddParticipants(r.ctx, r.hr, cy.ID, []string{r.ann.ID, r.ann.ID}); err != nil {
		t.Fatalf("duplicate in one call must be idempotent: %v", err)
	}
	if _, err := r.s.SetCycleState(r.ctx, r.hr, cy.ID, "calibration"); !errors.Is(err, ErrConflict) {
		t.Fatalf("skipped a state: %v", err)
	}
	if _, err := r.s.SetCycleState(r.ctx, r.hr, cy.ID, "open"); err != nil {
		t.Fatal(err)
	}
	// No going back, no adding after calibration starts.
	if _, err := r.s.SetCycleState(r.ctx, r.hr, cy.ID, "draft"); err == nil {
		t.Fatal("moved a cycle back to draft")
	}
	evs, _ := r.s.AllEvents(r.ctx)
	n := 0
	for _, ev := range evs {
		if ev.Type == EventReviewCycleOpened {
			n++
			for k := range ev.Data {
				if k != "cycle_id" && k != "opened_on" && k != "participants" {
					t.Errorf("unexpected key %q in cycle.opened", k)
				}
			}
		}
	}
	if n != 1 {
		t.Fatalf("cycle.opened events = %d", n)
	}
}

func TestReviewFlowVisibilityAndRelease(t *testing.T) {
	r := newRevEnv(t)
	_, id := r.openCycle("anonymous")
	colleague := r.benA
	hiring := Actor{Subject: r.benA.Subject, Roles: []string{"hiring"}, PersonID: r.ben.ID, HasPerson: true, Status: StActive}

	// Before anything is written only the subject, the direct manager and hr know the review exists.
	for name, a := range map[string]Actor{"colleague": colleague, "skip-level": r.bossA, "hiring": hiring} {
		if _, err := r.s.GetReview(r.ctx, a, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: GetReview = %v, want not found", name, err)
		}
	}
	for name, a := range map[string]Actor{"ann": r.annA, "manager": r.mgrA, "hr": r.hr} {
		if _, err := r.s.GetReview(r.ctx, a, id); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.s.GetReview(r.ctx, r.annA, "rev_zzzz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id: %v", err)
	}

	// Self-assessment: sealed at rest, hidden from the manager until submitted.
	r.must(r.s.SaveSelf(r.ctx, r.annA, id, Answers{"q1": {Rating: rating(4)}, "q2": {Text: "shipped the billing thing, ZEBRA-SELF"}}, false))
	rv, _ := r.s.GetReview(r.ctx, r.mgrA, id)
	if rv.Self != nil {
		t.Fatal("manager sees an unsubmitted self-assessment")
	}
	if err := r.s.SaveSelf(r.ctx, r.benA, id, Answers{"q1": {Rating: rating(1)}}, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("colleague wrote someone's self-assessment: %v", err)
	}
	if err := r.s.SaveSelf(r.ctx, r.annA, id, Answers{"q1": {Rating: rating(9)}}, false); err == nil {
		t.Fatal("rating above the scale accepted")
	}
	if err := r.s.SaveSelf(r.ctx, r.annA, id, Answers{"nope": {Text: "x"}}, false); err == nil {
		t.Fatal("unknown question accepted")
	}
	r.must(r.s.SaveSelf(r.ctx, r.annA, id, Answers{"q1": {Rating: rating(4)}, "q2": {Text: "shipped the billing thing, ZEBRA-SELF"}}, true))
	if err := r.s.SaveSelf(r.ctx, r.annA, id, Answers{"q1": {Rating: rating(5)}}, true); err == nil {
		t.Fatal("submitted self-assessment was edited")
	}
	rv, _ = r.s.GetReview(r.ctx, r.mgrA, id)
	if rv.Self == nil || rv.Self.Answers["q2"].Text == "" {
		t.Fatalf("manager cannot read the submitted self-assessment: %+v", rv.Self)
	}
	if rv, _ = r.s.GetReview(r.ctx, r.bossA, id); rv != nil {
		t.Fatal("skip-level read an unreleased review")
	}

	// Manager review. The employee sees nothing of it before release.
	if err := r.s.SaveManager(r.ctx, r.annA, id, ManagerIn{Rating: rating(3), Submit: true}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("employee wrote the manager review: %v", err)
	}
	if err := r.s.SaveManager(r.ctx, r.bossA, id, ManagerIn{Rating: rating(3)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("skip-level wrote the manager review: %v", err)
	}
	r.must(r.s.SaveManager(r.ctx, r.mgrA, id, ManagerIn{Answers: Answers{"q1": {Rating: rating(3)}, "q2": {Text: "solid, ZEBRA-MGR"}}, Rating: rating(3), Summary: "meets expectations ZEBRA-SUM", Submit: true}))
	rv, _ = r.s.GetReview(r.ctx, r.annA, id)
	if rv.Manager != nil || rv.Outcome != nil || rv.Calibration != nil || rv.Self == nil {
		t.Fatalf("employee view before release = %+v", rv)
	}

	// Sealed at rest: no plaintext in the database file or its WAL.
	r.must(r.s.scrub(r.ctx, false))
	raw, _ := os.ReadFile(r.path)
	for _, w := range []string{"ZEBRA-SELF", "ZEBRA-MGR", "ZEBRA-SUM"} {
		if strings.Contains(string(raw), w) {
			t.Errorf("%s stored in plaintext", w)
		}
	}

	// A sealed value copied onto another review fails to open (AAD binding): the read errors, never leaks.
	_, id2 := func() (*Cycle, string) {
		cy, _ := r.s.CreateCycle(r.ctx, r.hr, CycleIn{Name: "other", TemplateID: r.cycle.TemplateID, Anonymity: "attributed"})
		ids, err := r.s.AddParticipants(r.ctx, r.hr, cy.ID, []string{r.ben.ID})
		r.must(err)
		return cy, ids[0]
	}()
	var sealed string
	r.must(r.s.db.QueryRow(`SELECT self_sealed FROM review_participants WHERE id = ?`, id).Scan(&sealed))
	_, err := r.s.db.Exec(`UPDATE review_participants SET self_sealed = ?, self_submitted_at = 1 WHERE id = ?`, sealed, id2)
	r.must(err)
	if _, err := r.s.GetReview(r.ctx, r.benA, id2); err == nil {
		t.Fatal("a sealed value moved to another row was opened")
	}

	// Calibration needs the calibration stage; the employee never sees notes.
	if err := r.s.Calibrate(r.ctx, r.hr, id, 4, "NOTES-ZEBRA"); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrConflict) {
		t.Fatalf("calibrated while the cycle is open: %v", err)
	}
	_, err = r.s.SetCycleState(r.ctx, r.hr, r.cycle.ID, "calibration")
	r.must(err)
	if err := r.s.Calibrate(r.ctx, r.mgrA, id, 4, "x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager calibrated: %v", err)
	}
	r.must(r.s.Calibrate(r.ctx, r.hr, id, 4, "NOTES-ZEBRA"))
	rv, _ = r.s.GetReview(r.ctx, r.hr, id)
	if rv.Calibration == nil || rv.Calibration.Notes != "NOTES-ZEBRA" || rv.Calibration.Rating == nil || *rv.Calibration.Rating != 4 {
		t.Fatalf("hr cannot read calibration: %+v", rv.Calibration)
	}
	for name, a := range map[string]Actor{"ann": r.annA, "manager": r.mgrA} {
		if rv, _ := r.s.GetReview(r.ctx, a, id); rv.Calibration != nil {
			t.Errorf("%s sees calibration", name)
		}
	}

	// Release: hr asks, a different hr approves.
	if err := r.s.RequestRelease(r.ctx, r.mgrA, id); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager requested release: %v", err)
	}
	r.must(r.s.RequestRelease(r.ctx, r.hr, id))
	if rv, _ = r.s.GetReview(r.ctx, r.annA, id); rv.Outcome != nil {
		t.Fatal("outcome visible before the release is approved")
	}
	if err := r.s.DecideRelease(r.ctx, r.hr, id, true, ""); !errors.Is(err, ErrSeparation) {
		t.Fatalf("requester approved their own release: %v", err)
	}
	r.must(r.s.DecideRelease(r.ctx, r.hr2, id, true, ""))

	// After release: subject and manager see the outcome and the manager part, skip-level the summary only.
	rv, _ = r.s.GetReview(r.ctx, r.annA, id)
	if rv.Outcome == nil || rv.Outcome.Rating != 4 || rv.Manager == nil || rv.Manager.Answers["q2"].Text != "solid, ZEBRA-MGR" || rv.Calibration != nil {
		t.Fatalf("employee view after release = %+v", rv)
	}
	rv, err = r.s.GetReview(r.ctx, r.bossA, id)
	r.must(err)
	if rv.Outcome == nil || rv.Self != nil || rv.Manager != nil || rv.Calibration != nil || rv.Peer != nil {
		t.Fatalf("skip-level sees more than the summary: %+v", rv)
	}
	if _, err := r.s.GetReview(r.ctx, r.benA, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("colleague after release: %v", err)
	}
	// Acknowledgement: subject only, once.
	if err := r.s.Acknowledge(r.ctx, r.mgrA, id, ""); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager acknowledged: %v", err)
	}
	r.must(r.s.Acknowledge(r.ctx, r.annA, id, "thanks"))
	if err := r.s.Acknowledge(r.ctx, r.annA, id, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("double acknowledge: %v", err)
	}
	// Released content is frozen.
	if err := r.s.Calibrate(r.ctx, r.hr, id, 1, "x"); err == nil {
		t.Fatal("calibrated a released review")
	}
	// Event: ids only.
	evs, _ := r.s.AllEvents(r.ctx)
	found := false
	for _, ev := range evs {
		if ev.Type == EventReviewReleased {
			found = true
			for k, v := range ev.Data {
				if k != "cycle_id" && k != "review_id" && k != "person_id" && k != "released_on" {
					t.Errorf("unexpected key %q=%v in review.released", k, v)
				}
			}
		}
	}
	if !found {
		t.Fatal("no review.released.v1")
	}
	// The subject lists their history.
	l, err := r.s.ListReviews(r.ctx, r.annA, "")
	r.must(err)
	if len(l) != 1 || l[0].ID != id {
		t.Fatalf("history = %+v", l)
	}
	if l, _ = r.s.ListReviews(r.ctx, r.benA, ""); len(l) != 1 { // ben has one row in the "other" cycle only
		t.Fatalf("ben's list = %+v", l)
	}
}

func TestReleaseSeparationOfDutiesAndNoLaundering(t *testing.T) {
	r := newRevEnv(t)
	_, id := r.openCycle("attributed")
	r.must(r.s.SaveSelf(r.ctx, r.annA, id, Answers{"q1": {Rating: rating(3)}}, true))
	r.must(r.s.SaveManager(r.ctx, r.mgrA, id, ManagerIn{Rating: rating(3), Summary: "ok", Submit: true}))
	_, err := r.s.SetCycleState(r.ctx, r.hr, r.cycle.ID, "calibration")
	r.must(err)
	hrA := Actor{Subject: "sso-hera", Roles: []string{RoleEmployee, RoleHR}, PersonID: r.hr2.PersonID, HasPerson: true, Status: StActive}
	hrB := Actor{Subject: "svc-hr-b", Roles: []string{RoleHR}}
	hrC := Actor{Subject: "svc-hr-c", Roles: []string{RoleAdmin}}
	r.must(r.s.Calibrate(r.ctx, hrB, id, 3, ""))
	r.must(r.s.RequestRelease(r.ctx, hrA, id))
	// The calibrator and the requester cannot approve; neither can a Unicode-variant of their subject.
	for name, a := range map[string]Actor{
		"requester":  hrA,
		"calibrator": hrB,
		"variant":    {Subject: " SVC-HR-B\u200b", Roles: []string{RoleHR}},
		"fullwidth":  {Subject: "ｓｖｃ-ｈｒ-ｂ", Roles: []string{RoleHR}},
	} {
		if err := r.s.DecideRelease(r.ctx, a, id, true, ""); !errors.Is(err, ErrSeparation) {
			t.Errorf("%s: %v, want separation of duties", name, err)
		}
	}
	// Reject and resubmit: the history stays, so the original requester still cannot approve.
	r.must(r.s.DecideRelease(r.ctx, hrC, id, false, "needs a rewrite"))
	r.must(r.s.RequestRelease(r.ctx, hrC, id))
	if err := r.s.DecideRelease(r.ctx, hrA, id, true, ""); !errors.Is(err, ErrSeparation) {
		t.Fatalf("original requester approved after reject+resubmit: %v", err)
	}
	if err := r.s.DecideRelease(r.ctx, hrB, id, true, ""); !errors.Is(err, ErrSeparation) {
		t.Fatalf("calibrator approved after reject+resubmit: %v", err)
	}
	hrD := Actor{Subject: "svc-hr-d", Roles: []string{RoleHR}}
	// The manager who wrote the review and the subject can never approve, even holding hr.
	mgrHR := r.mgrA
	mgrHR.Roles = []string{RoleEmployee, RoleManager, RoleHR}
	if err := r.s.DecideRelease(r.ctx, mgrHR, id, true, ""); !errors.Is(err, ErrSeparation) {
		t.Fatalf("review author approved: %v", err)
	}
	annHR := r.annA
	annHR.Roles = []string{RoleEmployee, RoleHR}
	if err := r.s.DecideRelease(r.ctx, annHR, id, true, ""); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrSeparation) {
		t.Fatalf("subject approved their own release: %v", err)
	}
	r.must(r.s.DecideRelease(r.ctx, hrD, id, true, ""))
}

func TestApprovalEngineDecidesRelease(t *testing.T) {
	r := newRevEnv(t)
	eng := &fakeEngine{}
	r.s.approvals = eng
	_, id := r.openCycle("attributed")
	r.must(r.s.SaveManager(r.ctx, r.mgrA, id, ManagerIn{Rating: rating(3), Summary: "ok", Submit: true}))
	_, err := r.s.SetCycleState(r.ctx, r.hr, r.cycle.ID, "calibration")
	r.must(err)
	r.must(r.s.RequestRelease(r.ctx, r.hr, id))
	if len(eng.reqs) != 1 || eng.reqs[0].Type != "review-release" || eng.reqs[0].Ref != id {
		t.Fatalf("engine requests = %+v", eng.reqs)
	}
	if err := r.s.DecideRelease(r.ctx, r.hr2, id, true, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("approved while the engine says pending: %v", err)
	}
	eng.out, eng.by = "approved", "svc-hr" // the engine's decider is the requester's identity: separation applies to it too
	if err := r.s.DecideRelease(r.ctx, r.hr2, id, true, ""); !errors.Is(err, ErrSeparation) {
		t.Fatalf("engine decider equal to the requester: %v", err)
	}
	eng.by = "sso-hera"
	r.must(r.s.DecideRelease(r.ctx, r.hr2, id, true, ""))
}

func TestPeerFeedbackAnonymityThresholdAndNominations(t *testing.T) {
	r := newRevEnv(t)
	_, id := r.openCycle("anonymous")
	// The employee proposes peers; only the subject's own proposals are visible to them.
	nBen, err := r.s.Nominate(r.ctx, r.annA, id, r.ben.ID, "peer")
	r.must(err)
	if _, err := r.s.Nominate(r.ctx, r.annA, id, r.ann.ID, "peer"); err == nil {
		t.Fatal("nominated oneself")
	}
	if _, err := r.s.Nominate(r.ctx, r.benA, id, r.cat.ID, "peer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("colleague nominated for someone else: %v", err)
	}
	if _, err := r.s.Nominate(r.ctx, r.annA, id, r.ben.ID, "peer"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate nomination: %v", err)
	}
	// A nomination is not a request until the manager approves it.
	if l, _ := r.s.MyFeedbackRequests(r.ctx, r.benA); len(l) != 0 {
		t.Fatalf("ben sees an unapproved nomination: %+v", l)
	}
	if err := r.s.DecideNomination(r.ctx, r.annA, nBen.ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("employee approved their own nomination: %v", err)
	}
	r.must(r.s.DecideNomination(r.ctx, r.mgrA, nBen.ID, true))
	// The manager adds more directly; those are invisible to the subject.
	var rest []*Nomination
	for _, p := range []*Person{r.cat, r.dan, r.eve} {
		n, err := r.s.Nominate(r.ctx, r.mgrA, id, p.ID, "peer")
		r.must(err)
		rest = append(rest, n)
	}
	_ = rest
	ns, err := r.s.ListNominations(r.ctx, r.annA, id)
	r.must(err)
	if len(ns) != 1 || ns[0].ReviewerID != r.ben.ID {
		t.Fatalf("subject sees nominations = %+v", ns)
	}
	ns, _ = r.s.ListNominations(r.ctx, r.mgrA, id)
	if len(ns) != 4 {
		t.Fatalf("manager sees %d nominations", len(ns))
	}
	// The reviewer sees the request but not who asked.
	reqs, err := r.s.MyFeedbackRequests(r.ctx, r.benA)
	r.must(err)
	if len(reqs) != 1 || reqs[0].SubjectID != r.ann.ID || len(reqs[0].Questions) != 2 {
		t.Fatalf("requests = %+v", reqs)
	}
	if b, _ := jsonString(reqs[0]); strings.Contains(b, "proposed") || strings.Contains(b, r.annA.PersonID+"\"") && strings.Contains(b, "nominated") {
		t.Fatalf("request leaks the nominator: %s", b)
	}
	if err := r.s.SubmitFeedback(r.ctx, r.catA, reqs[0].ID, Answers{"q1": {Rating: rating(5)}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else answered ben's request: %v", err)
	}

	submit := func(a Actor, rt int, text string) {
		t.Helper()
		l, err := r.s.MyFeedbackRequests(r.ctx, a)
		r.must(err)
		if len(l) != 1 {
			t.Fatalf("%s has %d requests", a.Subject, len(l))
		}
		r.must(r.s.SubmitFeedback(r.ctx, a, l[0].ID, Answers{"q1": {Rating: rating(rt)}, "q2": {Text: text}}))
		if err := r.s.SubmitFeedback(r.ctx, a, l[0].ID, Answers{"q1": {Rating: rating(1)}}); !errors.Is(err, ErrConflict) {
			t.Fatalf("answered twice: %v", err)
		}
	}
	// Two responses: below the threshold nobody sees any feedback content, hr included.
	submit(r.benA, 5, "FB-BEN great")
	submit(r.catA, 3, "FB-CAT fine")
	rv, _ := r.s.GetReview(r.ctx, r.mgrA, id)
	if rv.Peer == nil || rv.Peer.Shown || len(rv.Peer.Comments) != 0 || len(rv.Peer.Ratings) != 0 {
		t.Fatalf("manager sees feedback below the threshold: %+v", rv.Peer)
	}
	rv, _ = r.s.GetReview(r.ctx, r.hr, id)
	if rv.Peer == nil || rv.Peer.Shown {
		t.Fatalf("hr sees anonymous feedback below the threshold: %+v", rv.Peer)
	}
	// The third response lifts the threshold.
	submit(r.danA, 4, "FB-DAN ok")
	rv, _ = r.s.GetReview(r.ctx, r.mgrA, id)
	if rv.Peer == nil || !rv.Peer.Shown || rv.Peer.Responses != 3 || len(rv.Peer.Comments) != 3 {
		t.Fatalf("manager feedback = %+v", rv.Peer)
	}
	if got := rv.Peer.Ratings[0]; got.QuestionID != "q1" || got.Average != 4 || got.N != 3 {
		t.Fatalf("aggregate = %+v", got)
	}
	if b, _ := jsonString(rv.Peer); strings.Contains(b, "reviewer") || strings.Contains(b, r.ben.ID) || strings.Contains(b, "time") || strings.Contains(b, "_at") {
		t.Fatalf("anonymous feedback carries attribution: %s", b)
	}
	// The employee sees nothing before release, and aggregates only (no comments) after.
	rv, _ = r.s.GetReview(r.ctx, r.annA, id)
	if rv.Peer != nil {
		t.Fatalf("employee sees peer feedback before release: %+v", rv.Peer)
	}
	// Storage: the anonymous response rows do not name a reviewer.
	var withReviewer int
	r.must(r.s.db.QueryRow(`SELECT COUNT(*) FROM review_responses WHERE reviewer_id IS NOT NULL`).Scan(&withReviewer))
	if withReviewer != 0 {
		t.Fatal("anonymous responses are linked to their authors")
	}
	var sealedText string
	r.must(r.s.db.QueryRow(`SELECT answers_sealed FROM review_responses LIMIT 1`).Scan(&sealedText))
	if !strings.HasPrefix(sealedText, "v1.k1.") {
		t.Fatalf("feedback not sealed: %q", sealedText)
	}
	// The order of comments does not depend on submission order: ids are random.
	if b1, _ := jsonString(rv); strings.Contains(b1, "FB-") {
		t.Fatal("employee view carries feedback text")
	}
}

func TestAttributedFeedbackIsManagerAndHrOnly(t *testing.T) {
	r := newRevEnv(t)
	_, id := r.openCycle("attributed")
	for _, p := range []*Person{r.ben, r.cat, r.dan} {
		_, err := r.s.Nominate(r.ctx, r.mgrA, id, p.ID, "peer")
		r.must(err)
	}
	for i, a := range []Actor{r.benA, r.catA, r.danA} {
		l, _ := r.s.MyFeedbackRequests(r.ctx, a)
		r.must(r.s.SubmitFeedback(r.ctx, a, l[0].ID, Answers{"q1": {Rating: rating(3 + i%2)}, "q2": {Text: "attributed text"}}))
	}
	rv, _ := r.s.GetReview(r.ctx, r.mgrA, id)
	if rv.Peer == nil || !rv.Peer.Shown || len(rv.Peer.Comments) != 3 || rv.Peer.Comments[0].ReviewerID == "" {
		t.Fatalf("manager view of attributed feedback = %+v", rv.Peer)
	}
	rv, _ = r.s.GetReview(r.ctx, r.annA, id)
	if rv.Peer != nil {
		t.Fatal("employee sees feedback before release")
	}
}

func TestUpwardFeedbackNeedsDirectReportsAndAudit(t *testing.T) {
	r := newRevEnv(t)
	tpl, _ := r.s.CreateReviewTemplate(r.ctx, r.hr, TemplateIn{Name: "u", Questions: testQuestions})
	cy, _ := r.s.CreateCycle(r.ctx, r.hr, CycleIn{Name: "u", TemplateID: tpl.ID, Anonymity: "anonymous"})
	ids, err := r.s.AddParticipants(r.ctx, r.hr, cy.ID, []string{r.mgr.ID})
	r.must(err)
	_, err = r.s.SetCycleState(r.ctx, r.hr, cy.ID, "open")
	r.must(err)
	if _, err := r.s.Nominate(r.ctx, r.hr, ids[0], r.boss.ID, "upward"); err == nil {
		t.Fatal("upward feedback requested from someone who is not a direct report")
	}
	n, err := r.s.Nominate(r.ctx, r.hr, ids[0], r.ann.ID, "upward")
	r.must(err)
	if n.Status != "approved" {
		t.Fatalf("hr-added nomination should be approved at once: %+v", n)
	}
	// Reads of review content by hr are audited before the data is returned.
	before := len(r.aud.evs)
	if _, err := r.s.GetReview(r.ctx, r.hr, ids[0]); err != nil {
		t.Fatal(err)
	}
	if r.aud.find("read", "review:"+ids[0], "ok") == nil || len(r.aud.evs) <= before {
		t.Fatal("hr read not audited")
	}
	r.aud.fail = true
	if _, err := r.s.GetReview(r.ctx, r.hr, ids[0]); err == nil {
		t.Fatal("review returned although the audit sink is down")
	}
	r.aud.fail = false
}

func TestReviewGoalsLinkedReadOnly(t *testing.T) {
	r := newRevEnv(t)
	_, id := r.openCycle("attributed")
	g, err := r.s.CreateGoal(r.ctx, r.annA, GoalInput{Scope: "person", Title: "Learn Go", Visibility: "private"})
	r.must(err)
	g2, err := r.s.CreateGoal(r.ctx, r.annA, GoalInput{Scope: "person", Title: "Ship it", Visibility: "manager"})
	r.must(err)
	rv, _ := r.s.GetReview(r.ctx, r.annA, id)
	if len(rv.Goals) != 2 {
		t.Fatalf("subject sees goals %+v", rv.Goals)
	}
	// The manager gets the goals the goal policy lets them see; the private one stays private.
	rv, _ = r.s.GetReview(r.ctx, r.mgrA, id)
	if len(rv.Goals) != 1 || rv.Goals[0].ID != g2.ID {
		t.Fatalf("manager sees goals %+v (private goal %s)", rv.Goals, g.ID)
	}
	var before int
	r.must(r.s.db.QueryRow(`SELECT COUNT(*) FROM goals`).Scan(&before))
	_, _ = r.s.GetReview(r.ctx, r.annA, id)
	var after int
	r.must(r.s.db.QueryRow(`SELECT COUNT(*) FROM goals`).Scan(&after))
	if before != after {
		t.Fatal("reading a review changed goals")
	}
}

func TestReviewErasureRetentionAndLegalHold(t *testing.T) {
	r := newRevEnv(t)
	_, id := r.openCycle("attributed")
	r.must(r.s.SaveSelf(r.ctx, r.annA, id, Answers{"q2": {Text: "ERASE-ME-SELF"}}, true))
	_, err := r.s.Nominate(r.ctx, r.mgrA, id, r.ben.ID, "peer")
	r.must(err)
	l, _ := r.s.MyFeedbackRequests(r.ctx, r.benA)
	r.must(r.s.SubmitFeedback(r.ctx, r.benA, l[0].ID, Answers{"q2": {Text: "ERASE-ME-FB"}}))

	// Erasing Ben (the reviewer) removes what he wrote and his request; Ann's review stays.
	for _, st := range []Status{StOffboarding, StFormer} {
		_, err = r.s.ChangeStatus(r.ctx, r.hr, r.ben.ID, st, "", "")
		r.must(err)
	}
	_, err = r.s.RequestErasure(r.ctx, r.hr, r.ben.ID)
	r.must(err)
	var c int
	r.must(r.s.db.QueryRow(`SELECT COUNT(*) FROM review_responses`).Scan(&c))
	if c != 0 {
		t.Fatalf("erased reviewer's responses remain: %d", c)
	}
	r.must(r.s.db.QueryRow(`SELECT COUNT(*) FROM review_participants`).Scan(&c))
	if c != 1 {
		t.Fatalf("participants = %d", c)
	}
	// Erasing Ann removes her review, with tombstones for the new classes.
	r.must(r.s.SetLegalHold(r.ctx, r.hr, r.ann.ID, true, "dispute"))
	for _, st := range []Status{StOffboarding, StFormer} {
		_, err := r.s.ChangeStatus(r.ctx, r.hr, r.ann.ID, st, "", "")
		r.must(err)
	}
	if _, err := r.s.RequestErasure(r.ctx, r.hr, r.ann.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("erased under legal hold: %v", err)
	}
	r.must(r.s.SetLegalHold(r.ctx, r.hr, r.ann.ID, false, ""))
	er, err := r.s.RequestErasure(r.ctx, r.hr, r.ann.ID)
	r.must(err)
	have := map[string]bool{}
	for _, c := range er.Classes {
		have[c] = true
	}
	if !have[ClassReviews] || !have[ClassTimeOff] {
		t.Fatalf("tombstone classes = %v", er.Classes)
	}
	r.must(r.s.db.QueryRow(`SELECT COUNT(*) FROM review_participants WHERE person_id = ?`, r.ann.ID).Scan(&c))
	if c != 0 {
		t.Fatal("erased person's review remains")
	}
	raw, _ := os.ReadFile(r.path)
	if wal, err := os.ReadFile(r.path + "-wal"); err == nil {
		raw = append(raw, wal...)
	}
	if strings.Contains(string(raw), "ERASE-ME") {
		t.Fatal("erased text is still in the database file or WAL")
	}
}

func TestMigrationOnExistingDatabase(t *testing.T) {
	e := newEnv(t)
	// Simulate a database created before E14b: drop the new objects and the migration record.
	for _, q := range []string{
		`DROP TRIGGER leave_ledger_no_delete`, `DROP TRIGGER leave_ledger_no_update`,
		`DROP TABLE ledger_guard`, `DROP TABLE leave_ledger`, `DROP TABLE leave_requests`, `DROP TABLE blackouts`, `DROP TABLE holidays`,
		`DROP TABLE holiday_calendars`, `DROP TABLE leave_types`, `DROP TABLE review_responses`, `DROP TABLE review_requests`,
		`DROP TABLE review_participants`, `DROP TABLE review_cycles`, `DROP TABLE review_templates`, `DELETE FROM schema_migrations WHERE version = 4`,
	} {
		if _, err := e.s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	p, _ := e.person("Old", "", RoleEmployee)
	e.must(e.s.migrate(e.ctx))
	if _, err := e.s.CreateReviewTemplate(e.ctx, e.hr, TemplateIn{Name: "t", Questions: testQuestions}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.s.GetPerson(e.ctx, e.hr, p.ID); err != nil || got.ID != p.ID {
		t.Fatalf("existing data lost: %v", err)
	}
	// Re-running is a no-op.
	e.must(e.s.migrate(e.ctx))
}
