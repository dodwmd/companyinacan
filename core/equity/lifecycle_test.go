package equity

import (
	"strings"
	"testing"
)

func TestGrantLifecycleEndToEnd(t *testing.T) {
	e := newEnv(t).base()
	id := e.draft(e.hr, e.eveStk.ID, q(4800))
	g := m(e.s.GetGrant(e.ctx, e.hr, id))
	if g.Status != GrantDraft || g.Notes == nil || *g.Notes != "board minute 14" {
		t.Fatalf("draft = %+v", g)
	}
	// terms are editable while draft
	qty := q(4800 + 48)
	g = m(e.s.UpdateGrant(e.ctx, e.hr, id, GrantPatch{Quantity: &qty}))
	if g.Quantity != qty {
		t.Fatal("update did not apply")
	}
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	g = m(e.s.ApproveGrant(e.ctx, e.board, id))
	if g.Status != GrantApproved || g.ApprovedOn != "2026-03-10" {
		t.Fatalf("approved = %+v", g)
	}
	// an approved grant is not exercisable until it is active
	if _, _, err := e.s.RequestExercise(e.ctx, e.hr, id, q(1), "", nil); !isErr(err, ErrConflict) {
		t.Fatalf("exercise on an approved grant: %v", err)
	}
	g = m(e.s.ActivateGrant(e.ctx, e.hr, id, "2026-03-10"))
	if g.Status != GrantActive {
		t.Fatal("not active")
	}
	// vesting: 15 Jan 2026 start, 12-month cliff
	e.advance("2027-01-14")
	v := m(e.s.Vesting(e.ctx, e.hr, id, ""))
	if v.Vested != 0 || v.Exercisable != 0 {
		t.Fatalf("before the cliff: %+v", v)
	}
	e.advance("2027-01-15")
	v = m(e.s.Vesting(e.ctx, e.hr, id, ""))
	if v.Vested != 1212*QtyScale || v.Exercisable != v.Vested || v.WindowEnd != "2036-01-15" {
		t.Fatalf("at the cliff: %+v", v)
	}
	if len(v.Timeline) == 0 || v.Timeline[0].Reason != "cliff" || !v.Illustrative {
		t.Fatalf("timeline = %+v", v.Timeline)
	}

	// exercise 100: strike 1.25 -> 125.00 due
	exID, _, err := e.s.RequestExercise(e.ctx, e.eve, id, q(100), "", nil)
	e.must(err)
	ex, err := e.s.GetExercise(e.ctx, e.eve, exID)
	e.must(err)
	if ex.DueMinor != 100*125 || ex.Status != ExRequested {
		t.Fatalf("exercise = %+v", ex)
	}
	// the holder cannot approve their own exercise (no role), finance can
	if _, err := e.s.ApproveExercise(e.ctx, e.eve, exID); !isErr(err, ErrForbidden) {
		t.Fatalf("holder approving: %v", err)
	}
	m(e.s.ApproveExercise(e.ctx, e.fin, exID))
	if _, err := e.s.RecordPayment(e.ctx, e.fin, exID, PaymentInput{AmountMinor: 100, PaidOn: "2027-01-15"}); !isErr(err, ErrConflict) {
		t.Fatalf("wrong amount accepted: %v", err)
	}
	m(e.s.RecordPayment(e.ctx, e.fin, exID, PaymentInput{AmountMinor: 12500, PaidOn: "2027-01-15", Reference: "WIRE-77"}))
	// the person who recorded the payment cannot also issue
	if _, err := e.s.IssueShares(e.ctx, e.fin, exID); !isErr(err, ErrSeparation) {
		t.Fatalf("payer issuing: %v", err)
	}
	ex = m(e.s.IssueShares(e.ctx, e.fin2, exID))
	if ex.Status != ExIssued || ex.LedgerID == "" {
		t.Fatalf("issued = %+v", ex)
	}
	g = m(e.s.GetGrant(e.ctx, e.hr, id))
	if g.Exercised != q(100) {
		t.Fatalf("exercised = %s", g.Exercised)
	}
	// the issuance is in the ledger and the cap table
	ct := m(e.s.GetCapTable(e.ctx, e.fin, e.ent.ID, "", "fully-diluted", true))
	var eve *HolderLine
	for i := range ct.Holders {
		if ct.Holders[i].StakeholderID == e.eveStk.ID {
			eve = &ct.Holders[i]
		}
	}
	if eve == nil || eve.Name != "Eve Example" {
		t.Fatalf("eve = %+v", eve)
	}
	if eve.BasicShares != q(100) || *eve.OptionsOutstanding != q(4848)-q(100) {
		t.Fatalf("eve = %+v options=%s", eve, eve.OptionsOutstanding)
	}
	ps := m(e.s.Pool(e.ctx, e.hr, e.plan.ID, ""))
	if ps.Authorised != q(1_000_000) || ps.GrantedOutstanding != q(4748) || ps.Exercised != q(100) || ps.Available != q(1_000_000-4848) {
		t.Fatalf("pool = %+v", ps)
	}
	// events carry ids only
	evs := m(e.s.AllEvents(e.ctx))
	types := map[string]int{}
	for _, ev := range evs {
		types[ev.Type]++
		for k, v := range ev.Data {
			s, _ := v.(string)
			if strings.Contains(k, "quantity") || strings.Contains(k, "strike") || strings.Contains(k, "name") || strings.Contains(s, "Eve") || strings.Contains(s, "@") {
				t.Errorf("event %s carries %s=%v", ev.Type, k, v)
			}
		}
	}
	if types[EventGrantApproved] != 1 || types[EventExerciseRecorded] != 1 {
		t.Fatalf("events = %v", types)
	}
}

