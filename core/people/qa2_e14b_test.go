package people

import (
	"strings"
	"testing"
)

// Regression tests for E14b QA round 2.

func TestFeedbackThresholdAppliesPerQuestion(t *testing.T) {
	r := newRevEnv(t)
	_, id := r.openCycle("anonymous")
	for _, p := range []*Person{r.ben, r.cat, r.dan} {
		_, err := r.s.Nominate(r.ctx, r.mgrA, id, p.ID, "peer")
		r.must(err)
	}
	answer := func(a Actor, ans Answers) {
		l, _ := r.s.MyFeedbackRequests(r.ctx, a)
		r.must(r.s.SubmitFeedback(r.ctx, a, l[0].ID, ans))
	}
	// One peer answers only q1 (a rating), two answer only q2 (text): each question has fewer than 3 answers.
	answer(r.benA, Answers{"q1": {Rating: rating(1)}})
	answer(r.catA, Answers{"q2": {Text: "PARTIAL-CAT"}})
	answer(r.danA, Answers{"q2": {Text: "PARTIAL-DAN"}})
	_, err := r.s.SetCycleState(r.ctx, r.hr, r.cycle.ID, "calibration")
	r.must(err)
	for name, a := range map[string]Actor{"manager": r.mgrA, "hr": r.hr} {
		rv, _ := r.s.GetReview(r.ctx, a, id)
		b, _ := jsonString(rv.Peer)
		if rv.Peer == nil || !rv.Peer.Shown || len(rv.Peer.Ratings) != 0 || len(rv.Peer.Comments) != 0 || strings.Contains(b, "PARTIAL") {
			t.Fatalf("%s sees a question answered by fewer than 3 people: %s", name, b)
		}
	}
}

func TestFeedbackShowsOnlyQuestionsWithEnoughAnswers(t *testing.T) {
	r := newRevEnv(t)
	_, id := r.openCycle("anonymous")
	for _, p := range []*Person{r.ben, r.cat, r.dan} {
		_, err := r.s.Nominate(r.ctx, r.mgrA, id, p.ID, "peer")
		r.must(err)
	}
	for i, a := range []Actor{r.benA, r.catA, r.danA} {
		l, _ := r.s.MyFeedbackRequests(r.ctx, a)
		ans := Answers{"q2": {Text: "ALL-THREE"}}
		if i == 0 {
			ans["q1"] = Answer{Rating: rating(1)} // only one person rated
		}
		r.must(r.s.SubmitFeedback(r.ctx, a, l[0].ID, ans))
	}
	_, err := r.s.SetCycleState(r.ctx, r.hr, r.cycle.ID, "calibration")
	r.must(err)
	rv, _ := r.s.GetReview(r.ctx, r.mgrA, id)
	if len(rv.Peer.Ratings) != 0 || len(rv.Peer.Comments) != 3 {
		t.Fatalf("view = %+v", rv.Peer)
	}
}

func TestUpwardFeedbackThresholdIsFiveAndPerQuestion(t *testing.T) {
	r := newRevEnv(t)
	tpl, _ := r.s.CreateReviewTemplate(r.ctx, r.hr, TemplateIn{Name: "u", Questions: testQuestions})
	cy, _ := r.s.CreateCycle(r.ctx, r.hr, CycleIn{Name: "u", TemplateID: tpl.ID, Anonymity: "anonymous"})
	ids, err := r.s.AddParticipants(r.ctx, r.hr, cy.ID, []string{r.mgr.ID})
	r.must(err)
	_, err = r.s.SetCycleState(r.ctx, r.hr, cy.ID, "open")
	r.must(err)
	reports := []Actor{r.annA, r.benA, r.catA, r.danA, r.eveA}
	for _, p := range []*Person{r.ann, r.ben, r.cat, r.dan, r.eve} {
		_, err := r.s.Nominate(r.ctx, r.hr, ids[0], p.ID, "upward")
		r.must(err)
	}
	for i, a := range reports[:4] { // four of five answer: below the upward threshold of 5
		l, _ := r.s.MyFeedbackRequests(r.ctx, a)
		r.must(r.s.SubmitFeedback(r.ctx, a, l[0].ID, Answers{"q1": {Rating: rating(1 + i)}, "q2": {Text: "UP-TEXT"}}))
	}
	l, _ := r.s.MyFeedbackRequests(r.ctx, reports[4])
	r.must(r.s.SubmitFeedback(r.ctx, reports[4], l[0].ID, Answers{"q2": {Text: "UP-TEXT"}})) // fifth answers q2 only
	_, err = r.s.SetCycleState(r.ctx, r.hr, cy.ID, "calibration")
	r.must(err)
	rv, err := r.s.GetReview(r.ctx, r.bossA, ids[0]) // the manager's manager reads upward feedback about them
	r.must(err)
	_ = rv
	rv, err = r.s.GetReview(r.ctx, r.hr, ids[0])
	r.must(err)
	if rv.Upward == nil || !rv.Upward.Shown || len(rv.Upward.Ratings) != 0 || len(rv.Upward.Comments) != 5 {
		t.Fatalf("upward view = %+v (q1 was answered by 4 < 5, q2 by 5)", rv.Upward)
	}
}

func TestCancellingFutureLeaveRestoresTheBalanceAsOfToday(t *testing.T) {
	x := newToEnv(t)
	lr := x.submit(x.annA, x.vac, "2026-04-06", "2026-04-10")
	_, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true)
	x.must(err)
	_, err = x.s.CancelLeaveRequest(x.ctx, x.annA, lr.ID)
	x.must(err)
	for _, asOf := range []string{"", "2026-03-10", "2026-04-06", "2026-12-31"} {
		if got := x.balance(x.annA, x.ann.ID, x.vac, asOf); got != 50 {
			t.Fatalf("as of %q the balance is %d, want 50 (the reversal must be dated with the use)", asOf, got)
		}
	}
}

func TestSkipLevelReviewListIsNotStarvedByTheLimit(t *testing.T) {
	r := newRevEnv(t)
	for i := 0; i < 1010; i++ {
		cid := newID("cyc")
		_, err := r.s.db.Exec(`INSERT INTO review_cycles (id, name, state, questions, anonymity, min_responses, rating_max, created_by, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			cid, "c-"+cid, "open", "[]", "anonymous", 3, 5, "t", 1)
		r.must(err)
		_, err = r.s.db.Exec(`INSERT INTO review_participants (id, cycle_id, person_id, created_at, updated_at) VALUES (?,?,?,?,?)`, newID("rev"), cid, r.ann.ID, 1, 1)
		r.must(err)
	}
	_, id := r.openCycle("attributed") // ann again, in a cycle created last
	r.must(r.s.SaveManager(r.ctx, r.mgrA, id, ManagerIn{Rating: rating(3), Summary: "ok", Submit: true}))
	_, err := r.s.db.Exec(`UPDATE review_participants SET release_state = 'released', released_at = 5 WHERE id = ?`, id)
	r.must(err)
	l, err := r.s.ListReviews(r.ctx, r.bossA, "") // boss is the skip-level: unreleased reviews are none of their business
	r.must(err)
	if len(l) != 1 || l[0].ID != id {
		t.Fatalf("skip-level list = %d entries", len(l))
	}
}
