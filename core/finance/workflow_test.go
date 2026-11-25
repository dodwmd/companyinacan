package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func sheetEntries(project string, billable bool) obj {
	return obj{"entries": []obj{
		{"day": "2026-11-23", "project": project, "task": project + "-1", "minutes": 150, "billable": billable},
		{"day": "2026-11-24", "project": project, "task": project + "-1", "minutes": 90, "billable": billable},
		{"day": "2026-11-24", "project": project, "task": project + "-2", "minutes": 60, "billable": billable},
		{"day": "2026-11-25", "project": "INT", "minutes": 120, "billable": false},
	}}
}

func TestTimesheetWorkflowSoDLockAndBilling(t *testing.T) {
	e := newEnv(t)
	alice := e.token("alice", rw, nil, nil, true)
	mgr := e.token("mgr", app, nil, nil, false)
	maker := e.token("maker", rw, nil, nil, false)
	var sh Timesheet
	e.must(201, alice, "POST", "/v1/timesheets", obj{"week_start": "2026-11-23"}, &sh)
	e.must(200, alice, "POST", "/v1/timesheets", obj{"week_start": "2026-11-23"}, &sh) // opening again returns it
	e.must(422, alice, "POST", "/v1/timesheets", obj{"week_start": "2026-11-24"}, nil) // not a Monday
	e.must(422, alice, "POST", "/v1/timesheets/"+id(sh.ID)+"/submit", nil, nil)        // no entries
	// invalid entries
	for _, bad := range []obj{
		{"entries": []obj{{"day": "2026-11-30", "project": "ACME", "minutes": 60}}},
		{"entries": []obj{{"day": "2026-11-23", "project": "acme", "minutes": 60}}},
		{"entries": []obj{{"day": "2026-11-23", "project": "ACME", "task": "nope", "minutes": 60}}},
		{"entries": []obj{{"day": "2026-11-23", "project": "ACME", "minutes": 0}}},
		{"entries": []obj{{"day": "2026-11-23", "project": "ACME", "minutes": 900}, {"day": "2026-11-23", "project": "ACME", "minutes": 900}}},
	} {
		e.must(422, alice, "PUT", "/v1/timesheets/"+id(sh.ID)+"/entries", bad, nil)
	}
	e.must(200, alice, "PUT", "/v1/timesheets/"+id(sh.ID)+"/entries", sheetEntries("ACME", true), &sh)
	if sh.TotalMinutes != 420 || sh.BillableMinutes != 300 {
		t.Fatalf("totals %+v", sh)
	}
	// approving before submit conflicts; the owner cannot approve even with the scope
	e.must(409, mgr, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, nil)
	e.must(200, alice, "POST", "/v1/timesheets/"+id(sh.ID)+"/submit", nil, &sh)
	e.must(409, alice, "PUT", "/v1/timesheets/"+id(sh.ID)+"/entries", sheetEntries("ACME", true), nil) // submitted is frozen
	selfApprover := e.token("alice", []string{"finance.approve"}, nil, nil, false)                     // same person, approver token
	var eb struct{ Code string }
	e.must(403, selfApprover, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, &eb)
	if eb.Code != "separation_of_duties" {
		t.Fatalf("self approval code %q", eb.Code)
	}
	e.must(403, maker, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, nil) // write is not approve
	// reject, edit, resubmit, approve
	e.must(422, mgr, "POST", "/v1/timesheets/"+id(sh.ID)+"/reject", obj{}, nil)
	e.must(200, mgr, "POST", "/v1/timesheets/"+id(sh.ID)+"/reject", obj{"reason": "missing Friday"}, &sh)
	if sh.Status != "rejected" || sh.RejectReason == "" {
		t.Fatalf("rejected %+v", sh)
	}
	e.must(200, alice, "PUT", "/v1/timesheets/"+id(sh.ID)+"/entries", sheetEntries("ACME", true), &sh)
	e.must(200, alice, "POST", "/v1/timesheets/"+id(sh.ID)+"/submit", nil, &sh)
	e.must(200, mgr, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, &sh)
	if sh.Status != "approved" || sh.DecidedBy == nil || *sh.DecidedBy != "mgr" {
		t.Fatalf("approved %+v", sh)
	}
	evs := e.outbox("timesheet.approved.v1")
	if len(evs) != 1 || evs[0]["data"].(map[string]any)["billable_minutes"].(float64) != 300 {
		t.Fatalf("events %v", evs)
	}
	e.must(409, mgr, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, nil)

	// Billing: one line per task, minutes summed first (150+90 = 240 min = 4h; 60 min = 1h), no double billing.
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	req := obj{"party_id": cust, "project": "ACME", "from": "2026-11-23", "to": "2026-11-29", "rate_minor": 15000}
	e.must(422, maker, "POST", "/v1/timesheets/billing-draft", obj{"party_id": cust, "project": "ACME", "from": "2026-12-01", "to": "2026-12-07", "rate_minor": 1}, nil) // nothing to bill
	var inv Invoice
	e.must(201, maker, "POST", "/v1/timesheets/billing-draft", req, &inv, "Idempotency-Key", "bill-key-0001")
	if len(inv.Lines) != 2 || inv.Lines[0].QuantityMilli != 4000 || inv.Lines[1].QuantityMilli != 1000 || inv.TotalMinor != 75000 || inv.Project != "ACME" || inv.Status != "draft" {
		t.Fatalf("billing draft %+v", inv)
	}
	var again Invoice
	e.must(200, maker, "POST", "/v1/timesheets/billing-draft", req, &again, "Idempotency-Key", "bill-key-0001")
	if again.ID != inv.ID {
		t.Fatal("replay made another invoice")
	}
	e.must(422, maker, "POST", "/v1/timesheets/billing-draft", req, nil) // already billed
	// deleting the draft releases the time
	e.must(204, maker, "DELETE", "/v1/invoices/"+id(inv.ID), nil, nil)
	e.must(201, maker, "POST", "/v1/timesheets/billing-draft", req, &inv)
	// voiding an approved invoice releases it too
	e.must(200, mgr, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	e.must(200, mgr, "POST", "/v1/invoices/"+id(inv.ID)+"/void", obj{"reason": "rate was wrong"}, nil)
	req["rate_minor"] = 16000
	e.must(201, maker, "POST", "/v1/timesheets/billing-draft", req, &inv)

	// Locked periods: nothing in a locked week changes.
	var sh2 Timesheet
	e.must(201, alice, "POST", "/v1/timesheets", obj{"week_start": "2026-11-16"}, &sh2)
	e.must(403, maker, "PUT", "/v1/period-lock", obj{"locked_through": "2026-11-22"}, nil)
	e.must(200, adminTok, "PUT", "/v1/period-lock", obj{"locked_through": "2026-11-22", "note": "month close"}, nil)
	e.must(409, alice, "PUT", "/v1/timesheets/"+id(sh2.ID)+"/entries", obj{"entries": []obj{{"day": "2026-11-16", "project": "ACME", "minutes": 60}}}, nil)
	e.must(409, alice, "POST", "/v1/timesheets", obj{"week_start": "2026-11-09"}, nil)
	var lock struct {
		LockedThrough string `json:"locked_through"`
	}
	e.must(200, alice, "GET", "/v1/period-lock", nil, &lock)
	if lock.LockedThrough != "2026-11-22" {
		t.Fatalf("lock %+v", lock)
	}
}

func TestTimesheetProjectScope(t *testing.T) {
	e := newEnv(t)
	alice := e.token("alice", rw, nil, nil, true)
	var sh Timesheet
	e.must(201, alice, "POST", "/v1/timesheets", obj{"week_start": "2026-11-23"}, &sh)
	e.must(200, alice, "PUT", "/v1/timesheets/"+id(sh.ID)+"/entries", sheetEntries("ACME", true), nil)
	e.must(200, alice, "POST", "/v1/timesheets/"+id(sh.ID)+"/submit", nil, nil)
	mgrA := e.token("mgr-a", app, nil, []string{"ACME"}, false)
	mgrB := e.token("mgr-b", app, nil, []string{"OTHER"}, false)
	e.must(404, mgrB, "GET", "/v1/timesheets/"+id(sh.ID), nil, nil)
	e.must(404, mgrB, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, nil)
	var got Timesheet
	e.must(200, mgrA, "GET", "/v1/timesheets/"+id(sh.ID), nil, &got)
	for _, en := range got.Entries {
		if en.Project != "ACME" {
			t.Fatalf("entries of another project leaked: %+v", got.Entries)
		}
	}
	// mgrA sees only ACME entries of a sheet that also holds INT time, so cannot approve the whole sheet.
	e.must(404, mgrA, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, nil)
	var list struct{ Items []Timesheet }
	e.must(200, mgrB, "GET", "/v1/timesheets", nil, &list)
	if len(list.Items) != 0 {
		t.Fatal("project scope leaked in list")
	}
	// A project-scoped biller cannot bill another project.
	maker := e.token("maker", rw, nil, []string{"OTHER"}, false)
	cust := e.party(e.token("m2", rw, nil, nil, false), "Acme Fake Ltd", "customer")
	e.must(404, maker, "POST", "/v1/timesheets/billing-draft", obj{"party_id": cust, "project": "ACME", "from": "2026-11-23", "to": "2026-11-29", "rate_minor": 1}, nil)
}

type approverFn func(owner string, p *Principal) bool

func (f approverFn) CanApprove(_ context.Context, p *Principal, r ApprovalRequest) (bool, error) {
	return f(r.Owner, p), nil
}

func TestPluggableApproverStillBoundBySoD(t *testing.T) {
	// Only "mgr-of-alice" may approve alice's time (a reporting-line approver).
	e := newEnv(t, func(_ *Config, o *Options) {
		o.Approver = approverFn(func(owner string, p *Principal) bool { return owner != "alice" || p.Subject == "mgr-of-alice" })
	})
	alice := e.token("alice", rw, nil, nil, true)
	var sh Timesheet
	e.must(201, alice, "POST", "/v1/timesheets", obj{"week_start": "2026-11-23"}, &sh)
	e.must(200, alice, "PUT", "/v1/timesheets/"+id(sh.ID)+"/entries", sheetEntries("ACME", true), nil)
	e.must(200, alice, "POST", "/v1/timesheets/"+id(sh.ID)+"/submit", nil, nil)
	other := e.token("other-mgr", app, nil, nil, false)
	right := e.token("mgr-of-alice", app, nil, nil, false)
	e.must(403, other, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, nil)
	e.must(200, right, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, nil)
}

func TestExpensesWorkflow(t *testing.T) {
	e := newEnv(t)
	alice := e.token("alice", rw, nil, nil, true)
	mgr := e.token("mgr", app, nil, nil, false)
	fin := e.token("fin", rw, nil, nil, false)
	var ex Expense
	e.must(201, alice, "POST", "/v1/expenses", validExpense(), &ex, "Idempotency-Key", "exp-key-00001")
	e.must(200, alice, "POST", "/v1/expenses", validExpense(), &ex, "Idempotency-Key", "exp-key-00001")
	// validation
	for _, mut := range []func(o obj){
		func(o obj) { o["amount_minor"] = 0 }, func(o obj) { o["currency"] = "ZZZ" }, func(o obj) { o["incurred_on"] = "yesterday" },
		func(o obj) { o["category"] = "Travel Costs" },
		func(o obj) {
			o["receipt"] = obj{"filename": "../etc/passwd", "content_type": "application/pdf", "size_bytes": 1, "sha256": strings.Repeat("a", 64)}
		},
		func(o obj) {
			o["receipt"] = obj{"filename": "a.exe", "content_type": "application/x-msdownload", "size_bytes": 1, "sha256": strings.Repeat("a", 64)}
		},
		func(o obj) {
			o["receipt"] = obj{"filename": "a.pdf", "content_type": "application/pdf", "size_bytes": 1, "sha256": "short"}
		},
		func(o obj) {
			o["receipt"] = obj{"filename": "a.pdf", "content_type": "application/pdf", "size_bytes": 1 << 30, "sha256": strings.Repeat("a", 64)}
		},
	} {
		b := validExpense()
		mut(b)
		e.must(422, alice, "POST", "/v1/expenses", b, nil)
	}
	e.must(409, mgr, "POST", "/v1/expenses/"+id(ex.ID)+"/approve", nil, nil) // not submitted
	e.must(200, alice, "POST", "/v1/expenses/"+id(ex.ID)+"/submit", nil, nil)
	e.must(403, alice, "POST", "/v1/expenses/"+id(ex.ID)+"/approve", nil, nil)
	own := e.token("alice", []string{"finance.approve"}, nil, nil, false)
	e.must(403, own, "POST", "/v1/expenses/"+id(ex.ID)+"/approve", nil, nil) // alice with the approve scope still cannot
	e.must(200, mgr, "POST", "/v1/expenses/"+id(ex.ID)+"/reject", obj{"reason": "no receipt photo"}, nil)
	e.must(200, alice, "PUT", "/v1/expenses/"+id(ex.ID), validExpense(), nil)
	e.must(200, alice, "POST", "/v1/expenses/"+id(ex.ID)+"/submit", nil, nil)
	e.must(200, mgr, "POST", "/v1/expenses/"+id(ex.ID)+"/approve", nil, &ex)
	if ex.Status != "approved" || ex.Receipt == nil || ex.Receipt.SHA256 == "" {
		t.Fatalf("approved %+v", ex)
	}
	if evs := e.outbox("expense.approved.v1"); len(evs) != 1 {
		t.Fatalf("events %d", len(evs))
	}
	e.must(409, alice, "PUT", "/v1/expenses/"+id(ex.ID), validExpense(), nil)
	e.must(200, fin, "POST", "/v1/expenses/"+id(ex.ID)+"/reimburse", obj{"reference": "PAYRUN-7"}, &ex)
	if ex.Status != "reimbursed" {
		t.Fatalf("reimbursed %+v", ex)
	}
	// locked period
	e.must(200, adminTok, "PUT", "/v1/period-lock", obj{"locked_through": "2026-11-30"}, nil)
	e.must(409, alice, "POST", "/v1/expenses", validExpense(), nil)
}

func TestContractsLifecycle(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	var tpl ContractTemplate
	e.must(201, maker, "POST", "/v1/contract-templates", obj{"name": "Services", "body": "Agreement between us and {{customer}} for {{scope}}. Fee {{fee}}."}, &tpl)
	if strings.Join(tpl.Variables, ",") != "customer,fee,scope" {
		t.Fatalf("variables %v", tpl.Variables)
	}
	e.must(422, maker, "POST", "/v1/contracts", obj{"party_id": cust, "title": "T", "template_id": tpl.ID, "variables": obj{"customer": "Acme"}}, nil)                                      // missing
	e.must(422, maker, "POST", "/v1/contracts", obj{"party_id": cust, "title": "T", "template_id": tpl.ID, "variables": obj{"customer": "A", "scope": "s", "fee": "f", "extra": "x"}}, nil) // extra
	var c Contract
	e.must(201, maker, "POST", "/v1/contracts", obj{"party_id": cust, "title": "Services 2026", "template_id": tpl.ID,
		"variables": obj{"customer": "Acme {{scope}}", "scope": "web work", "fee": "$1,000"}, "start_date": "2026-11-25", "end_date": "2027-11-24", "renewal_date": "2027-10-25", "reminder_days": 30}, &c)
	var v1 ContractVersion
	e.must(200, maker, "GET", "/v1/contracts/"+id(c.ID)+"/versions/1", nil, &v1)
	if v1.Body != "Agreement between us and Acme {{scope}} for web work. Fee $1,000." { // values are not expanded again
		t.Fatalf("body %q", v1.Body)
	}
	if len(v1.SHA256) != 64 || v1.SHA256 != c.SHA256 {
		t.Fatalf("hash %s vs %s", v1.SHA256, c.SHA256)
	}
	// a new version while draft; then send
	e.must(200, maker, "POST", "/v1/contracts/"+id(c.ID)+"/versions", obj{"variables": obj{"customer": "Acme", "scope": "web and app work", "fee": "$2,000"}}, &c)
	if c.CurrentVersion != 2 || c.SHA256 == v1.SHA256 {
		t.Fatalf("v2 %+v", c)
	}
	e.must(422, maker, "POST", "/v1/contracts/"+id(c.ID)+"/send", obj{"signers": []string{"not-an-email"}}, nil)
	e.must(422, maker, "POST", "/v1/contracts/"+id(c.ID)+"/send", obj{}, nil)
	e.must(200, maker, "POST", "/v1/contracts/"+id(c.ID)+"/send", obj{"signers": []string{"signer@example.test"}}, &c)
	if c.Status != "sent" || c.ESignProvider != "fake" {
		t.Fatalf("sent %+v", c)
	}
	e.must(409, maker, "POST", "/v1/contracts/"+id(c.ID)+"/send", obj{"manual": true}, nil)
	// a wrong hash is refused, the right one signs
	e.must(422, maker, "POST", "/v1/contracts/"+id(c.ID)+"/signature", obj{"sha256": v1.SHA256}, nil)
	e.must(422, maker, "POST", "/v1/contracts/"+id(c.ID)+"/signature", obj{"sha256": "zz"}, nil)
	var ext string
	_ = e.app.store.db.QueryRow(`SELECT esign_id FROM contracts WHERE id = ?`, c.ID).Scan(&ext)
	body := fmt.Sprintf(`{"id":"evt1","type":"document.signed","document_id":%q,"sha256":%q}`, ext, c.SHA256)
	sig := SignFake([]byte(provSec), e.now, []byte(body))
	e.must(401, "", "POST", "/v1/webhooks/esign", body, nil, "X-Fake-Signature", "t=1,v1=00")
	e.must(401, "", "POST", "/v1/webhooks/esign", body, nil, "X-Fake-Signature", SignFake([]byte("wrong-secret-wrong-secret"), e.now, []byte(body)))
	e.must(401, "", "POST", "/v1/webhooks/esign", body, nil, "X-Fake-Signature", SignFake([]byte(provSec), e.now.Add(-time.Hour), []byte(body))) // stale
	e.must(200, "", "POST", "/v1/webhooks/esign", body, nil, "X-Fake-Signature", sig)
	e.must(200, "", "POST", "/v1/webhooks/esign", body, nil, "X-Fake-Signature", sig) // repeated notification
	e.must(200, maker, "GET", "/v1/contracts/"+id(c.ID), nil, &c)
	if c.Status != "active" || c.SignedSHA256 != c.SHA256 || c.SignedAt == nil {
		t.Fatalf("signed %+v", c)
	}
	if evs := e.outbox("contract.signed.v1"); len(evs) != 1 {
		t.Fatalf("signed events %d", len(evs))
	}
	e.must(409, maker, "POST", "/v1/contracts/"+id(c.ID)+"/versions", obj{"body": "changed", "variables": obj{}}, nil) // text frozen once signed
	var vr VerifyReport
	e.must(200, maker, "GET", "/v1/contracts/"+id(c.ID)+"/verify", nil, &vr)
	if !vr.OK || vr.Versions != 2 {
		t.Fatalf("verify %+v", vr)
	}
	// Tampering below the API is refused by the database, and detected by verify if it ever happened.
	if _, err := e.app.store.db.Exec(`UPDATE contract_versions SET body = 'forged'`); err == nil {
		t.Error("contract versions must be immutable")
	}
	if _, err := e.app.store.db.Exec(`UPDATE retention_gate SET open = 1`); err != nil {
		t.Fatal(err)
	}
	_, _ = e.app.store.db.Exec(`DROP TRIGGER contract_versions_no_update`) // simulate a tamperer with schema access
	_, _ = e.app.store.db.Exec(`UPDATE contract_versions SET body = 'forged' WHERE version = 2`)
	e.must(200, maker, "GET", "/v1/contracts/"+id(c.ID)+"/verify", nil, &vr)
	if vr.OK {
		t.Fatal("verify did not notice the forged text")
	}
}

func TestContractRemindersExpiryAndTermination(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	mk := func(start, end, renew string) Contract {
		var c Contract
		e.must(201, maker, "POST", "/v1/contracts", obj{"party_id": cust, "title": "C", "body": "Plain text.", "start_date": start, "end_date": end, "renewal_date": renew, "reminder_days": 30}, &c)
		e.must(200, maker, "POST", "/v1/contracts/"+id(c.ID)+"/send", obj{"manual": true}, &c)
		e.must(200, maker, "POST", "/v1/contracts/"+id(c.ID)+"/signature", obj{"sha256": c.SHA256}, &c)
		return c
	}
	future := mk("2027-01-01", "2027-12-31", "2027-01-20")
	if future.Status != "signed" { // starts later
		t.Fatalf("status %s", future.Status)
	}
	active := mk("2026-01-01", "2026-12-31", "2026-12-20")
	if active.Status != "active" {
		t.Fatalf("status %s", active.Status)
	}
	sweep := func(asOf string) int {
		var r struct{ Reminders int }
		e.must(200, adminTok, "POST", "/v1/contracts/sweep", obj{"as_of": asOf}, &r)
		return r.Reminders
	}
	if n := sweep("2026-11-25"); n != 0 { // renewal 2026-12-20, 30 days before = 2026-11-20 -> due
		_ = n
	}
	// 2026-12-20 minus 30 days is 2026-11-20, already past on the 25th: exactly one reminder, once.
	if evs := e.outbox("contract.expiring.v1"); len(evs) != 1 {
		t.Fatalf("reminders %d", len(evs))
	}
	if n := sweep("2026-11-26"); n != 0 {
		t.Fatalf("reminder repeated: %d", n)
	}
	// moving the renewal date re-arms it
	var c Contract
	e.must(200, maker, "PATCH", "/v1/contracts/"+id(active.ID)+"/dates", obj{"renewal_date": "2026-12-28"}, &c)
	if n := sweep("2026-11-30"); n != 1 {
		t.Fatalf("re-armed reminder: %d", n)
	}
	// start date arrives: signed -> active; end passes: expired
	sweep("2027-01-02")
	e.must(200, maker, "GET", "/v1/contracts/"+id(future.ID), nil, &c)
	if c.Status != "active" {
		t.Fatalf("future now %s", c.Status)
	}
	sweep("2027-01-03")
	e.must(200, maker, "GET", "/v1/contracts/"+id(active.ID), nil, &c)
	if c.Status != "expired" {
		t.Fatalf("active now %s", c.Status)
	}
	e.must(409, maker, "POST", "/v1/contracts/"+id(active.ID)+"/terminate", obj{"reason": "x"}, nil)
	e.must(422, maker, "POST", "/v1/contracts/"+id(future.ID)+"/terminate", obj{}, nil)
	e.must(200, maker, "POST", "/v1/contracts/"+id(future.ID)+"/terminate", obj{"reason": "customer left"}, &c)
	if c.Status != "terminated" {
		t.Fatal(c.Status)
	}
	// scoping
	b := e.party(maker, "Beta Fake", "customer")
	sc := e.token("beta", []string{"finance.read"}, []int64{b}, nil, false)
	e.must(404, sc, "GET", "/v1/contracts/"+id(future.ID), nil, nil)
	e.must(404, sc, "GET", "/v1/contracts/"+id(future.ID)+"/versions/1", nil, nil)
	e.must(404, sc, "GET", "/v1/contracts/"+id(future.ID)+"/verify", nil, nil)
	var list struct{ Items []Contract }
	e.must(200, sc, "GET", "/v1/contracts", nil, &list)
	if len(list.Items) != 0 {
		t.Fatal("contracts leaked across parties")
	}
}

func TestRenderTemplate(t *testing.T) {
	got, err := RenderTemplate("Hi {{name}}, {{name}}!", map[string]string{"name": "Bo"})
	if err != nil || got != "Hi Bo, Bo!" {
		t.Fatalf("%q %v", got, err)
	}
	for _, c := range []struct {
		body string
		vars map[string]string
	}{{"{{a}}", nil}, {"x", map[string]string{"a": "1"}}, {"{{a}}", map[string]string{"a": "1", "b": "2"}}, {"{{a}}", map[string]string{"A": "1"}}} {
		if _, err := RenderTemplate(c.body, c.vars); err == nil {
			t.Errorf("%q %v should fail", c.body, c.vars)
		}
	}
}

func TestPaymentProviderWebhookAndImport(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	inv := e.invoice(maker, cust, nil, line("x", 1000, 10000))
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)

	var sess CheckoutSession
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/checkout", nil, &sess)
	if !strings.HasPrefix(sess.URL, "https://pay.example.invalid/") {
		t.Fatalf("session %+v", sess)
	}
	body := fmt.Sprintf(`{"id":"evt_p1","type":"payment.succeeded","invoice_id":%d,"amount_minor":2500,"currency":"USD"}`, inv.ID)
	e.must(401, "", "POST", "/v1/webhooks/payments", body, nil)
	e.must(401, "", "POST", "/v1/webhooks/payments", body, nil, "X-Fake-Signature", SignFake([]byte("nope-nope-nope-nope"), e.now, []byte(body)))
	e.must(401, "", "POST", "/v1/webhooks/payments", body+" ", nil, "X-Fake-Signature", SignFake([]byte(provSec), e.now, []byte(body))) // body altered
	sig := SignFake([]byte(provSec), e.now, []byte(body))
	var r struct{ Status string }
	e.must(200, "", "POST", "/v1/webhooks/payments", body, &r, "X-Fake-Signature", sig)
	if r.Status != "recorded" {
		t.Fatal(r)
	}
	e.must(200, "", "POST", "/v1/webhooks/payments", body, &r, "X-Fake-Signature", sig)
	if r.Status != "duplicate" {
		t.Fatal("redelivery must not double-count", r)
	}
	var got Invoice
	e.must(200, maker, "GET", "/v1/invoices/"+id(inv.ID), nil, &got)
	if got.PaidMinor != 2500 || got.Status != "partially_paid" {
		t.Fatalf("after webhook %+v", got)
	}

	// Import: all or nothing; re-import is a no-op.
	rows := obj{"payments": []obj{{"invoice_number": "INV-00001", "amount_minor": 1000, "external_id": "bank-1", "received_on": "2026-11-20"}, {"invoice_number": "INV-00001", "amount_minor": 500, "external_id": "bank-2"}}}
	var res struct{ Items []ImportResult }
	e.must(201, maker, "POST", "/v1/payments/import", rows, &res)
	if len(res.Items) != 2 || res.Items[0].Status != "created" {
		t.Fatalf("import %+v", res)
	}
	e.must(201, maker, "POST", "/v1/payments/import", rows, &res)
	if res.Items[0].Status != "duplicate" || res.Items[1].Status != "duplicate" {
		t.Fatalf("re-import %+v", res)
	}
	bad := obj{"payments": []obj{{"invoice_number": "INV-00001", "amount_minor": 100, "external_id": "bank-3"}, {"invoice_number": "INV-99999", "amount_minor": 100, "external_id": "bank-4"}}}
	e.must(422, maker, "POST", "/v1/payments/import", bad, nil)
	e.must(422, maker, "POST", "/v1/payments/import", obj{"payments": []obj{{"invoice_number": "INV-00001", "amount_minor": 100}}}, nil) // external id required
	e.must(200, maker, "GET", "/v1/invoices/"+id(inv.ID), nil, &got)
	if got.PaidMinor != 4000 {
		t.Fatalf("paid %d: the failed import must have rolled back", got.PaidMinor)
	}
}

