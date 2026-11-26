package finance

// Timesheets, expenses and purchase orders through can-core's approval engine
// (specs/070-approvals-inbox), against the fake can-core.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modapproval"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

const bobWriter = finPAT + "B"

func queueEnv(t *testing.T) (*env, *fakecore.Core) {
	t.Helper()
	e, core := approvalEnv(t)
	core.AddToken(bobWriter, fakecore.Token{Subject: "user:bob", Permissions: []string{PermWrite, PermApprove}})
	return e, core
}

func (e *env) sync() error { return e.app.store.SyncApprovals(context.Background()) }

func (e *env) mustSync() {
	e.t.Helper()
	if err := e.sync(); err != nil {
		e.t.Fatal(err)
	}
}

func submittedExpense(t *testing.T, e *env, tok string) Expense {
	t.Helper()
	var x Expense
	e.must(201, tok, "POST", "/v1/expenses", obj{"description": "Taxi for Jane Doe", "amount_minor": 4200, "currency": "USD", "category": "travel", "incurred_on": e.today()}, &x)
	e.must(200, tok, "POST", "/v1/expenses/"+id(x.ID)+"/submit", nil, &x)
	return x
}

func (e *env) today() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now.UTC().Format(time.DateOnly)
}

func (e *env) expense(n int64) Expense {
	e.t.Helper()
	var x Expense
	e.must(200, adminTok, "GET", "/v1/expenses/"+id(n), nil, &x)
	return x
}

func (e *env) decided(core *fakecore.Core, approvalID, outcome string, by ...string) {
	e.t.Helper()
	ev, ok := core.DecideBy(approvalID, outcome, by...)
	if !ok {
		e.t.Fatalf("no approval %s", approvalID)
	}
	if code := e.deliver(e.t, ev); code != 204 {
		e.t.Fatalf("delivery = %d", code)
	}
}

func TestExpenseIsDecidedByTheEngine(t *testing.T) {
	e, core := queueEnv(t)
	x := submittedExpense(t, e, writerPAT)
	// Nothing can approve it before the engine did, whether or not the request was sent yet.
	e.must(409, approverPAT, "POST", "/v1/expenses/"+id(x.ID)+"/approve", nil, nil)
	e.mustSync()
	reqs := core.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests: %+v", reqs)
	}
	a := reqs[0]
	if a.Type != "expense" || a.Requester != "user:alice" || a.Amount != 4200 || a.Ref != a.Key || !strings.HasPrefix(a.Ref, "expense:"+id(x.ID)+":") {
		t.Fatalf("%+v", a)
	}
	var payload map[string]string
	if err := json.Unmarshal(a.Payload, &payload); err != nil || payload["module_path"] != "/expenses/"+id(x.ID) {
		t.Fatalf("payload %s", a.Payload)
	}
	if strings.Contains(a.Title+string(a.Payload), "Jane") {
		t.Fatalf("free text left finance: %s %s", a.Title, a.Payload)
	}
	e.must(409, approverPAT, "POST", "/v1/expenses/"+id(x.ID)+"/approve", nil, nil)
	e.must(409, approverPAT, "POST", "/v1/expenses/"+id(x.ID)+"/reject", obj{"reason": "no"}, nil)
	e.mustSync()
	if n := len(core.Requests()); n != 1 {
		t.Fatalf("a second pass opened %d requests", n)
	}
	e.decided(core, a.ID, "approved", "user:carol")
	got := e.expense(x.ID)
	if got.Status != ExApproved || got.DecidedBy == nil || *got.DecidedBy != "user:carol" {
		t.Fatalf("%+v", got)
	}
	if len(e.outbox(EventExpenseDecided)) != 1 || len(e.outbox(EventExpenseApproved)) != 1 {
		t.Fatalf("events: %v %v", e.outbox(EventExpenseDecided), e.outbox(EventExpenseApproved))
	}
	// A redelivery changes nothing.
	e.decided(core, a.ID, "approved", "user:carol")
	if len(e.outbox(EventExpenseApproved)) != 1 {
		t.Fatal("applied twice")
	}
}

func TestEngineRejectionAndSeparationOfDuties(t *testing.T) {
	e, core := queueEnv(t)
	x := submittedExpense(t, e, writerPAT)
	e.mustSync()
	e.decided(core, core.Requests()[0].ID, "rejected", "")
	if got := e.expense(x.ID); got.Status != ExRejected || !strings.Contains(got.RejectReason, "approvals inbox") {
		t.Fatalf("%+v", got)
	}
	// Resubmitted: a new request. An "approval" naming the requester, or nobody, approves nothing.
	for i, by := range [][]string{{"user:alice"}, {}} {
		e.must(200, writerPAT, "POST", "/v1/expenses/"+id(x.ID)+"/submit", nil, nil)
		e.mustSync()
		reqs := core.Requests()
		if len(reqs) != i+2 {
			t.Fatalf("requests: %d", len(reqs))
		}
		e.decided(core, reqs[i+1].ID, "approved", by...)
		if got := e.expense(x.ID); got.Status != ExRejected || !strings.Contains(got.RejectReason, "separation of duties") {
			t.Fatalf("%v: %+v", by, got)
		}
	}
}