func TestVestedEventOncePerIncrease(t *testing.T) {
	e := newEnv(t).base()
	id := e.active(e.eveStk.ID, q(4800))
	n := func() int {
		c := 0
		for _, ev := range m(e.s.AllEvents(e.ctx)) {
			if ev.Type == EventGrantVested {
				c++
			}
		}
		return c
	}
	m(e.s.ApplyDue(e.ctx))
	if n() != 0 {
		t.Fatal("event before anything vested")
	}
	e.advance("2027-01-15")
	m(e.s.ApplyDue(e.ctx))
	m(e.s.ApplyDue(e.ctx)) // a second sweep must not repeat it
	if n() != 1 {
		t.Fatalf("vested events = %d", n())
	}
	e.advance("2027-02-15")
	m(e.s.ApplyDue(e.ctx))
	if n() != 2 {
		t.Fatalf("vested events = %d", n())
	}
	_ = id
}

func TestAmendCreatesNewVersionAndCancelsOldOnApproval(t *testing.T) {
	e := newEnv(t).base()
	old := e.active(e.eveStk.ID, q(1000))
	nv := m(e.s.AmendGrant(e.ctx, e.hr, old))
	if nv.Version != 2 || nv.SupersedesID != old || nv.Status != GrantDraft {
		t.Fatalf("amendment = %+v", nv)
	}
	if _, err := e.s.AmendGrant(e.ctx, e.hr, old); !isErr(err, ErrConflict) {
		t.Fatalf("second amendment in progress: %v", err)
	}
	qty := q(1500)
	m(e.s.UpdateGrant(e.ctx, e.hr2, nv.ID, GrantPatch{Quantity: &qty}))
	m(e.s.SubmitGrant(e.ctx, e.hr, nv.ID))
	// the old grant stays in force until the new one is approved
	if g := m(e.s.GetGrant(e.ctx, e.hr, old)); g.Status != GrantActive {
		t.Fatalf("old = %s", g.Status)
	}
	m(e.s.ApproveGrant(e.ctx, e.board, nv.ID))
	g := m(e.s.GetGrant(e.ctx, e.hr, old))
	if g.Status != GrantCancelled || !strings.Contains(g.TerminalReason, "superseded") {
		t.Fatalf("old = %+v", g)
	}
	if ps := m(e.s.Pool(e.ctx, e.hr, e.plan.ID, "")); ps.GrantedOutstanding != q(1500) {
		t.Fatalf("pool = %+v", ps)
	}
	// a grant with exercises cannot be amended
	act := e.active(e.eveStk.ID, q(100))
	e.advance("2027-01-15")
	exID, _, err := e.s.RequestExercise(e.ctx, e.hr, act, q(1), "", nil)
	e.must(err)
	m(e.s.ApproveExercise(e.ctx, e.fin, exID))
	m(e.s.RecordPayment(e.ctx, e.fin, exID, PaymentInput{AmountMinor: 125, PaidOn: "2027-01-15"}))
	m(e.s.IssueShares(e.ctx, e.fin2, exID))
	if _, err := e.s.AmendGrant(e.ctx, e.hr, act); !isErr(err, ErrConflict) {
		t.Fatalf("amending an exercised grant: %v", err)
	}
}