func TestAgingBuckets(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	mk := func(due string, amount int64, cur string) *Invoice {
		extra := obj{"issue_date": "2026-01-01", "due_date": due}
		if cur != "" {
			extra["currency"], extra["fx_rate"] = cur, "1.1"
		}
		i := e.invoice(maker, cust, extra, line("x", 1000, amount))
		e.must(200, boss, "POST", "/v1/invoices/"+id(i.ID)+"/approve", nil, nil)
		return i
	}
	mk("2026-12-01", 100, "")  // current
	mk("2026-11-20", 200, "")  // 5 days
	mk("2026-10-20", 400, "")  // 36 days
	mk("2026-09-20", 800, "")  // 66 days
	mk("2026-06-01", 1600, "") // 177 days
	mk("2026-11-01", 50, "EUR")
	paid := mk("2026-11-01", 999, "")
	e.must(201, maker, "POST", "/v1/invoices/"+id(paid.ID)+"/payments", obj{"amount_minor": 999}, nil)
	part := mk("2026-11-10", 1000, "")
	e.must(201, maker, "POST", "/v1/invoices/"+id(part.ID)+"/payments", obj{"amount_minor": 400}, nil)

	var ag AgingReport
	e.must(200, maker, "GET", "/v1/reports/aging?as_of=2026-11-25", nil, &ag)
	if len(ag.Currencies) != 2 || ag.Currencies[0].Currency != "EUR" || ag.Currencies[1].Currency != "USD" {
		t.Fatalf("currencies %+v", ag.Currencies)
	}
	usd := ag.Currencies[1].Totals
	want := AgingBuckets{Current: 100, Days1to30: 200 + 600, Days31_60: 400, Days61_90: 800, Over90: 1600, Total: 100 + 800 + 400 + 800 + 1600}
	if usd != want {
		t.Fatalf("usd %+v want %+v", usd, want)
	}
	if ag.Currencies[0].Totals.Days1to30 != 50 {
		t.Fatalf("eur %+v", ag.Currencies[0])
	}
	e.must(422, maker, "GET", "/v1/reports/aging?as_of=nope", nil, nil)
}

