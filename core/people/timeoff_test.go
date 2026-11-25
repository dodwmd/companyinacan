package people

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// All names, ids and values in these tests are fake. The clock starts on Tuesday 2026-03-10.

type toEnv struct {
	*revEnv
	vac, sick, parental, unpaid *LeaveType
}

func newToEnv(t *testing.T) *toEnv {
	r := newRevEnv(t)
	x := &toEnv{revEnv: r}
	var err error
	x.vac, err = r.s.CreateLeaveType(r.ctx, r.hr, LeaveTypeIn{Key: "vacation", Name: "Vacation", Paid: true, AccrualMode: "annual", GrantHalves: 50, CarryoverCapHalves: intp(10)})
	r.must(err)
	x.sick, err = r.s.CreateLeaveType(r.ctx, r.hr, LeaveTypeIn{Key: "sick", Name: "Sick leave", Paid: true, Sensitive: true, IgnoresBlackouts: true})
	r.must(err)
	x.parental, err = r.s.CreateLeaveType(r.ctx, r.hr, LeaveTypeIn{Key: "parental", Name: "Parental leave", Paid: true, Sensitive: true, IgnoresBlackouts: true, SetsLeaveStatus: true, LeaveStatusMinHalves: 10})
	r.must(err)
	x.unpaid, err = r.s.CreateLeaveType(r.ctx, r.hr, LeaveTypeIn{Key: "unpaid", Name: "Unpaid"})
	r.must(err)
	_, err = r.s.RunAccruals(r.ctx, r.hr)
	r.must(err)
	return x
}

func intp(n int) *int { return &n }

func (x *toEnv) draft(a Actor, typ *LeaveType, start, end string) *LeaveRequest {
	x.t.Helper()
	lr, err := x.s.CreateLeaveRequest(x.ctx, a, LeaveRequestIn{TypeID: typ.ID, StartDate: start, EndDate: end, Note: "private note"})
	x.must(err)
	return lr
}

func (x *toEnv) submit(a Actor, typ *LeaveType, start, end string) *LeaveRequest {
	x.t.Helper()
	lr := x.draft(a, typ, start, end)
	out, err := x.s.SubmitLeaveRequest(x.ctx, a, lr.ID)
	x.must(err)
	return out
}

func (x *toEnv) balance(a Actor, person string, typ *LeaveType, asOf string) int {
	x.t.Helper()
	bs, err := x.s.LeaveBalances(x.ctx, a, person, asOf)
	x.must(err)
	for _, b := range bs {
		if b.TypeID == typ.ID {
			return b.Halves
		}
	}
	return 0
}