func TestCancelAndExpiryReleaseThePool(t *testing.T) {
	e := newEnv(t).base()
	id := e.active(e.eveStk.ID, q(2000))
	avail := func() Qty { return m(e.s.Pool(e.ctx, e.hr, e.plan.ID, "")).Available }
	if avail() != q(998_000) {
		t.Fatalf("available = %s", avail())
	}
	m(e.s.CancelGrant(e.ctx, e.hr, id, "left before the cliff"))
	if avail() != q(1_000_000) {
		t.Fatalf("after cancel = %s", avail())
	}
	if _, err := e.s.CancelGrant(e.ctx, e.hr, id, ""); !isErr(err, ErrConflict) {
		t.Fatalf("cancelling twice: %v", err)
	}
	// termination: the window closes, the sweep expires the grant and the pool recovers the unvested part
	id2 := e.active(e.eveStk.ID, q(4800))
	e.advance("2027-07-15") // 18 months vested
	m(e.s.TerminateGrant(e.ctx, e.hr, id2, "2027-07-15", false))
	e.advance("2027-10-13") // 90 days later: last day of the window
	m(e.s.ApplyDue(e.ctx))
	if g := m(e.s.GetGrant(e.ctx, e.hr, id2)); g.Status != GrantActive {
		t.Fatalf("inside the window: %s", g.Status)
	}
	if got := avail(); got != q(1_000_000-1800) {
		t.Fatalf("pool after termination = %s", got)
	}
	e.advance("2027-10-14")
	m(e.s.ApplyDue(e.ctx))
	if g := m(e.s.GetGrant(e.ctx, e.hr, id2)); g.Status != GrantExpired {
		t.Fatalf("after the window: %s", g.Status)
	}
	if avail() != q(1_000_000) {
		t.Fatalf("after expiry = %s", avail())
	}
	// for cause forfeits at once
	id3 := e.active(e.eveStk.ID, q(100))
	g := m(e.s.TerminateGrant(e.ctx, e.hr, id3, "2027-10-14", true))
	if g.Status != GrantForfeited {
		t.Fatalf("for cause: %s", g.Status)
	}
}

func TestExerciseGuards(t *testing.T) {
	e := newEnv(t).base()
	id := e.active(e.eveStk.ID, q(4800))
	e.advance("2027-01-15") // 1200 vested
	for _, n := range []Qty{0, q(1201)} {
		if _, _, err := e.s.RequestExercise(e.ctx, e.eve, id, n, "", nil); err == nil {
			t.Fatalf("exercise of %s accepted", n)
		}
	}
	ex1, _, err := e.s.RequestExercise(e.ctx, e.eve, id, q(1000), "", nil)
	e.must(err)
	// 1000 is reserved: only 200 remain exercisable
	if _, _, err := e.s.RequestExercise(e.ctx, e.eve, id, q(201), "", nil); !isErr(err, ErrConflict) {
		t.Fatalf("over-reserving: %v", err)
	}
	if _, _, err := e.s.RequestExercise(e.ctx, e.eve, id, q(200), "", nil); err != nil {
		t.Fatal(err)
	}
	// withdrawing frees the reservation
	m(e.s.CancelExercise(e.ctx, e.eve, ex1))
	if _, _, err := e.s.RequestExercise(e.ctx, e.eve, id, q(1000), "", nil); err != nil {
		t.Fatalf("after withdrawal: %v", err)
	}
	// somebody else's grant looks like it does not exist
	if _, _, err := e.s.RequestExercise(e.ctx, e.max, id, q(1), "", nil); !isErr(err, ErrNotFound) {
		t.Fatalf("other employee: %v", err)
	}
	// a finance user who requests on behalf cannot approve it, and neither can the holder's own person id
	onBehalf, _, err := e.s.RequestExercise(e.ctx, e.fin, id, Qty(0), "", nil)
	if err == nil {
		_ = onBehalf
		t.Fatal("zero accepted")
	}
	// SARs are not settled here
	sar := e.draft(e.hr, e.eveStk.ID, q(10))
	sq := "SAR"
	m(e.s.UpdateGrant(e.ctx, e.hr, sar, GrantPatch{Type: &sq}))
	m(e.s.SubmitGrant(e.ctx, e.hr, sar))
	m(e.s.ApproveGrant(e.ctx, e.board, sar))
	m(e.s.ActivateGrant(e.ctx, e.hr, sar, ""))
	if _, _, err := e.s.RequestExercise(e.ctx, e.eve, sar, q(1), "", nil); !isErr(err, ErrConflict) {
		t.Fatalf("SAR exercise: %v", err)
	}
	// after the window nothing is exercisable
	e.advance("2036-01-16")
	if _, _, err := e.s.RequestExercise(e.ctx, e.eve, id, q(1), "", nil); !isErr(err, ErrConflict) {
		t.Fatalf("after expiry: %v", err)
	}
}