// Whoever edited a sheet for its owner is not the requester the engine knows, and still cannot
// approve it.
func TestAnEditorsApprovalIsNotApplied(t *testing.T) {
	e, core := queueEnv(t)
	alice := e.token("emp-alice", []string{"finance.write"}, nil, nil, true)
	var sh Timesheet
	e.must(201, adminTok, "POST", "/v1/timesheets", obj{"owner": "emp-alice", "week_start": monday(e)}, &sh)
	e.must(200, adminTok, "PUT", "/v1/timesheets/"+id(sh.ID)+"/entries", obj{"entries": []obj{{"day": sh.WeekStart, "project": "ACME", "minutes": 90}}}, nil)
	e.must(200, alice, "POST", "/v1/timesheets/"+id(sh.ID)+"/submit", nil, nil)
	e.mustSync()
	a := core.Requests()[0]
	if a.Type != "timesheet" || a.Requester != "party:emp-alice" {
		t.Fatalf("a module token's subject must not look like an account: %+v", a)
	}
	var payload map[string]string
	if err := json.Unmarshal(a.Payload, &payload); err != nil || payload["hours"] != "1:30" {
		t.Fatalf("payload %s", a.Payload)
	}
	e.decided(core, a.ID, "approved", bootstrapSubject) // the admin token that created the sheet
	var got Timesheet
	e.must(200, adminTok, "GET", "/v1/timesheets/"+id(sh.ID), nil, &got)
	if got.Status == TsApproved {
		t.Fatalf("the creator's approval was applied: %+v", got)
	}
}

func monday(e *env) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.now.UTC()
	return d.AddDate(0, 0, -int((d.Weekday()+6)%7)).Format(time.DateOnly)
}

func TestCancelledAndExpiredGiveTheRecordBack(t *testing.T) {
	e, core := queueEnv(t)
	for i, outcome := range []string{"cancelled", "expired"} {
		x := submittedExpense(t, e, writerPAT)
		e.mustSync()
		e.decided(core, core.Requests()[i].ID, outcome)
		if got := e.expense(x.ID); got.Status != ExDraft || !strings.Contains(got.RejectReason, outcome) {
			t.Fatalf("%+v", got)
		}
	}
}

// can-core down or refusing: the record waits, nobody can approve it, and it is sent later.
func TestUnreachableCanCoreFailsClosed(t *testing.T) {
	e, core := queueEnv(t)
	core.SetDown(true)
	// The token is checked by can-core, so use finance's own tokens while it is down.
	emp := e.token("emp-1", []string{"finance.write"}, nil, nil, true)
	boss := e.token("emp-boss", app, nil, nil, false)
	x := submittedExpense(t, e, emp)
	if err := e.sync(); err == nil {
		t.Fatal("a pass against a dead can-core must report it")
	}
	e.must(409, boss, "POST", "/v1/expenses/"+id(x.ID)+"/approve", nil, nil)
	e.must(409, adminTok, "POST", "/v1/expenses/"+id(x.ID)+"/approve", nil, nil)
	core.SetDown(false)
	core.SetApprovalStatus(422) // no approver
	e.mustSync()
	status, note, err := e.app.store.ApprovalState(context.Background(), apExpense, x.ID)
	if err != nil || status != modapproval.Refused || note == "" {
		t.Fatalf("%q %q %v", status, note, err)
	}
	e.must(409, boss, "POST", "/v1/expenses/"+id(x.ID)+"/approve", nil, nil)
	core.SetApprovalStatus(0)
	e.mustSync()
	if len(core.Requests()) != 0 {
		t.Fatal("a refused request is retried only later")
	}
	e.advance(2 * time.Hour)
	e.mustSync()
	if len(core.Requests()) != 1 {
		t.Fatalf("not sent after the wait: %+v", core.Requests())
	}
}

// A record that cannot be sent or is refused can still be sent back from the module.
func TestSendingBackWithoutAnOpenRequest(t *testing.T) {
	e, core := queueEnv(t)
	x := submittedExpense(t, e, writerPAT)
	e.must(200, approverPAT, "POST", "/v1/expenses/"+id(x.ID)+"/reject", obj{"reason": "wrong receipt"}, nil)
	e.mustSync()
	if n := len(core.Requests()); n != 0 {
		t.Fatalf("a withdrawn record was sent: %d", n)
	}
}