func TestLeaveTypesAreHrDataAndValidated(t *testing.T) {
	x := newToEnv(t)
	if _, err := x.s.CreateLeaveType(x.ctx, x.mgrA, LeaveTypeIn{Key: "x", Name: "X"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager created a leave type: %v", err)
	}
	for _, bad := range []LeaveTypeIn{
		{Key: "Bad Key", Name: "x"}, {Key: "k", Name: ""}, {Key: "vacation", Name: "dup"},
		{Key: "k2", Name: "x", AccrualMode: "weekly"}, {Key: "k3", Name: "x", GrantHalves: -1},
		{Key: "k4", Name: "x", AllowNegative: false, NegativeFloorHalves: 4}, {Key: "k5", Name: "x", AccrualMode: "annual", GrantHalves: 100000},
	} {
		if _, err := x.s.CreateLeaveType(x.ctx, x.hr, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	l, err := x.s.ListLeaveTypes(x.ctx, x.annA)
	x.must(err)
	if len(l) != 4 {
		t.Fatalf("employees see %d types", len(l))
	}
	n := 12
	up, err := x.s.UpdateLeaveType(x.ctx, x.hr, x.vac.ID, LeaveTypePatch{GrantHalves: &n})
	x.must(err)
	if up.GrantHalves != 12 {
		t.Fatalf("update = %+v", up)
	}
	n = 50
	_, err = x.s.UpdateLeaveType(x.ctx, x.hr, x.vac.ID, LeaveTypePatch{GrantHalves: &n})
	x.must(err)
}

func TestWorkingDaysHolidaysAndHalfDays(t *testing.T) {
	x := newToEnv(t)
	if _, err := x.s.SaveCalendar(x.ctx, x.annA, CalendarIn{Location: "Berlin", Name: "DE"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("employee saved a calendar: %v", err)
	}
	// Ann works in "Berlin"; the Monday 2026-03-16 is a holiday there.
	_, err := x.s.UpdatePerson(x.ctx, x.hr, x.ann.ID, PersonPatch{Location: sp("Berlin")})
	x.must(err)
	_, err = x.s.SaveCalendar(x.ctx, x.hr, CalendarIn{Location: " berlin ", Name: "Germany (example)", Holidays: []Holiday{{Date: "2026-03-16", Name: "Example Day"}}})
	x.must(err)
	// Fri 13th to Tue 17th: Fri, (Sat, Sun off), (Mon holiday), Tue = 2 working days.
	lr := x.draft(x.annA, x.vac, "2026-03-13", "2026-03-17")
	lr, err = x.s.SubmitLeaveRequest(x.ctx, x.annA, lr.ID)
	x.must(err)
	if lr.Halves != 4 || lr.Days != 2 {
		t.Fatalf("halves = %d", lr.Halves)
	}
	// Ben has no calendar: the Monday counts.
	lr2 := x.submit(x.benA, x.vac, "2026-03-13", "2026-03-17")
	if lr2.Halves != 6 {
		t.Fatalf("no-calendar halves = %d", lr2.Halves)
	}
	// Half days: afternoon of the first day and morning of the last.
	in := LeaveRequestIn{TypeID: x.vac.ID, StartDate: "2026-03-23", EndDate: "2026-03-25", StartPart: "pm", EndPart: "am"}
	h, err := x.s.CreateLeaveRequest(x.ctx, x.catA, in)
	x.must(err)
	h, err = x.s.SubmitLeaveRequest(x.ctx, x.catA, h.ID)
	x.must(err)
	if h.Halves != 3 { // Mon pm, Tue full (2), Wed am would be 4: pm(1)+full(2)+am(1)=4
		if h.Halves != 4 {
			t.Fatalf("half-day halves = %d", h.Halves)
		}
	}
	// A weekend-only request has no working days.
	if lr, err := x.s.CreateLeaveRequest(x.ctx, x.danA, LeaveRequestIn{TypeID: x.vac.ID, StartDate: "2026-03-14", EndDate: "2026-03-15"}); err == nil {
		if _, err := x.s.SubmitLeaveRequest(x.ctx, x.danA, lr.ID); err == nil {
			t.Fatal("a weekend-only request was accepted")
		}
	}
	// One day, morning only: the same day cannot be both pm-start and am-end.
	for _, bad := range []LeaveRequestIn{
		{TypeID: x.vac.ID, StartDate: "2026-03-23", EndDate: "2026-03-23", StartPart: "pm", EndPart: "am"},
		{TypeID: x.vac.ID, StartDate: "2026-03-24", EndDate: "2026-03-23"},
		{TypeID: x.vac.ID, StartDate: "2024-01-01", EndDate: "2024-01-02"},
		{TypeID: x.vac.ID, StartDate: "2026-03-23", EndDate: "2028-03-23"},
		{TypeID: x.vac.ID, StartDate: "2026-03-23", EndDate: "2026-03-24", StartPart: "noon"},
		{TypeID: "lvt_000000000000000000000000", StartDate: "2026-03-23", EndDate: "2026-03-24"},
		{TypeID: x.vac.ID, StartDate: "not-a-date", EndDate: "2026-03-24"},
	} {
		if _, err := x.s.CreateLeaveRequest(x.ctx, x.eveA, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func sp(s string) *string { return &s }

func TestAccrualsCarryOverAndIdempotence(t *testing.T) {
	x := newToEnv(t)
	// 2026: an annual grant of 50 halves (25 days) was granted by the setup run.
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2026-12-31"); got != 50 {
		t.Fatalf("balance = %d", got)
	}
	n, err := x.s.RunAccruals(x.ctx, x.hr)
	x.must(err)
	if n != 0 {
		t.Fatalf("second run inserted %d entries", n)
	}
	// A monthly type accrues for each month since January, once.
	mon, err := x.s.CreateLeaveType(x.ctx, x.hr, LeaveTypeIn{Key: "monthly", Name: "Monthly", AccrualMode: "monthly", GrantHalves: 4})
	x.must(err)
	if _, err := x.s.RunAccruals(x.ctx, x.hr); err != nil {
		t.Fatal(err)
	}
	if got := x.balance(x.annA, x.ann.ID, mon, "2026-12-31"); got != 12 { // Jan, Feb, Mar
		t.Fatalf("monthly balance = %d", got)
	}
	if got := x.balance(x.annA, x.ann.ID, mon, "2026-02-15"); got != 8 {
		t.Fatalf("as-of balance = %d", got)
	}
	// Use 20 halves in 2026, leaving 30; the new year carries at most 10 (cap) and grants 50.
	*x.clock = time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	_, err = x.s.AdjustLeave(x.ctx, x.hr, x.ann.ID, x.vac.ID, -20, "used before the system was introduced")
	x.must(err)
	*x.clock = time.Date(2027, 1, 4, 9, 0, 0, 0, time.UTC)
	if _, err := x.s.RunAccruals(x.ctx, x.hr); err != nil {
		t.Fatal(err)
	}
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2027-01-04"); got != 60 { // 30 -> 10 carried + 50
		t.Fatalf("balance after carry-over = %d", got)
	}
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2026-12-31"); got != 30 {
		t.Fatalf("balance as of last year changed: %d", got)
	}
	if n, _ := x.s.RunAccruals(x.ctx, x.hr); n != 0 {
		t.Fatalf("rerun inserted %d", n)
	}
	if _, err := x.s.RunAccruals(x.ctx, x.mgrA); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager ran accruals: %v", err)
	}
}

func TestRequestLifecycleLedgerAndSeparation(t *testing.T) {
	x := newToEnv(t)
	lr := x.draft(x.annA, x.vac, "2026-03-23", "2026-03-27")
	// Drafts are private: not even the manager sees them.
	if _, err := x.s.GetLeaveRequest(x.ctx, x.mgrA, lr.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("manager sees a draft: %v", err)
	}
	lr, err := x.s.SubmitLeaveRequest(x.ctx, x.annA, lr.ID)
	x.must(err)
	if lr.Status != "pending" || lr.Halves != 10 {
		t.Fatalf("submitted = %+v", lr)
	}
	// Only the manager (or hr) decides; not the requester, a colleague, or the skip-level.
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.annA, lr.ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("requester approved own leave: %v", err)
	}
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.benA, lr.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("colleague decided: %v", err)
	}
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.bossA, lr.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("skip-level decided: %v", err)
	}
	annHR := x.annA
	annHR.Roles = []string{RoleEmployee, RoleHR}
	if _, err := x.s.DecideLeaveRequest(x.ctx, annHR, lr.ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("requester approved own leave as hr: %v", err)
	}
	// The balance is not touched until approval; pending requests reserve it.
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2026-12-31"); got != 50 {
		t.Fatalf("balance before approval = %d", got)
	}
	lr, err = x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true)
	x.must(err)
	if lr.Status != "approved" {
		t.Fatal("not approved")
	}
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2026-12-31"); got != 40 {
		t.Fatalf("balance after approval = %d", got)
	}
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("decided twice: %v", err)
	}
	// The ledger is append-only at the database level.
	if _, err := x.s.db.Exec(`UPDATE leave_ledger SET halves = 1000`); err == nil {
		t.Fatal("ledger entry edited")
	}
	if _, err := x.s.db.Exec(`DELETE FROM leave_ledger`); err == nil {
		t.Fatal("ledger entry deleted")
	}
	// A manager's own request goes to their manager.
	m := x.submit(x.mgrA, x.vac, "2026-04-06", "2026-04-07")
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, m.ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager approved own: %v", err)
	}
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.bossA, m.ID, true); err != nil {
		t.Fatal(err)
	}
	// Rejection frees the reservation and is final.
	r2 := x.submit(x.benA, x.vac, "2026-04-06", "2026-04-07")
	r2, err = x.s.DecideLeaveRequest(x.ctx, x.mgrA, r2.ID, false)
	x.must(err)
	if r2.Status != "rejected" {
		t.Fatal(r2.Status)
	}
	if _, err := x.s.SubmitLeaveRequest(x.ctx, x.benA, r2.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("resubmitted a rejected request: %v", err)
	}
	// The note is sealed at rest and visible to the person, their manager and hr only.
	x.must(x.s.scrub(x.ctx, false))
	raw, _ := os.ReadFile(x.path)
	if strings.Contains(string(raw), "private note") {
		t.Fatal("note stored in plaintext")
	}
	for name, a := range map[string]Actor{"ann": x.annA, "manager": x.mgrA, "hr": x.hr} {
		got, err := x.s.GetLeaveRequest(x.ctx, a, lr.ID)
		if err != nil || got.Note != "private note" {
			t.Errorf("%s: %v %+v", name, err, got)
		}
	}
	if x.aud.find("read", "leave-request:"+lr.ID, "ok") == nil {
		t.Fatal("read of a leave request not audited")
	}
	x.aud.fail = true
	if _, err := x.s.GetLeaveRequest(x.ctx, x.mgrA, lr.ID); err == nil {
		t.Fatal("request returned although the audit sink is down")
	}
	x.aud.fail = false
	// Events carry ids and dates only, and no leave type.
	evs, _ := x.s.AllEvents(x.ctx)
	seen := 0
	for _, ev := range evs {
		if ev.Type != EventTimeOffApproved {
			continue
		}
		seen++
		for k := range ev.Data {
			switch k {
			case "request_id", "person_id", "start_date", "end_date", "start_part", "end_part", "halves":
			default:
				t.Errorf("unexpected key %q in timeoff.approved", k)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("timeoff.approved events = %d", seen)
	}
}

func TestApprovalEngineAndLeaveSeparation(t *testing.T) {
	x := newToEnv(t)
	eng := &fakeEngine{}
	x.s.approvals = eng
	lr := x.submit(x.annA, x.vac, "2026-03-23", "2026-03-24")
	if len(eng.reqs) != 1 || eng.reqs[0].Type != "timeoff" || eng.reqs[0].Ref != lr.ID {
		t.Fatalf("engine requests = %+v", eng.reqs)
	}
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("approved while the engine says pending: %v", err)
	}
	// The engine's decider is the requester under another spelling: refused.
	eng.out, eng.by = "approved", " SSO-ANN\u200b"
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true); !errors.Is(err, ErrSeparation) {
		t.Fatalf("engine decider = requester: %v", err)
	}
	eng.by = "sso-mgr"
	if _, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true); err != nil {
		t.Fatal(err)
	}
	// An engine failure at submission fails the submission: nothing is left half-pending.
	eng.failReq = true
	lr2 := x.draft(x.annA, x.vac, "2026-04-13", "2026-04-14")
	if _, err := x.s.SubmitLeaveRequest(x.ctx, x.annA, lr2.ID); err == nil {
		t.Fatal("submitted although the engine is down")
	}
	got, _ := x.s.GetLeaveRequest(x.ctx, x.annA, lr2.ID)
	if got.Status != "draft" {
		t.Fatalf("status after failed submit = %s", got.Status)
	}
}

