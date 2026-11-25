package people

import (
	"errors"
	"strings"
	"testing"
)

// Regression tests for the E14b QA round.

func TestResponsesTableHasNoInsertionOrderAndAnonymousAuditNamesNobody(t *testing.T) {
	r := newRevEnv(t)
	var ddl string
	r.must(r.s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'review_responses'`).Scan(&ddl))
	if !strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID") {
		t.Fatalf("review_responses keeps insertion order (rowid): %s", ddl)
	}
	_, id := r.openCycle("anonymous")
	_, err := r.s.Nominate(r.ctx, r.mgrA, id, r.ben.ID, "peer")
	r.must(err)
	l, _ := r.s.MyFeedbackRequests(r.ctx, r.benA)
	r.must(r.s.SubmitFeedback(r.ctx, r.benA, l[0].ID, Answers{"q1": {Rating: rating(4)}}))
	for _, e := range r.aud.evs {
		if strings.HasPrefix(e.Resource, "feedback-request:") || (e.Actor == r.ben.ID && e.Action == "write") {
			t.Fatalf("the audit trail links the reviewer to the answer: %+v", e)
		}
	}
	found := false
	for _, e := range r.aud.evs {
		if e.Resource == "review-cycle:"+r.cycle.ID && e.Fields != nil && e.Fields[0] == "feedback" {
			found = true
		}
	}
	if !found {
		t.Fatal("no coarse audit entry for the answer")
	}
}

func TestCancellationCreditsWhatWasDebitedWhateverHappenedToTheCalendar(t *testing.T) {
	// Direction 1: a holiday is added after approval. The old recomputation credited 8 back for a 10 debit.
	x := newToEnv(t)
	_, err := x.s.UpdatePerson(x.ctx, x.hr, x.ann.ID, PersonPatch{Location: sp("Berlin")})
	x.must(err)
	lr := x.submit(x.annA, x.vac, "2026-04-06", "2026-04-10")
	_, err = x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true)
	x.must(err)
	_, err = x.s.SaveCalendar(x.ctx, x.hr, CalendarIn{Location: "Berlin", Name: "DE", Holidays: []Holiday{{Date: "2026-04-08", Name: "Example Day"}}})
	x.must(err)
	_, err = x.s.CancelLeaveRequest(x.ctx, x.annA, lr.ID)
	x.must(err)
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2026-12-31"); got != 50 {
		t.Fatalf("holiday added after approval: balance %d, want 50", got)
	}
	// Direction 2: a holiday is removed after approval. The old code minted 2 extra halves.
	_, err = x.s.SaveCalendar(x.ctx, x.hr, CalendarIn{Location: "Berlin", Name: "DE", Holidays: []Holiday{{Date: "2026-05-13", Name: "Example Day"}}})
	x.must(err)
	l2 := x.submit(x.annA, x.vac, "2026-05-11", "2026-05-15")
	l2, err = x.s.DecideLeaveRequest(x.ctx, x.mgrA, l2.ID, true)
	x.must(err)
	if l2.Halves != 8 {
		t.Fatalf("halves = %d", l2.Halves)
	}
	_, err = x.s.SaveCalendar(x.ctx, x.hr, CalendarIn{Location: "Berlin", Name: "DE"})
	x.must(err)
	_, err = x.s.CancelLeaveRequest(x.ctx, x.annA, l2.ID)
	x.must(err)
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2026-12-31"); got != 50 {
		t.Fatalf("holiday removed after approval: balance %d, want 50 (credit must not exceed the debit)", got)
	}
	// Partly taken: only the stored slots from today on come back.
	_, err = x.s.SaveCalendar(x.ctx, x.hr, CalendarIn{Location: "Berlin", Name: "DE", Holidays: []Holiday{{Date: "2026-03-12", Name: "Example Day"}}})
	x.must(err)
	l3 := x.submit(x.annA, x.vac, "2026-03-09", "2026-03-13") // today is Tue 03-10; Thu is a holiday: 8 halves
	_, err = x.s.DecideLeaveRequest(x.ctx, x.mgrA, l3.ID, true)
	x.must(err)
	_, err = x.s.SaveCalendar(x.ctx, x.hr, CalendarIn{Location: "Berlin", Name: "DE"})
	x.must(err)
	_, err = x.s.CancelLeaveRequest(x.ctx, x.hr, l3.ID)
	x.must(err)
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2026-12-31"); got != 50-8+6 { // Mon used; Tue, Wed, Fri return
		t.Fatalf("partial credit: balance %d, want 48", got)
	}
	var back int
	x.must(x.s.db.QueryRow(`SELECT COALESCE(SUM(halves),0) FROM leave_ledger WHERE kind = 'reversal' AND ref = ?`, l3.ID).Scan(&back))
	if back != 6 {
		t.Fatalf("reversal = %d", back)
	}
}

func TestCycleListIsFilteredBeforeTheLimit(t *testing.T) {
	r := newRevEnv(t)
	for i := 0; i < 520; i++ {
		_, err := r.s.db.Exec(`INSERT INTO review_cycles (id, name, state, questions, anonymity, min_responses, rating_max, created_by, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			newID("cyc"), "other-"+newID("n"), "draft", "[]", "anonymous", 3, 5, "t", 1)
		r.must(err)
	}
	_, id := r.openCycle("attributed")
	l, err := r.s.ListCycles(r.ctx, r.annA)
	r.must(err)
	if len(l) != 1 {
		t.Fatalf("ann sees %d cycles", len(l))
	}
	rs, err := r.s.ListReviews(r.ctx, r.mgrA, "")
	r.must(err)
	if len(rs) != 1 || rs[0].ID != id {
		t.Fatalf("manager sees %+v", rs)
	}
}

