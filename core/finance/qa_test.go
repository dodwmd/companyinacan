package finance

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Regression tests for the QA review of PR #21.

func TestReservedSeriesCannotBurnPOOrCreditNoteNumbers(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	vend := e.party(maker, "Vendor Fake", "vendor")
	for _, s := range []string{"PO", "CN"} {
		e.must(422, maker, "POST", "/v1/invoices", obj{"party_id": cust, "series": s, "lines": []obj{line("x", 1000, 1)}}, nil)
		e.must(422, maker, "POST", "/v1/recurring", obj{"party_id": cust, "name": "r", "interval": "monthly", "start_date": "2026-12-01", "series": s, "lines": []obj{line("x", 1000, 1)}}, nil)
		e.must(422, maker, "POST", "/v1/timesheets/billing-draft", obj{"party_id": cust, "project": "ACME", "from": "2026-11-01", "to": "2026-11-30", "rate_minor": 1, "series": s}, nil)
	}
	var po PurchaseOrder
	e.must(201, maker, "POST", "/v1/purchase-orders", obj{"vendor_id": vend, "lines": []obj{line("x", 1000, 1)}}, &po)
	e.must(200, boss, "POST", "/v1/purchase-orders/"+id(po.ID)+"/approve", nil, &po)
	if *po.Number != "PO-00001" {
		t.Fatalf("po number %s", *po.Number)
	}
	// credit notes use their own fixed counter, independent of invoices
	inv := e.invoice(maker, cust, nil, line("A", 1000, 1000))
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	var cn Invoice
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/credit-notes", obj{"reason": "r", "lines": []obj{line("c", 1000, 100)}}, &cn)
	e.must(200, boss, "POST", "/v1/invoices/"+id(cn.ID)+"/approve", nil, &cn)
	if *cn.Number != "CN-00001" {
		t.Fatalf("credit note number %s", *cn.Number)
	}
}