// An approval that cannot be applied (a locked period) is kept, and the module's route finishes it.
func TestApprovalOfALockedPeriodIsAppliedLater(t *testing.T) {
	e, core := queueEnv(t)
	x := submittedExpense(t, e, writerPAT)
	e.mustSync()
	if err := e.app.store.LockPeriod(context.Background(), &Principal{Subject: "user:cfo"}, e.today(), "month end"); err != nil {
		t.Fatal(err)
	}
	e.decided(core, core.Requests()[0].ID, "approved", "user:carol")
	if got := e.expense(x.ID); got.Status != ExSubmitted {
		t.Fatalf("%+v", got)
	}
	status, note, _ := e.app.store.ApprovalState(context.Background(), apExpense, x.ID)
	if status != modapproval.Approved || !strings.Contains(note, "locked") {
		t.Fatalf("%q %q", status, note)
	}
	e.must(409, approverPAT, "POST", "/v1/expenses/"+id(x.ID)+"/approve", nil, nil) // still locked
	if err := e.app.store.LockPeriod(context.Background(), &Principal{Subject: "user:cfo"}, "2000-01-01", "reopened"); err != nil {
		t.Fatal(err)
	}
	e.must(403, writerPAT, "POST", "/v1/expenses/"+id(x.ID)+"/approve", nil, nil)
	e.must(200, approverPAT, "POST", "/v1/expenses/"+id(x.ID)+"/approve", nil, nil)
}

// Records already waiting when the module starts using the engine are queued once, and a backlog
// stays below can-core's limit of 120 requests an hour.
func TestUpgradeQueuesWaitingRecordsWithinTheLimit(t *testing.T) {
	e, core := queueEnv(t)
	const n = modapproval.DefaultMaxHour + 5
	for range n {
		submittedExpense(t, e, writerPAT)
	}
	// As an installation from before the upgrade: submitted records and no rows.
	if _, err := e.app.store.db.Exec(`DELETE FROM approval_queue`); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		e.mustSync()
	}
	if got := len(core.Requests()); got != modapproval.DefaultMaxHour {
		t.Fatalf("sent %d in the first hour", got)
	}
	e.advance(time.Hour + time.Minute)
	e.mustSync()
	e.mustSync()
	reqs := core.Requests()
	if len(reqs) != n {
		t.Fatalf("sent %d of %d", len(reqs), n)
	}
	seen := map[string]bool{}
	for _, a := range reqs {
		seen[strings.Join(strings.Split(a.Ref, ":")[:2], ":")] = true
		if a.Requester != "user:alice" {
			t.Fatalf("requester %q", a.Requester)
		}
	}
	if len(seen) != n {
		t.Fatalf("%d distinct records", len(seen))
	}
}

func TestRateLimitedPassStopsAndWaits(t *testing.T) {
	e, core := queueEnv(t)
	submittedExpense(t, e, writerPAT)
	core.SetApprovalStatus(429)
	if err := e.sync(); err == nil {
		t.Fatal("429 must be reported")
	}
	core.SetApprovalStatus(0)
	e.mustSync() // paused
	if len(core.Requests()) != 0 {
		t.Fatal("sent while told to wait")
	}
	e.advance(10 * time.Minute)
	e.mustSync()
	if len(core.Requests()) != 1 {
		t.Fatal("not sent after the pause")
	}
}