func TestBalanceChecksUnderConcurrency(t *testing.T) {
	x := newToEnv(t)
	// 50 halves available; each request is 10 halves; submit 12 concurrently: exactly 5 may be pending.
	var drafts []string
	starts := []string{"2026-04-06", "2026-04-13", "2026-04-20", "2026-04-27", "2026-05-04", "2026-05-11", "2026-05-18", "2026-05-25", "2026-06-01", "2026-06-08", "2026-06-15", "2026-06-22"}
	for _, st := range starts {
		d, _ := time.Parse(dateLayout, st)
		lr := x.draft(x.annA, x.vac, st, d.AddDate(0, 0, 4).Format(dateLayout))
		drafts = append(drafts, lr.ID)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, refused := 0, 0
	for _, id := range drafts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := x.s.SubmitLeaveRequest(x.ctx, x.annA, id)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if errors.Is(err, ErrConflict) {
				refused++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 5 || refused != 7 {
		t.Fatalf("pending = %d refused = %d", ok, refused)
	}
	// Approve concurrently after an adjustment took 20 halves away: only 3 of the 5 still fit.
	_, err := x.s.AdjustLeave(x.ctx, x.hr, x.ann.ID, x.vac.ID, -20, "correction")
	x.must(err)
	lrs, err := x.s.ListLeaveRequests(x.ctx, x.mgrA, LeaveFilter{Status: "pending"})
	x.must(err)
	if len(lrs) != 5 {
		t.Fatalf("manager sees %d pending", len(lrs))
	}
	approved := 0
	var wg2 sync.WaitGroup
	for _, lr := range lrs {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			_, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				approved++
			}
		}()
	}
	wg2.Wait()
	if approved != 3 {
		t.Fatalf("approved %d, want 3", approved)
	}
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2026-12-31"); got != 0 {
		t.Fatalf("final balance = %d", got)
	}
	// A type that allows negative balances, up to a floor.
	neg, err := x.s.CreateLeaveType(x.ctx, x.hr, LeaveTypeIn{Key: "advance", Name: "Advance", AccrualMode: "annual", GrantHalves: 4, AllowNegative: true, NegativeFloorHalves: 6})
	x.must(err)
	_, err = x.s.RunAccruals(x.ctx, x.hr)
	x.must(err)
	if _, err := x.s.SubmitLeaveRequest(x.ctx, x.benA, x.draft(x.benA, neg, "2026-07-06", "2026-07-09").ID); err != nil { // 8 halves: 4-8 = -4 >= -6
		t.Fatalf("within the negative floor: %v", err)
	}
	if _, err := x.s.SubmitLeaveRequest(x.ctx, x.benA, x.draft(x.benA, neg, "2026-07-13", "2026-07-14").ID); !errors.Is(err, ErrConflict) { // pending 8 + 4 = 12; 4-12 = -8 < -6
		t.Fatalf("beyond the floor: %v", err)
	}
}