func TestCreditNoteCannotExceedUnpaidBalance(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	inv := e.invoice(maker, cust, nil, line("A", 1000, 10000))
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 4000}, nil)
	credit := func(amount int64) int {
		var cn Invoice
		e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/credit-notes", obj{"reason": "r", "lines": []obj{line("c", 1000, amount)}}, &cn)
		return e.do(boss, "POST", "/v1/invoices/"+id(cn.ID)+"/approve", nil, nil)
	}
	if code := credit(6001); code != 422 { // 4000 paid: only 6000 is open
		t.Fatalf("over-credit = %d, want 422", code)
	}
	if code := credit(6000); code != 200 {
		t.Fatalf("credit of the open balance = %d", code)
	}
	var got Invoice
	e.must(200, maker, "GET", "/v1/invoices/"+id(inv.ID), nil, &got)
	if got.Status != "paid" || got.BalanceMinor != 0 || got.PaidMinor != 4000 || got.CreditedMinor != 6000 {
		t.Fatalf("settled %+v", got)
	}
	if code := credit(1); code != 422 {
		t.Fatalf("credit after settlement = %d", code)
	}
	// fully paid invoice: nothing left to credit
	full := e.invoice(maker, cust, nil, line("B", 1000, 500))
	e.must(200, boss, "POST", "/v1/invoices/"+id(full.ID)+"/approve", nil, nil)
	e.must(201, maker, "POST", "/v1/invoices/"+id(full.ID)+"/payments", obj{"amount_minor": 500}, nil)
	var cn Invoice
	e.must(201, maker, "POST", "/v1/invoices/"+id(full.ID)+"/credit-notes", obj{"reason": "r", "lines": []obj{line("c", 1000, 1)}}, &cn)
	e.must(422, boss, "POST", "/v1/invoices/"+id(cn.ID)+"/approve", nil, nil)
	var n int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM invoices WHERE kind = 'invoice' AND total_minor - paid_minor - credited_minor < 0`).Scan(&n)
	if n != 0 {
		t.Fatal("a balance went negative")
	}
}

func TestAdminCannotEditSubmitAndApproveATimesheet(t *testing.T) {
	e := newEnv(t)
	alice := e.token("alice", rw, nil, nil, true)
	root := e.token("root", []string{"finance.admin"}, nil, nil, false) // admin implies approve and edit-for-others
	other := e.token("other", app, nil, nil, false)
	mk := func(week string) int64 {
		var sh Timesheet
		e.must(201, alice, "POST", "/v1/timesheets", obj{"week_start": week}, &sh)
		return sh.ID
	}
	entries := func(day string) obj {
		return obj{"entries": []obj{{"day": day, "project": "ACME", "minutes": 60, "billable": true}}}
	}
	// 1. admin edits, alice submits: admin cannot approve (edited it), another approver can
	a := mk("2026-11-16")
	e.must(200, root, "PUT", "/v1/timesheets/"+id(a)+"/entries", entries("2026-11-16"), nil)
	e.must(200, alice, "POST", "/v1/timesheets/"+id(a)+"/submit", nil, nil)
	e.must(403, root, "POST", "/v1/timesheets/"+id(a)+"/approve", nil, nil)
	e.must(403, root, "POST", "/v1/timesheets/"+id(a)+"/reject", obj{"reason": "x"}, nil)
	e.must(200, other, "POST", "/v1/timesheets/"+id(a)+"/approve", nil, nil)
	// 2. alice edits, admin submits on her behalf: admin cannot approve (submitted it)
	b := mk("2026-11-09")
	e.must(200, alice, "PUT", "/v1/timesheets/"+id(b)+"/entries", entries("2026-11-09"), nil)
	e.must(200, root, "POST", "/v1/timesheets/"+id(b)+"/submit", nil, nil)
	e.must(403, root, "POST", "/v1/timesheets/"+id(b)+"/approve", nil, nil)
	// 3. the full sequence by the admin alone
	var sh Timesheet
	e.must(201, root, "POST", "/v1/timesheets", obj{"owner": "carol", "week_start": "2026-11-02"}, &sh)
	e.must(200, root, "PUT", "/v1/timesheets/"+id(sh.ID)+"/entries", entries("2026-11-02"), nil)
	e.must(200, root, "POST", "/v1/timesheets/"+id(sh.ID)+"/submit", nil, nil)
	e.must(403, root, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, nil)
	e.must(200, other, "POST", "/v1/timesheets/"+id(sh.ID)+"/approve", nil, nil)
}

func TestSubjectComparisonIsNormalised(t *testing.T) {
	for _, c := range [][2]string{{"Alice", "alice"}, {"alice", "ALICE "}, {"ａlice", "alice"}} {
		if !sameSubject(c[0], c[1]) {
			t.Errorf("%q and %q must be the same person", c[0], c[1])
		}
	}
	if sameSubject("alice", "alicia") {
		t.Error("different people")
	}
	e := newEnv(t)
	alice := e.token("alice", rw, nil, nil, true)
	var sh Timesheet
	e.must(201, alice, "POST", "/v1/timesheets", obj{"week_start": "2026-11-23"}, &sh)
	e.must(200, alice, "PUT", "/v1/timesheets/"+id(sh.ID)+"/entries", sheetEntries("ACME", true), nil)
	e.must(200, alice, "POST", "/v1/timesheets/"+id(sh.ID)+"/submit", nil, nil)
	// a differently spelled subject (cannot be minted, but an external Authenticator could present it)
	_, err := e.app.store.ApproveTimesheet(t.Context(), &Principal{Subject: "ALICE", Permissions: []string{PermApprove}}, sh.ID, nil)
	if err != ErrSeparation {
		t.Fatalf("approval by ALICE: %v", err)
	}
	// the spelling cannot be minted either
	e.must(422, adminTok, "POST", "/v1/tokens", obj{"kind": "service", "subject": "Alice", "scopes": []string{"finance.approve"}}, nil)
	e.must(422, adminTok, "POST", "/v1/tokens", obj{"kind": "service", "subject": "ＡLICE", "scopes": []string{"finance.approve"}}, nil)
	e.must(201, adminTok, "POST", "/v1/tokens", obj{"kind": "service", "subject": "alice", "scopes": []string{"finance.read"}}, nil) // exact spelling is fine
}

func TestVoidFollowsMakerChecker(t *testing.T) {
	e := newEnv(t)
	both := e.token("both", []string{"finance.write", "finance.approve"}, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(both, "Acme Fake Ltd", "customer")
	inv := e.invoice(both, cust, nil, line("A", 1000, 100))
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	e.must(403, both, "POST", "/v1/invoices/"+id(inv.ID)+"/void", obj{"reason": "mine"}, nil)
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/void", obj{"reason": "checked"}, nil)
}

func TestErasureRemovesDraftContractsAndReportsKeptRecords(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	p := e.party(maker, "Gone Fake Person", "customer")
	var draft, sent Contract
	body := "Agreement with {{who}} living at {{addr}}."
	vars := obj{"who": "Gone Fake Person", "addr": "7 Secret Lane"}
	e.must(201, maker, "POST", "/v1/contracts", obj{"party_id": p, "title": "Draft", "body": body + " MARKER-ZX81 only in the draft", "variables": vars}, &draft)
	e.must(201, maker, "POST", "/v1/contracts", obj{"party_id": p, "title": "Sent", "body": body, "variables": vars}, &sent)
	e.must(200, maker, "POST", "/v1/contracts/"+id(sent.ID)+"/send", obj{"manual": true}, nil)
	inv := e.invoice(maker, p, nil, line("A", 1000, 100))
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil) // unpaid: blocks the purge

	var rep ErasureReport
	e.must(200, adminTok, "POST", "/v1/parties/"+id(p)+"/erase", nil, &rep)
	e.must(404, maker, "GET", "/v1/contracts/"+id(draft.ID), nil, nil)
	e.must(404, maker, "GET", "/v1/contracts/"+id(draft.ID)+"/versions/1", nil, nil)
	e.must(200, maker, "GET", "/v1/contracts/"+id(sent.ID), nil, nil) // sent: a legal record, kept
	classes := map[string]int{}
	for _, r := range rep.Retained {
		classes[r.Class] = r.Count
	}
	if classes["finance-contract"] != 1 || classes["finance-invoice"] != 1 || rep.DeletedDrafts < 1 {
		t.Fatalf("report %+v", rep)
	}
	var n int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM contract_versions WHERE contract_id = ?`, draft.ID).Scan(&n)
	if n != 0 {
		t.Fatal("draft contract versions remain")
	}
	// What only the draft held is gone from the database file and the WAL.
	for _, f := range []string{"f.db", "f.db-wal"} {
		if b, err := os.ReadFile(e.dir + "/" + f); err == nil && strings.Contains(string(b), "MARKER-ZX81") {
			t.Errorf("%s still holds the draft contract's text", f)
		}
	}
	// the purge says which erased parties it could not delete yet
	e.advance(8 * 366 * 24 * time.Hour)
	r, err := e.app.store.Purge(t.Context(), 7, 2555)
	if err != nil {
		t.Fatal(err)
	}
	if r.ErasedPartiesKept != 1 || r.ErasedParties != 0 { // the unpaid invoice is never purged
		t.Fatalf("purge %+v", r)
	}
}