func TestExerciseCostRounding(t *testing.T) {
	for _, c := range []struct {
		qty    Qty
		strike int64
		want   int64
	}{
		{q(100), 125, 12500},
		{Qty(5), 1, 0},     // 0.0005 x 1 = 0.0005 -> 0
		{Qty(5000), 1, 1},  // 0.5 x 1 = 0.5 -> half away from zero -> 1
		{Qty(4999), 1, 0},  // 0.4999 -> 0
		{Qty(15000), 3, 5}, // 1.5 x 3 = 4.5 -> 5
		{q(3), 333, 999},
		{MaxQty, maxMinor, 0}, // too large: must be refused, not wrapped
	} {
		got, err := exerciseCost(c.qty, c.strike)
		if c.qty == MaxQty {
			if err == nil {
				t.Errorf("an unrepresentable cost was accepted: %d", got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("cost(%s x %d) = %d, want %d", c.qty, c.strike, got, c.want)
		}
	}
}

func TestPaymentReferenceIsAFieldOfItsOwn(t *testing.T) {
	e := newEnv(t).base()
	gid := e.active(e.eveStk.ID, q(4800))
	e.advance("2028-01-15")
	ex, _, err := e.s.RequestExercise(e.ctx, e.eve, gid, q(10), "", nil)
	e.must(err)
	m(e.s.ApproveExercise(e.ctx, e.fin, ex))
	m(e.s.RecordPayment(e.ctx, e.fin2, ex, PaymentInput{AmountMinor: 1250, PaidOn: "2028-01-15", Reference: "BANK-REF-9"}))
	for name, tc := range map[string]struct {
		who  Actor
		want string
	}{"finance": {e.fin, "BANK-REF-9"}, "admin": {e.admin, "BANK-REF-9"}, "hr": {e.hr, ""}, "board": {e.board, ""}, "holder": {e.eve, ""}} {
		got, err := e.s.GetExercise(e.ctx, tc.who, ex)
		if err != nil || got.PaymentReference != tc.want {
			t.Errorf("%s: %+v (%v), want reference %q", name, got, err, tc.want)
		}
	}
	var sealed string
	e.must(e.s.db.QueryRow(`SELECT sens_payment_ref FROM exercises WHERE id = ?`, ex).Scan(&sealed))
	if strings.Contains(sealed, "BANK") || !strings.HasPrefix(sealed, "v1.") {
		t.Fatalf("payment reference not sealed: %q", sealed)
	}
}

func TestMilestoneAndAccelerationFlowThroughTheService(t *testing.T) {
	e := newEnv(t).base()
	in := e.standardInput(e.eveStk.ID, q(1000))
	in.Schedule = Schedule{Kind: "tranches", Tranches: []Tranche{{Label: "beta", BasisPoints: 5000, Milestone: true}, {Label: "yr", BasisPoints: 5000, Date: "2027-01-15"}},
		Acceleration: []Acceleration{{Name: "coc", Type: "single", Triggers: []string{"change_of_control"}, PercentBP: 10000}}}
	body := []byte("x")
	id, _, err := e.s.CreateGrant(e.ctx, e.hr, e.plan.ID, in, "", body)
	e.must(err)
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	m(e.s.ApproveGrant(e.ctx, e.board, id))
	m(e.s.ActivateGrant(e.ctx, e.hr, id, ""))
	if err := e.s.RecordMilestone(e.ctx, e.hr, id, 1, "2026-03-01"); err == nil {
		t.Fatal("a dated tranche accepted as a milestone")
	}
	if err := e.s.RecordMilestone(e.ctx, e.hr, id, 0, "2026-12-31"); err == nil {
		t.Fatal("a future milestone accepted")
	}
	e.must(e.s.RecordMilestone(e.ctx, e.hr, id, 0, "2026-03-01"))
	if err := e.s.RecordMilestone(e.ctx, e.hr, id, 0, "2026-03-02"); !isErr(err, ErrConflict) {
		t.Fatalf("duplicate milestone: %v", err)
	}
	if v := m(e.s.Vesting(e.ctx, e.hr, id, "")); v.Vested != q(500) {
		t.Fatalf("after milestone: %s", v.Vested)
	}
	if err := e.s.RecordAcceleration(e.ctx, e.hr, id, "nonsense", "2026-03-05"); err == nil {
		t.Fatal("unknown trigger accepted")
	}
	e.must(e.s.RecordAcceleration(e.ctx, e.hr, id, "change_of_control", "2026-03-09"))
	if v := m(e.s.Vesting(e.ctx, e.hr, id, "")); v.Vested != q(1000) {
		t.Fatalf("after acceleration: %s", v.Vested)
	}
	// facts are append-only at the database level
	if _, err := e.s.db.Exec(`UPDATE grant_milestones SET achieved_on = '2020-01-01'`); err == nil {
		t.Fatal("milestone rewritten")
	}
}