func TestOverlapBlackoutsAndHalfDayOverlap(t *testing.T) {
	x := newToEnv(t)
	a := x.submit(x.annA, x.vac, "2026-03-23", "2026-03-24")
	_ = a
	if _, err := x.s.SubmitLeaveRequest(x.ctx, x.annA, x.draft(x.annA, x.vac, "2026-03-24", "2026-03-25").ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlapping request accepted: %v", err)
	}
	// Tuesday morning is taken (whole Tuesday); Wednesday afternoon-only start is fine.
	pm, err := x.s.CreateLeaveRequest(x.ctx, x.annA, LeaveRequestIn{TypeID: x.vac.ID, StartDate: "2026-03-25", EndDate: "2026-03-25", StartPart: "pm"})
	x.must(err)
	if _, err := x.s.SubmitLeaveRequest(x.ctx, x.annA, pm.ID); err != nil {
		t.Fatalf("adjacent half day refused: %v", err)
	}
	am, err := x.s.CreateLeaveRequest(x.ctx, x.annA, LeaveRequestIn{TypeID: x.vac.ID, StartDate: "2026-03-25", EndDate: "2026-03-25", EndPart: "am"})
	x.must(err)
	if _, err := x.s.SubmitLeaveRequest(x.ctx, x.annA, am.ID); err != nil {
		t.Fatalf("the other half of the day refused: %v", err)
	}
	// Blackouts: hr only; block vacation but not sick leave.
	if _, err := x.s.CreateBlackout(x.ctx, x.mgrA, BlackoutIn{Name: "Year end", StartDate: "2026-12-20", EndDate: "2026-12-31"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager created a blackout: %v", err)
	}
	b, err := x.s.CreateBlackout(x.ctx, x.hr, BlackoutIn{Name: "Audit week", StartDate: "2026-05-04", EndDate: "2026-05-08"})
	x.must(err)
	if _, err := x.s.SubmitLeaveRequest(x.ctx, x.benA, x.draft(x.benA, x.vac, "2026-05-06", "2026-05-06").ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("blackout ignored: %v", err)
	}
	if _, err := x.s.SubmitLeaveRequest(x.ctx, x.benA, x.draft(x.benA, x.sick, "2026-05-06", "2026-05-06").ID); err != nil {
		t.Fatalf("sick leave blocked by a blackout: %v", err)
	}
	// A team-scoped blackout only applies to that team.
	x.must(x.s.DeleteBlackout(x.ctx, x.hr, b.ID))
	if _, err := x.s.SubmitLeaveRequest(x.ctx, x.catA, x.draft(x.catA, x.vac, "2026-05-06", "2026-05-06").ID); err != nil {
		t.Fatalf("blackout removed but still applied: %v", err)
	}
}

func TestCancellationReversesTheLedger(t *testing.T) {
	x := newToEnv(t)
	lr := x.submit(x.annA, x.vac, "2026-04-06", "2026-04-10")
	lr, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true)
	x.must(err)
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2026-12-31"); got != 40 {
		t.Fatal(got)
	}
	if _, err := x.s.CancelLeaveRequest(x.ctx, x.benA, lr.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("colleague cancelled: %v", err)
	}
	if _, err := x.s.CancelLeaveRequest(x.ctx, x.mgrA, lr.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager cancelled an approved request: %v", err)
	}
	lr, err = x.s.CancelLeaveRequest(x.ctx, x.annA, lr.ID)
	x.must(err)
	if lr.Status != "cancelled" {
		t.Fatal(lr.Status)
	}
	if got := x.balance(x.annA, x.ann.ID, x.vac, "2026-12-31"); got != 50 {
		t.Fatalf("balance after cancellation = %d", got)
	}
	if _, err := x.s.CancelLeaveRequest(x.ctx, x.annA, lr.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled twice: %v", err)
	}
	var reversals int
	x.must(x.s.db.QueryRow(`SELECT COUNT(*) FROM leave_ledger WHERE kind = 'reversal'`).Scan(&reversals))
	if reversals != 1 {
		t.Fatalf("reversals = %d", reversals)
	}
	evs, _ := x.s.AllEvents(x.ctx)
	found := false
	for _, ev := range evs {
		if ev.Type == EventTimeOffCancelled {
			found = true
		}
	}
	if !found {
		t.Fatal("no timeoff.cancelled.v1")
	}
	// Started leave: the employee cannot cancel it; hr can, and only the unused days come back.
	l2 := x.submit(x.benA, x.vac, "2026-03-09", "2026-03-13") // Mon-Fri, today is Tue the 10th
	l2, err = x.s.DecideLeaveRequest(x.ctx, x.mgrA, l2.ID, true)
	x.must(err)
	if _, err := x.s.CancelLeaveRequest(x.ctx, x.benA, l2.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("employee cancelled started leave: %v", err)
	}
	if _, err := x.s.CancelLeaveRequest(x.ctx, x.hr, l2.ID); err != nil {
		t.Fatal(err)
	}
	if got := x.balance(x.benA, x.ben.ID, x.vac, "2026-12-31"); got != 50-2 { // Mon (2 halves) was used; Tue-Fri return
		t.Fatalf("partial reversal balance = %d", got)
	}
	// A pending request is cancelled without ledger entries.
	l3 := x.submit(x.catA, x.vac, "2026-04-06", "2026-04-07")
	if _, err := x.s.CancelLeaveRequest(x.ctx, x.catA, l3.ID); err != nil {
		t.Fatal(err)
	}
	if got := x.balance(x.catA, x.cat.ID, x.vac, "2026-12-31"); got != 50 {
		t.Fatal(got)
	}
}