func TestProviderWebhookUnappliablePaymentsAreAckedAndRecorded(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	inv := e.invoice(maker, cust, nil, line("A", 1000, 1000))
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	send := func(body string) (int, string) {
		var r struct{ Status string }
		code := e.do("", "POST", "/v1/webhooks/payments", body, &r, "X-Fake-Signature", SignFake([]byte(provSec), e.now, []byte(body)))
		return code, r.Status
	}
	// no currency: the notification is not usable
	if c, _ := send(fmt.Sprintf(`{"id":"e1","type":"payment.succeeded","invoice_id":%d,"amount_minor":100}`, inv.ID)); c != 401 {
		t.Errorf("missing currency = %d", c)
	}
	for i, body := range []string{
		fmt.Sprintf(`{"id":"e2","type":"payment.succeeded","invoice_id":%d,"amount_minor":999999,"currency":"USD"}`, inv.ID), // overpayment
		`{"id":"e3","type":"payment.succeeded","invoice_id":4242,"amount_minor":100,"currency":"USD"}`,                       // unknown invoice
		fmt.Sprintf(`{"id":"e4","type":"payment.succeeded","invoice_id":%d,"amount_minor":100,"currency":"EUR"}`, inv.ID),    // wrong currency
	} {
		if c, st := send(body); c != 200 || st != "unmatched" {
			t.Errorf("case %d: %d %q, want 200 unmatched", i, c, st)
		}
	}
	var n int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'payment.unmatched'`).Scan(&n)
	if n != 3 {
		t.Errorf("unmatched records %d, want 3", n)
	}
	var got Invoice
	e.must(200, maker, "GET", "/v1/invoices/"+id(inv.ID), nil, &got)
	if got.PaidMinor != 0 {
		t.Fatal("an unappliable payment changed the invoice")
	}
}

func TestPublicViewHidesInternalFields(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	inv := e.invoice(maker, cust, nil, line("A", 1000, 1000))
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/void", obj{"reason": "INTERNAL fraud suspicion"}, nil)
	var l struct{ URL string }
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/link", nil, &l)
	_, _, b := e.req("", "GET", strings.TrimPrefix(l.URL, "https://billing.example.test"), nil)
	if strings.Contains(string(b), "INTERNAL") || !strings.Contains(string(b), "VOID") {
		t.Fatalf("public view:\n%s", b)
	}
}

func TestPeriodLockCanBeReopened(t *testing.T) {
	e := newEnv(t)
	alice := e.token("alice", rw, nil, nil, true)
	var sh Timesheet
	e.must(201, alice, "POST", "/v1/timesheets", obj{"week_start": "2026-11-16"}, &sh)
	put := func() int {
		return e.do(alice, "PUT", "/v1/timesheets/"+id(sh.ID)+"/entries", obj{"entries": []obj{{"day": "2026-11-16", "project": "ACME", "minutes": 60}}}, nil)
	}
	e.must(200, adminTok, "PUT", "/v1/period-lock", obj{"locked_through": "2026-11-30"}, nil)
	if put() != 409 {
		t.Fatal("locked week editable")
	}
	e.must(200, adminTok, "PUT", "/v1/period-lock", obj{"locked_through": "2026-11-01", "note": "reopen for a correction"}, nil)
	var lock struct {
		LockedThrough string `json:"locked_through"`
	}
	e.must(200, alice, "GET", "/v1/period-lock", nil, &lock)
	if lock.LockedThrough != "2026-11-01" || put() != 200 {
		t.Fatalf("lock %+v after reopening", lock)
	}
}

// Every line and every document add up, whatever the inputs.
func TestDocumentTotalsAreSumsOfLines(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	e.must(201, adminTok, "POST", "/v1/tax-rates", obj{"code": "T7", "name": "7.5%", "rate_ppm": 75000}, nil)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	var ls []obj
	for i := 1; i <= 25; i++ {
		ls = append(ls, obj{"description": fmt.Sprintf("l%d", i), "quantity_milli": 333*i + 7, "unit_price_minor": 1234567 % (97 * i), "discount_bp": (i * 137) % 10000, "tax_code": "T7"})
	}
	inv := e.invoice(maker, cust, nil, ls...)
	var gross, disc, tax, total int64
	for _, l := range inv.Lines {
		if l.GrossMinor-l.DiscountMinor+l.TaxMinor != l.TotalMinor {
			t.Fatalf("line %d does not add up: %+v", l.Position, l)
		}
		gross, disc, tax, total = gross+l.GrossMinor, disc+l.DiscountMinor, tax+l.TaxMinor, total+l.TotalMinor
	}
	if inv.SubtotalMinor != gross || inv.DiscountMinor != disc || inv.TaxMinor != tax || inv.TotalMinor != total || inv.SubtotalMinor-inv.DiscountMinor+inv.TaxMinor != inv.TotalMinor {
		t.Fatalf("totals %d/%d/%d/%d vs lines %d/%d/%d/%d", inv.SubtotalMinor, inv.DiscountMinor, inv.TaxMinor, inv.TotalMinor, gross, disc, tax, total)
	}
}