func TestErasureDropsIdentityHashesAndTimeOffEvents(t *testing.T) {
	x := newToEnv(t)
	_, id := x.openCycle("attributed")
	x.must(x.s.SaveManager(x.ctx, x.mgrA, id, ManagerIn{Rating: rating(3), Summary: "ok", Submit: true}))
	lr := x.submit(x.benA, x.vac, "2026-04-06", "2026-04-07")
	_, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true)
	x.must(err)
	var touched string
	x.must(x.s.db.QueryRow(`SELECT touched_by FROM review_participants WHERE id = ?`, id).Scan(&touched))
	if !strings.Contains(touched, hashID(x.mgr.ID)) {
		t.Fatal("the manager is not in the history")
	}
	// Erase Ben (leave events) and the manager (history hashes); both become former first.
	for _, p := range []*Person{x.ben, x.mgr} {
		for _, st := range []Status{StOffboarding, StFormer} {
			_, err := x.s.ChangeStatus(x.ctx, x.hr, p.ID, st, "", "")
			x.must(err)
		}
		_, err := x.s.RequestErasure(x.ctx, x.hr, p.ID)
		x.must(err)
	}
	x.must(x.s.db.QueryRow(`SELECT touched_by FROM review_participants WHERE id = ?`, id).Scan(&touched))
	if strings.Contains(touched, hashID(x.mgr.ID)) || strings.Contains(touched, hashID("sso-mgr")) {
		t.Fatalf("an erased person's identity hash survives: %s", touched)
	}
	evs, _ := x.s.AllEvents(x.ctx)
	for _, ev := range evs {
		if (ev.Type == EventTimeOffApproved || ev.Type == EventTimeOffCancelled) && ev.Data["person_id"] == x.ben.ID {
			t.Fatalf("outbox still holds an absence of an erased person: %+v", ev)
		}
	}
}

func TestAdjustmentReasonsAreForThePersonAndHrOnly(t *testing.T) {
	x := newToEnv(t)
	_, err := x.s.AdjustLeave(x.ctx, x.hr, x.ann.ID, x.vac.ID, 2, "REASON-TEXT")
	x.must(err)
	for name, a := range map[string]Actor{"self": x.annA, "hr": x.hr} {
		es, err := x.s.LeaveLedger(x.ctx, a, x.ann.ID, x.vac.ID)
		x.must(err)
		ok := false
		for _, e := range es {
			ok = ok || e.Note == "REASON-TEXT"
		}
		if !ok {
			t.Errorf("%s cannot read the reason", name)
		}
	}
	es, err := x.s.LeaveLedger(x.ctx, x.mgrA, x.ann.ID, x.vac.ID)
	x.must(err)
	for _, e := range es {
		if e.Note != "" {
			t.Fatalf("the manager reads an adjustment reason: %+v", e)
		}
	}
	if _, err := x.s.LeaveLedger(x.ctx, x.benA, x.ann.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("colleague: %v", err)
	}
}

func TestSensitiveLeaveTypesAreTaggedInTheAudit(t *testing.T) {
	x := newToEnv(t)
	lr := x.submit(x.annA, x.sick, "2026-03-23", "2026-03-23")
	_, err := x.s.GetLeaveRequest(x.ctx, x.mgrA, lr.ID)
	x.must(err)
	tagged := false
	for _, e := range x.aud.evs {
		if e.Action == "read" && e.Resource == "leave-request:"+lr.ID {
			for _, f := range e.Fields {
				tagged = tagged || f == "sensitive-type"
			}
		}
	}
	if !tagged {
		t.Fatal("a read of a sensitive-type request is not tagged")
	}
}