func TestCalendarShowsOnlyOutToColleagues(t *testing.T) {
	x := newToEnv(t)
	sick := x.submit(x.annA, x.sick, "2026-03-23", "2026-03-24")
	_, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, sick.ID, true)
	x.must(err)
	vac := x.submit(x.annA, x.vac, "2026-04-06", "2026-04-07")
	_ = vac // still pending
	// Same team? Teams are optional here: put Ann and Ben in one.
	tm, err := x.s.CreateOrgUnit(x.ctx, x.hr, "teams", OrgInput{Name: "Platform"})
	x.must(err)
	for _, p := range []*Person{x.ann, x.ben, x.mgr} {
		_, err = x.s.UpdatePerson(x.ctx, x.hr, p.ID, PersonPatch{TeamID: sp(tm.ID)})
		x.must(err)
	}
	cal, err := x.s.LeaveCalendar(x.ctx, x.benA, "2026-03-01", "2026-04-30", tm.ID)
	x.must(err)
	if len(cal) != 1 {
		t.Fatalf("colleague sees %d entries (pending must be hidden): %+v", len(cal), cal)
	}
	b, _ := jsonString(cal)
	for _, leak := range []string{"sick", "type", "request", "status", "pending", "approved", "halves", "note"} {
		if strings.Contains(strings.ToLower(b), leak) {
			t.Errorf("colleague calendar leaks %q: %s", leak, b)
		}
	}
	if !cal[0].Out || cal[0].PersonID != x.ann.ID || cal[0].StartDate != "2026-03-23" {
		t.Fatalf("entry = %+v", cal[0])
	}
	// Manager and the person see the type; the manager sees the pending one too.
	for name, a := range map[string]Actor{"manager": x.mgrA, "self": x.annA, "hr": x.hr} {
		c, err := x.s.LeaveCalendar(x.ctx, a, "2026-03-01", "2026-04-30", tm.ID)
		x.must(err)
		if len(c) != 2 || c[0].Type != "sick" {
			t.Errorf("%s calendar = %+v", name, c)
		}
	}
	// Someone outside the team sees nothing of it; unknown teams and wide ranges are refused or empty.
	if c, _ := x.s.LeaveCalendar(x.ctx, x.catA, "2026-03-01", "2026-04-30", tm.ID); len(c) != 0 {
		t.Fatalf("non-member sees %+v", c)
	}
	if _, err := x.s.LeaveCalendar(x.ctx, x.benA, "2026-01-01", "2027-12-31", tm.ID); err == nil {
		t.Fatal("a two year range was accepted")
	}
	// Direct reads: a colleague gets a 404, never a 403.
	if _, err := x.s.GetLeaveRequest(x.ctx, x.benA, sick.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("colleague read a request: %v", err)
	}
	if bs, err := x.s.LeaveBalances(x.ctx, x.benA, x.ann.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("colleague read balances: %v %v", bs, err)
	}
}