func TestPurchaseOrderApprovalThroughTheEngine(t *testing.T) {
	e, core := queueEnv(t)
	vendor := e.party(adminTok, "Parts Co", "vendor")
	newPO := func() PurchaseOrder {
		var po PurchaseOrder
		e.must(201, writerPAT, "POST", "/v1/purchase-orders", obj{"vendor_id": vendor, "lines": []obj{line("Widgets", 1000, 5000)}}, &po)
		return po
	}
	po := newPO()
	e.must(409, approverPAT, "POST", "/v1/purchase-orders/"+id(po.ID)+"/approve", nil, nil)
	var ap, again CoreApproval
	e.must(200, writerPAT, "POST", "/v1/purchase-orders/"+id(po.ID)+"/request-approval", nil, &ap)
	e.must(200, writerPAT, "POST", "/v1/purchase-orders/"+id(po.ID)+"/request-approval", nil, &again)
	if ap.Status != modapproval.Pending || ap.ApprovalID == "" || again.ApprovalID != ap.ApprovalID || len(core.Requests()) != 1 {
		t.Fatalf("%+v %+v %+v", ap, again, core.Requests())
	}
	// Editing withdraws the request: the old decision no longer applies.
	e.must(200, writerPAT, "PUT", "/v1/purchase-orders/"+id(po.ID), obj{"vendor_id": vendor, "lines": []obj{line("Widgets", 2000, 5000)}}, nil)
	e.decided(core, ap.ApprovalID, "approved", "user:carol")
	var got PurchaseOrder
	e.must(200, adminTok, "GET", "/v1/purchase-orders/"+id(po.ID), nil, &got)
	if got.Status != PoDraft {
		t.Fatalf("a stale approval applied: %+v", got)
	}
	e.mustSync() // the cancel
	e.must(200, writerPAT, "POST", "/v1/purchase-orders/"+id(po.ID)+"/request-approval", nil, &ap)
	e.decided(core, ap.ApprovalID, "rejected")
	e.must(409, approverPAT, "POST", "/v1/purchase-orders/"+id(po.ID)+"/approve", nil, nil)
	e.must(200, writerPAT, "POST", "/v1/purchase-orders/"+id(po.ID)+"/request-approval", nil, &ap)
	e.decided(core, ap.ApprovalID, "approved", "user:carol")
	e.must(200, adminTok, "GET", "/v1/purchase-orders/"+id(po.ID), nil, &got)
	if got.Status != PoApproved || got.Number == nil || got.ApprovedBy == nil || *got.ApprovedBy != "user:carol" {
		t.Fatalf("%+v", got)
	}
	// can-core down: the request is queued, the caller is told, nothing is approved.
	po2 := newPO()
	core.SetApprovalStatus(503)
	if code, _, b := e.req(writerPAT, "POST", "/v1/purchase-orders/"+id(po2.ID)+"/request-approval", nil); code != 503 {
		t.Fatalf("%d %s", code, b)
	}
	e.must(409, approverPAT, "POST", "/v1/purchase-orders/"+id(po2.ID)+"/approve", nil, nil)
	// Cancelling an order with an open request withdraws it.
	core.SetApprovalStatus(0)
	e.mustSync()
	before := core.Requests()
	e.must(200, writerPAT, "POST", "/v1/purchase-orders/"+id(po2.ID)+"/status", obj{"status": "cancelled"}, nil)
	e.mustSync()
	if last := core.Requests()[len(before)-1]; last.Status != "cancelled" {
		t.Fatalf("%+v", last)
	}
}

func TestDecisionsForSomethingElseAreIgnored(t *testing.T) {
	e, core := queueEnv(t)
	x := submittedExpense(t, e, writerPAT)
	e.mustSync()
	a := core.Requests()[0]
	ev, _ := core.DecideBy(a.ID, "approved", "user:carol")
	forged := ev
	forged.Source = "jobs"
	if code := e.deliver(t, forged); code != 204 {
		t.Fatal(code)
	}
	other := ev
	other.ID = "evt_other"
	other.Data = []byte(fmt.Sprintf(`{"request_id":"apr_999","type":"expense","ref":%q,"outcome":"approved","approvers":["user:carol"]}`, a.Ref))
	if code := e.deliver(t, other); code != 204 {
		t.Fatal(code)
	}
	if got := e.expense(x.ID); got.Status != ExSubmitted {
		t.Fatalf("%+v", got)
	}
}

func TestEnginePagesRender(t *testing.T) {
	e, _ := queueEnv(t)
	x := submittedExpense(t, e, writerPAT)
	v, err := e.app.store.engineView(context.Background(), apExpense, x.ID, true)
	if err != nil || !v.On || v.State != modapproval.Queued || v.MayApprove() || !v.MaySendBack() {
		t.Fatalf("%+v %v", v, err)
	}
}

// A decision that never arrives does not strand the record: after a day of can-core saying the
// request is over, and another, the record is asked about again.
func TestALostDecisionIsAskedAgain(t *testing.T) {
	e, core := queueEnv(t)
	x := submittedExpense(t, e, writerPAT)
	e.mustSync()
	core.Decide(core.Requests()[0].ID, "approved") // never delivered
	e.advance(25 * time.Hour)
	e.mustSync()
	if len(core.Requests()) != 1 {
		t.Fatal("asked again at once: the delivery may still be on its way")
	}
	e.advance(25 * time.Hour)
	e.mustSync()
	e.mustSync()
	if n := len(core.Requests()); n != 2 {
		t.Fatalf("requests: %d", n)
	}
	if got := e.expense(x.ID); got.Status != ExSubmitted {
		t.Fatalf("approved without a delivered decision: %+v", got)
	}
}