func TestRecurringInvoices(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	var r Recurring
	e.must(201, maker, "POST", "/v1/recurring", obj{"party_id": cust, "name": "Retainer", "interval": "monthly", "start_date": "2026-11-30", "end_date": "2027-01-31", "lines": []obj{line("Retainer", 1000, 50000)}}, &r)
	e.must(422, maker, "POST", "/v1/recurring", obj{"party_id": cust, "name": "x", "interval": "daily", "start_date": "2026-11-30", "lines": []obj{line("x", 1000, 1)}}, nil)
	var out struct{ Drafted []int64 }
	e.must(403, maker, "POST", "/v1/recurring/run", obj{"as_of": "2026-11-30"}, nil) // admin only
	e.must(200, adminTok, "POST", "/v1/recurring/run", obj{"as_of": "2026-11-30"}, &out)
	if len(out.Drafted) != 1 {
		t.Fatalf("first run %v", out)
	}
	e.must(200, adminTok, "POST", "/v1/recurring/run", obj{"as_of": "2026-11-30"}, &out) // same date again
	if len(out.Drafted) != 0 {
		t.Fatalf("second run drafted %v", out)
	}
	e.must(200, adminTok, "POST", "/v1/recurring/run", obj{"as_of": "2027-03-01"}, &out) // catches up Dec 30, Jan 30, then ends
	if len(out.Drafted) != 2 {
		t.Fatalf("catch-up %v", out)
	}
	var list struct{ Items []Recurring }
	e.must(200, maker, "GET", "/v1/recurring", nil, &list)
	if list.Items[0].Status != "ended" {
		t.Fatalf("schedule %+v", list.Items[0])
	}
	// generated drafts need a person's approval, and the schedule's creator can approve them
	var inv Invoice
	e.must(200, maker, "GET", "/v1/invoices/"+id(out.Drafted[0]), nil, &inv)
	if inv.Status != "draft" || inv.CreatedBy != "maker" || inv.IssueDate != "2026-12-30" {
		t.Fatalf("generated %+v", inv)
	}
	// maker-checker holds for generated drafts: the person who set the schedule up cannot approve them
	e.must(403, maker2(e), "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	// month arithmetic clamps
	for in, want := range map[string]string{"2027-01-31": "2027-02-28", "2028-01-31": "2028-02-29", "2026-11-30": "2026-12-30"} {
		if got := AddInterval(parseDate(in), "monthly").Format(time.DateOnly); got != want {
			t.Errorf("%s + 1 month = %s, want %s", in, got, want)
		}
	}
	// pausing stops generation
	var r2 Recurring
	e.must(201, maker, "POST", "/v1/recurring", obj{"party_id": cust, "name": "Paused", "interval": "weekly", "start_date": "2026-11-01", "lines": []obj{line("x", 1000, 1)}}, &r2)
	e.must(200, maker, "POST", "/v1/recurring/"+id(r2.ID)+"/status", obj{"status": "paused"}, nil)
	e.must(200, adminTok, "POST", "/v1/recurring/run", obj{"as_of": "2026-11-25"}, &out)
	if len(out.Drafted) != 0 {
		t.Fatal("paused schedule drafted")
	}
}

func TestPurchaseOrdersAndCatalogue(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	vendor := e.party(maker, "Vendor Fake", "vendor")
	cust := e.party(maker, "Cust Fake", "customer")
	e.must(201, adminTok, "POST", "/v1/tax-rates", obj{"code": "GST10", "name": "GST", "rate_ppm": 100000}, nil)
	e.must(409, adminTok, "POST", "/v1/tax-rates", obj{"code": "GST10", "name": "GST", "rate_ppm": 100000}, nil)
	e.must(403, maker, "POST", "/v1/tax-rates", obj{"code": "X", "name": "x", "rate_ppm": 1}, nil)
	e.must(422, adminTok, "POST", "/v1/tax-rates", obj{"code": "BIG", "name": "x", "rate_ppm": 2000000}, nil)
	e.must(201, maker, "POST", "/v1/products", obj{"sku": "WIDGET", "name": "Widget", "unit_price_minor": 2500, "currency": "USD", "tax_code": "GST10"}, nil)
	e.must(409, maker, "POST", "/v1/products", obj{"sku": "WIDGET", "name": "Widget", "unit_price_minor": 1, "currency": "USD"}, nil)
	e.must(422, maker, "POST", "/v1/products", obj{"sku": "BAD", "name": "x", "unit_price_minor": 1, "currency": "USD", "tax_code": "NOPE"}, nil)
	// catalogue fills a line; a later rate change does not touch an issued invoice
	inv := e.invoice(maker, cust, nil, obj{"sku": "WIDGET", "quantity_milli": 2000})
	if inv.TotalMinor != 5500 || inv.Lines[0].Description != "Widget" || inv.Lines[0].TaxRatePPM != 100000 {
		t.Fatalf("catalogue invoice %+v", inv)
	}
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	e.must(200, adminTok, "PATCH", "/v1/tax-rates/GST10", obj{"rate_ppm": 150000}, nil)
	var after Invoice
	e.must(200, maker, "GET", "/v1/invoices/"+id(inv.ID), nil, &after)
	if after.TotalMinor != 5500 {
		t.Fatalf("issued invoice changed with the tax table: %d", after.TotalMinor)
	}
	// ... while a draft saved earlier is repriced at approval
	d := e.invoice(maker, cust, nil, obj{"sku": "WIDGET", "quantity_milli": 2000})
	e.must(200, adminTok, "PATCH", "/v1/tax-rates/GST10", obj{"rate_ppm": 200000}, nil)
	var ap Invoice
	e.must(200, boss, "POST", "/v1/invoices/"+id(d.ID)+"/approve", nil, &ap)
	if ap.TotalMinor != 6000 {
		t.Fatalf("draft repriced to %d, want 6000", ap.TotalMinor)
	}

	// purchase orders
	e.must(422, maker, "POST", "/v1/purchase-orders", obj{"vendor_id": cust, "lines": []obj{line("x", 1000, 1)}}, nil)
	var po PurchaseOrder
	e.must(201, maker, "POST", "/v1/purchase-orders", obj{"vendor_id": vendor, "lines": []obj{line("Laptops", 2000, 100000)}}, &po)
	e.must(403, maker, "POST", "/v1/purchase-orders/"+id(po.ID)+"/approve", nil, nil)
	maker2 := e.token("maker2", []string{"finance.write", "finance.approve"}, nil, nil, false)
	po2 := PurchaseOrder{}
	e.must(201, maker2, "POST", "/v1/purchase-orders", obj{"vendor_id": vendor, "lines": []obj{line("x", 1000, 1)}}, &po2)
	e.must(403, maker2, "POST", "/v1/purchase-orders/"+id(po2.ID)+"/approve", nil, nil) // own
	e.must(200, boss, "POST", "/v1/purchase-orders/"+id(po.ID)+"/approve", nil, &po)
	if po.Number == nil || *po.Number != "PO-00001" || po.TotalMinor != 200000 {
		t.Fatalf("po %+v", po)
	}
	e.must(409, maker, "PUT", "/v1/purchase-orders/"+id(po.ID), obj{"vendor_id": vendor, "lines": []obj{line("x", 1000, 1)}}, nil)
	e.must(200, maker, "POST", "/v1/purchase-orders/"+id(po.ID)+"/status", obj{"status": "received"}, nil)
	e.must(409, maker, "POST", "/v1/purchase-orders/"+id(po.ID)+"/status", obj{"status": "cancelled"}, nil)
}

func TestEventsHaveNoContactData(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	inv := e.invoice(maker, cust, nil, line("x", 1000, 100))
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	e.must(200, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/send", nil, nil)
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 100}, nil)
	for _, typ := range []string{"invoice.sent.v1", "invoice.paid.v1"} {
		for _, ev := range e.outbox(typ) {
			b, _ := json.Marshal(ev)
			if strings.Contains(string(b), "Acme") || strings.Contains(string(b), "example.test") || strings.Contains(string(b), "Fake Road") {
				t.Fatalf("%s carries contact data: %s", typ, b)
			}
		}
	}
}

func maker2(e *env) string {
	return e.token("maker", []string{"finance.write", "finance.approve"}, nil, nil, false)
}