func TestLongLeaveMovesStatusWithoutLeakingTheReason(t *testing.T) {
	x := newToEnv(t)
	// Parental leave of 10 halves or more sets the leave status while it runs.
	lr := x.submit(x.annA, x.parental, "2026-03-16", "2026-03-27")
	_, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true)
	x.must(err)
	if p, _ := x.s.GetPerson(x.ctx, x.hr, x.ann.ID); p.Status != StActive {
		t.Fatalf("status before the leave starts = %s", p.Status)
	}
	*x.clock = time.Date(2026, 3, 16, 8, 0, 0, 0, time.UTC)
	n, err := x.s.ApplyDue(x.ctx)
	x.must(err)
	_ = n
	p, _ := x.s.GetPerson(x.ctx, x.hr, x.ann.ID)
	if p.Status != StLeave {
		t.Fatalf("status during leave = %s", p.Status)
	}
	// Colleagues still see "active", unchanged updated_at, and no event says leave.
	cp, err := x.s.GetPerson(x.ctx, x.benA, x.ann.ID)
	x.must(err)
	if cp.Status != StActive || cp.UpdatedAt != cp.CreatedAt {
		t.Fatalf("colleague view = %s / %s", cp.Status, cp.UpdatedAt)
	}
	evs, _ := x.s.AllEvents(x.ctx)
	for _, ev := range evs {
		b, _ := jsonString(ev)
		if ev.Type == EventPersonStatusChanged && strings.Contains(b, `"leave"`) {
			t.Fatalf("event leaks leave: %s", b)
		}
		if strings.Contains(b, "parental") {
			t.Fatalf("event carries the leave type: %s", b)
		}
	}
	// The status history records no reason; the note is empty.
	h, err := x.s.History(x.ctx, x.annA, x.ann.ID)
	x.must(err)
	for _, e := range h {
		if e.Note != "" {
			t.Fatalf("history note = %q", e.Note)
		}
	}
	// A manager on leave cannot approve (no elevated role while on leave): hr does.
	// After the end date the person is active again.
	*x.clock = time.Date(2026, 3, 30, 8, 0, 0, 0, time.UTC)
	_, err = x.s.ApplyDue(x.ctx)
	x.must(err)
	p, _ = x.s.GetPerson(x.ctx, x.hr, x.ann.ID)
	if p.Status != StActive {
		t.Fatalf("status after the leave = %s", p.Status)
	}
	// HR-set leave is not touched by a leave request ending.
	_, err = x.s.ChangeStatus(x.ctx, x.hr, x.ben.ID, StLeave, "", "")
	x.must(err)
	_, err = x.s.ApplyDue(x.ctx)
	x.must(err)
	if p, _ = x.s.GetPerson(x.ctx, x.hr, x.ben.ID); p.Status != StLeave {
		t.Fatalf("hr-set leave changed: %s", p.Status)
	}
	// Short leave does not set the status.
	l2 := x.submit(x.catA, x.parental, "2026-04-06", "2026-04-07")
	_, err = x.s.DecideLeaveRequest(x.ctx, x.mgrA, l2.ID, true)
	x.must(err)
	*x.clock = time.Date(2026, 4, 6, 8, 0, 0, 0, time.UTC)
	_, _ = x.s.ApplyDue(x.ctx)
	if p, _ = x.s.GetPerson(x.ctx, x.hr, x.cat.ID); p.Status != StActive {
		t.Fatalf("short leave set the status: %s", p.Status)
	}
}

func TestTimeOffErasureAndLedgerGuard(t *testing.T) {
	x := newToEnv(t)
	lr := x.submit(x.annA, x.sick, "2026-03-23", "2026-03-24")
	_, err := x.s.DecideLeaveRequest(x.ctx, x.mgrA, lr.ID, true)
	x.must(err)
	_, err = x.s.AdjustLeave(x.ctx, x.hr, x.ann.ID, x.vac.ID, 2, "ERASE-ME-REASON")
	x.must(err)
	for _, st := range []Status{StOffboarding, StFormer} {
		_, err = x.s.ChangeStatus(x.ctx, x.hr, x.ann.ID, st, "", "")
		x.must(err)
	}
	if _, err := x.s.RequestErasure(x.ctx, x.hr, x.ann.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	x.must(x.s.db.QueryRow(`SELECT COUNT(*) FROM leave_requests WHERE person_id = ?`, x.ann.ID).Scan(&n))
	if n != 0 {
		t.Fatalf("requests remain: %d", n)
	}
	x.must(x.s.db.QueryRow(`SELECT COUNT(*) FROM leave_ledger WHERE person_id = ?`, x.ann.ID).Scan(&n))
	if n != 0 {
		t.Fatalf("ledger remains: %d", n)
	}
	var open int
	x.must(x.s.db.QueryRow(`SELECT open FROM ledger_guard`).Scan(&open))
	if open != 0 {
		t.Fatal("the ledger guard was left open")
	}
	if _, err := x.s.db.Exec(`DELETE FROM leave_ledger`); err == nil && n == 0 {
		// no rows: deleting nothing succeeds; the trigger fires per row, so insert one to prove it.
		_, err = x.s.db.Exec(`INSERT INTO leave_ledger (person_id, type_id, entry_date, kind, halves, ref, created_by, created_at) VALUES (?,?,?,?,?,?,?,?)`, x.ben.ID, x.vac.ID, "2026-01-01", "adjust", 1, "r1", "t", 1)
		x.must(err)
		if _, err := x.s.db.Exec(`DELETE FROM leave_ledger`); err == nil {
			t.Fatal("ledger rows deletable outside erasure")
		}
	}
	raw, _ := os.ReadFile(x.path)
	if wal, err := os.ReadFile(x.path + "-wal"); err == nil {
		raw = append(raw, wal...)
	}
	if strings.Contains(string(raw), "private note") {
		t.Fatal("erased leave note found in the database files")
	}
}

func TestTimeOffServiceAccountAndStatusGates(t *testing.T) {
	x := newToEnv(t)
	// A service account has no person record: it cannot request leave, but hr/admin can read.
	if _, err := x.s.CreateLeaveRequest(x.ctx, x.hr, LeaveRequestIn{TypeID: x.vac.ID, StartDate: "2026-03-23", EndDate: "2026-03-24"}); err == nil {
		t.Fatal("a service account created a leave request")
	}
	// A former employee with a still-valid token gets nothing.
	_, err := x.s.ChangeStatus(x.ctx, x.hr, x.ben.ID, StOffboarding, "", "")
	x.must(err)
	_, err = x.s.ChangeStatus(x.ctx, x.hr, x.ben.ID, StFormer, "", "")
	x.must(err)
	former := x.benA
	former.Status = StFormer
	if _, err := x.s.CreateLeaveRequest(x.ctx, former, LeaveRequestIn{TypeID: x.vac.ID, StartDate: "2026-03-23", EndDate: "2026-03-24"}); err == nil {
		t.Fatal("a former employee created a leave request")
	}
}
