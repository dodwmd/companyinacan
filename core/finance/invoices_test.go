package finance

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestInvoiceLifecycleAndImmutability(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	e.must(201, adminTok, "POST", "/v1/tax-rates", obj{"code": "VAT20", "name": "VAT 20%", "rate_ppm": 200000}, nil)
	cust := e.party(maker, "Acme Fake Ltd", "customer")

	inv := e.invoice(maker, cust, obj{"due_days": 14},
		obj{"description": "Consulting", "quantity_milli": 1500, "unit_price_minor": 10001, "tax_code": "VAT20"},
		obj{"description": "Setup", "quantity_milli": 1000, "unit_price_minor": 5000, "discount_bp": 1000})
	// line 1: 1.5 x 100.01 = 150.015 -> 15002 (half away from zero), tax 20% = 3000.4 -> 3000; line 2: 5000 - 500 discount = 4500.
	if inv.Status != "draft" || inv.Number != nil {
		t.Fatalf("new invoice %+v", inv)
	}
	if inv.Lines[0].GrossMinor != 15002 || inv.Lines[0].TaxMinor != 3000 || inv.Lines[1].DiscountMinor != 500 {
		t.Fatalf("lines %+v", inv.Lines)
	}
	if inv.SubtotalMinor != 20002 || inv.DiscountMinor != 500 || inv.TaxMinor != 3000 || inv.TotalMinor != 22502 || inv.DueDate != "2026-12-09" {
		t.Fatalf("totals %+v", inv)
	}

	// Drafts can be edited; the version is the ETag and If-Match is enforced.
	var up Invoice
	e.must(412, maker, "PATCH", "/v1/invoices/"+id(inv.ID), obj{"notes": "x"}, nil, "If-Match", `"99"`)
	e.must(200, maker, "PATCH", "/v1/invoices/"+id(inv.ID), obj{"notes": "Thanks"}, &up, "If-Match", fmt.Sprintf(`"%d"`, inv.Version))
	if up.Notes != "Thanks" || up.Version != inv.Version+1 || up.TotalMinor != inv.TotalMinor {
		t.Fatalf("patched %+v", up)
	}

	// Separation of duties: the creator cannot approve; a holder of finance.approve who did not create it can.
	creatorApprover := e.token("maker2", []string{"finance.write", "finance.approve"}, nil, nil, false)
	inv2 := e.invoice(creatorApprover, cust, nil, line("Mine", 1000, 100))
	var errBody struct{ Code string }
	e.must(403, creatorApprover, "POST", "/v1/invoices/"+id(inv2.ID)+"/approve", nil, &errBody)
	if errBody.Code != "separation_of_duties" {
		t.Fatalf("code %q", errBody.Code)
	}
	e.must(403, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil) // write is not approve
}

func TestApprovalAndNumbering(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	a := e.invoice(maker, cust, nil, line("A", 1000, 1000))
	b := e.invoice(maker, cust, nil, line("B", 1000, 2000))

	var got Invoice
	e.must(200, boss, "POST", "/v1/invoices/"+id(a.ID)+"/approve", nil, &got)
	if got.Status != "approved" || got.Number == nil || *got.Number != "INV-00001" || got.ApprovedBy == nil || *got.ApprovedBy != "boss" {
		t.Fatalf("approved %+v", got)
	}
	if got.PartySnapshot == nil || got.PartySnapshot.Name != "Acme Fake Ltd" {
		t.Fatalf("snapshot %+v", got.PartySnapshot)
	}
	e.must(200, boss, "POST", "/v1/invoices/"+id(b.ID)+"/approve", nil, &got)
	if *got.Number != "INV-00002" {
		t.Fatalf("second number %v", *got.Number)
	}

	// Immutability, at the API: no edits, no delete, approving twice conflicts.
	e.must(409, maker, "PATCH", "/v1/invoices/"+id(a.ID), obj{"notes": "sneaky"}, nil)
	e.must(409, maker, "DELETE", "/v1/invoices/"+id(a.ID), nil, nil)
	e.must(409, boss, "POST", "/v1/invoices/"+id(a.ID)+"/approve", nil, nil)

	// ... and in the database, even for code (or an operator) that bypasses the API.
	st := e.app.store
	for _, q := range []string{
		`UPDATE invoices SET total_minor = 1 WHERE id = ` + id(a.ID),
		`UPDATE invoices SET party_id = 999 WHERE id = ` + id(a.ID),
		`UPDATE invoices SET number_text = 'INV-99999' WHERE id = ` + id(a.ID),
		`DELETE FROM invoices WHERE id = ` + id(a.ID),
		`UPDATE invoice_lines SET unit_price_minor = 1 WHERE invoice_id = ` + id(a.ID),
		`DELETE FROM invoice_lines WHERE invoice_id = ` + id(a.ID),
		`INSERT INTO invoice_lines (invoice_id, position, description, qty_milli, unit_price_minor, gross_minor, discount_minor, tax_minor, total_minor) VALUES (` + id(a.ID) + `, 9, 'x', 1, 1, 1, 0, 0, 1)`,
	} {
		if _, err := st.db.Exec(q); err == nil || !strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "retention") {
			t.Errorf("%s: want a trigger refusal, got %v", q, err)
		}
	}
	var n int64
	_ = st.db.QueryRow(`SELECT total_minor FROM invoices WHERE id = ?`, a.ID).Scan(&n)
	if n != 1000 {
		t.Fatalf("total changed to %d", n)
	}
}

// Numbers are gap-free per series: a rolled-back approval gives its number back, deleted
// drafts never had one, and concurrent approvals get distinct consecutive numbers.
func TestNumberingGapFree(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")

	// A failing approval (credit note larger than what is left) must not burn a number.
	orig := e.invoice(maker, cust, nil, line("A", 1000, 1000))
	e.must(200, boss, "POST", "/v1/invoices/"+id(orig.ID)+"/approve", nil, nil) // INV-00001
	var cn Invoice
	e.must(201, maker, "POST", "/v1/invoices/"+id(orig.ID)+"/credit-notes", obj{"reason": "too big", "lines": []obj{line("c", 1000, 5000)}}, &cn)
	e.must(422, boss, "POST", "/v1/invoices/"+id(cn.ID)+"/approve", nil, nil)

	d := e.invoice(maker, cust, nil, line("gone", 1000, 1))
	e.must(204, maker, "DELETE", "/v1/invoices/"+id(d.ID), nil, nil)

	const N = 20
	ids := make([]int64, N)
	for i := range ids {
		ids[i] = e.invoice(maker, cust, nil, line("x", 1000, int64(100+i))).ID
	}
	var wg sync.WaitGroup
	nums := make([]string, N)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var v Invoice
			if code := e.do(boss, "POST", "/v1/invoices/"+id(ids[i])+"/approve", nil, &v); code != 200 {
				t.Errorf("approve %d = %d", i, code)
				return
			}
			nums[i] = *v.Number
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, n := range nums {
		seen[n] = true
	}
	for i := 2; i <= N+1; i++ { // INV-00001 was the original
		if !seen[fmt.Sprintf("INV-%05d", i)] {
			t.Errorf("INV-%05d missing: numbers are %v", i, nums)
		}
	}
	// A different series counts on its own.
	other := e.invoice(maker, cust, obj{"series": "PRO"}, line("p", 1000, 1))
	var o Invoice
	e.must(200, boss, "POST", "/v1/invoices/"+id(other.ID)+"/approve", nil, &o)
	if *o.Number != "PRO-00001" {
		t.Fatalf("series number %s", *o.Number)
	}
}

func TestMultiCurrencyAndFX(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")

	// A non-base currency needs a rate; the base currency does not accept another one.
	e.must(422, maker, "POST", "/v1/invoices", obj{"party_id": cust, "currency": "EUR", "lines": []obj{line("x", 1000, 100)}}, nil)
	e.must(422, maker, "POST", "/v1/invoices", obj{"party_id": cust, "currency": "USD", "fx_rate": "1.1", "lines": []obj{line("x", 1000, 100)}}, nil)
	e.must(422, maker, "POST", "/v1/invoices", obj{"party_id": cust, "currency": "XXX", "fx_rate": "1", "lines": []obj{line("x", 1000, 100)}}, nil)
	e.must(422, maker, "POST", "/v1/invoices", obj{"party_id": cust, "currency": "EUR", "fx_rate": "1.123456789", "lines": []obj{line("x", 1000, 100)}}, nil)

	var eur Invoice
	e.must(201, maker, "POST", "/v1/invoices", obj{"party_id": cust, "currency": "EUR", "fx_rate": "1.0850", "lines": []obj{line("x", 1000, 12345)}}, &eur)
	// 123.45 EUR x 1.085 = 133.94325 USD -> 13394 cents.
	if eur.TotalMinor != 12345 || eur.BaseTotalMinor != 13394 || eur.FXRate != "1.085" || eur.BaseCurrency != "USD" {
		t.Fatalf("eur %+v", eur)
	}
	// Zero-decimal currency: 10000 JPY at 0.0066 USD = 66.00 USD = 6600 cents.
	var jpy Invoice
	e.must(201, maker, "POST", "/v1/invoices", obj{"party_id": cust, "currency": "JPY", "fx_rate": "0.0066", "lines": []obj{line("x", 1000, 10000)}}, &jpy)
	if jpy.BaseTotalMinor != 6600 {
		t.Fatalf("jpy base %d", jpy.BaseTotalMinor)
	}
	// The rate is stored on the document: changing a draft's rate recomputes only that document.
	var up Invoice
	e.must(200, maker, "PATCH", "/v1/invoices/"+id(eur.ID), obj{"fx_rate": "2"}, &up)
	if up.BaseTotalMinor != 24690 {
		t.Fatalf("repriced %d", up.BaseTotalMinor)
	}
}

func TestPaymentsStatusAndEvents(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	inv := e.invoice(maker, cust, nil, line("A", 1000, 10000))
	e.must(409, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 100}, nil) // draft cannot be paid
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	e.must(200, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/send", nil, nil)
	e.must(200, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/send", nil, nil) // idempotent: still one event
	if evs := e.outbox("invoice.sent.v1"); len(evs) != 1 {
		t.Fatalf("sent events %d", len(evs))
	}

	var pay Payment
	var got Invoice
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 4000, "method": "bank", "reference": "R1"}, &pay, "Idempotency-Key", "pay-key-0001")
	e.must(200, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 4000, "method": "bank", "reference": "R1"}, &pay, "Idempotency-Key", "pay-key-0001") // replay
	e.must(422, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 4000}, nil, "Idempotency-Key", "pay-key-0001")                                       // key reused, other body... same body hash differs? currency empty vs method
	e.must(200, maker, "GET", "/v1/invoices/"+id(inv.ID), nil, &got)
	if got.Status != "partially_paid" || got.PaidMinor != 4000 || got.BalanceMinor != 6000 {
		t.Fatalf("after first payment %+v", got)
	}
	e.must(422, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 6001}, nil)                   // overpayment
	e.must(422, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 100, "currency": "EUR"}, nil) // wrong currency
	e.must(422, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": -5}, nil)
	if len(e.outbox("invoice.paid.v1")) != 0 {
		t.Fatal("paid event too early")
	}
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 6000}, nil)
	e.must(200, maker, "GET", "/v1/invoices/"+id(inv.ID), nil, &got)
	if got.Status != "paid" || got.BalanceMinor != 0 || got.PaidAt == nil {
		t.Fatalf("after full payment %+v", got)
	}
	evs := e.outbox("invoice.paid.v1")
	if len(evs) != 1 {
		t.Fatalf("paid events %d", len(evs))
	}
	data := evs[0]["data"].(map[string]any)
	if data["paid_minor"].(float64) != 10000 || data["number"] != "INV-00001" {
		t.Fatalf("paid event %v", data)
	}
	if _, has := data["name"]; has {
		t.Fatal("events carry no names")
	}
	// A paid invoice cannot be paid again or voided; payments are append-only in the database.
	e.must(409, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 1}, nil)
	e.must(409, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/void", obj{"reason": "oops"}, nil)
	if _, err := e.app.store.db.Exec(`UPDATE payments SET amount_minor = 1`); err == nil {
		t.Error("payments must be append-only")
	}
	if _, err := e.app.store.db.Exec(`DELETE FROM payments`); err == nil {
		t.Error("payments must not be deletable")
	}
}

func TestVoidAndCreditNotes(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")

	v1 := e.invoice(maker, cust, nil, line("A", 1000, 10000))
	e.must(409, boss, "POST", "/v1/invoices/"+id(v1.ID)+"/void", obj{"reason": "draft"}, nil) // delete drafts instead
	e.must(200, boss, "POST", "/v1/invoices/"+id(v1.ID)+"/approve", nil, nil)
	e.must(422, boss, "POST", "/v1/invoices/"+id(v1.ID)+"/void", obj{}, nil) // reason required
	e.must(403, maker, "POST", "/v1/invoices/"+id(v1.ID)+"/void", obj{"reason": "x"}, nil)
	var voided Invoice
	e.must(200, boss, "POST", "/v1/invoices/"+id(v1.ID)+"/void", obj{"reason": "issued to the wrong customer"}, &voided)
	if voided.Status != "void" || voided.Number == nil || voided.VoidReason == "" {
		t.Fatalf("void %+v", voided)
	}
	e.must(409, boss, "POST", "/v1/invoices/"+id(v1.ID)+"/void", obj{"reason": "again"}, nil)
	e.must(409, maker, "POST", "/v1/invoices/"+id(v1.ID)+"/credit-notes", obj{"reason": "r", "lines": []obj{line("c", 1000, 1)}}, nil) // void invoices cannot be credited
	// the next invoice keeps counting: the voided number is not reused
	n := e.invoice(maker, cust, nil, line("B", 1000, 5000))
	var nv Invoice
	e.must(200, boss, "POST", "/v1/invoices/"+id(n.ID)+"/approve", nil, &nv)
	if *nv.Number != "INV-00002" {
		t.Fatalf("number after void %s", *nv.Number)
	}

	// Part payment, then a credit note for the rest settles the invoice.
	e.must(201, maker, "POST", "/v1/invoices/"+id(n.ID)+"/payments", obj{"amount_minor": 2000}, nil)
	e.must(409, boss, "POST", "/v1/invoices/"+id(n.ID)+"/void", obj{"reason": "no, credit it"}, nil) // has payments
	var cn Invoice
	e.must(201, maker, "POST", "/v1/invoices/"+id(n.ID)+"/credit-notes", obj{"reason": "goodwill", "lines": []obj{line("Credit", 1000, 3000)}}, &cn)
	if cn.Kind != "credit_note" || cn.OriginalID == nil || *cn.OriginalID != n.ID || cn.Status != "draft" {
		t.Fatalf("credit note %+v", cn)
	}
	var cnA Invoice
	e.must(200, boss, "POST", "/v1/invoices/"+id(cn.ID)+"/approve", nil, &cnA)
	if !strings.HasPrefix(*cnA.Number, "CN-") {
		t.Fatalf("credit note number %s", *cnA.Number)
	}
	var settled Invoice
	e.must(200, maker, "GET", "/v1/invoices/"+id(n.ID), nil, &settled)
	if settled.Status != "paid" || settled.CreditedMinor != 3000 || settled.BalanceMinor != 0 {
		t.Fatalf("settled %+v", settled)
	}
	e.must(409, boss, "POST", "/v1/invoices/"+id(cn.ID)+"/void", obj{"reason": "x"}, nil) // credit notes are not voided
}

func TestDraftEditingValidation(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	cust := e.party(maker, "Acme Fake Ltd", "customer")
	vend := e.party(maker, "Vendor Fake Ltd", "vendor")
	bad := []obj{
		{"party_id": cust, "lines": []obj{}},
		{"party_id": cust, "lines": []obj{line("x", 0, 100)}},
		{"party_id": cust, "lines": []obj{line("x", 1000, -1)}},
		{"party_id": cust, "lines": []obj{{"description": "x", "quantity_milli": 1000, "unit_price_minor": 1, "discount_bp": 10001}}},
		{"party_id": cust, "lines": []obj{{"description": "x", "quantity_milli": 1000, "unit_price_minor": 1, "tax_code": "NOPE"}}},
		{"party_id": cust, "lines": []obj{line("", 1000, 1)}},
		{"party_id": cust, "lines": []obj{line("two\nlines", 1000, 1)}},
		{"party_id": cust, "due_date": "2026-01-01", "issue_date": "2026-02-01", "lines": []obj{line("x", 1000, 1)}},
		{"party_id": cust, "series": "inv", "lines": []obj{line("x", 1000, 1)}},
		{"party_id": 99999, "lines": []obj{line("x", 1000, 1)}},
		{"party_id": vend, "lines": []obj{line("x", 1000, 1)}},
		{"party_id": cust, "lines": []obj{line("x", 1000, 1<<50), line("y", 1000, 1<<50), line("z", 1000, 1<<50)}},
	}
	for i, b := range bad {
		if code := e.do(maker, "POST", "/v1/invoices", b, nil); code != 422 {
			t.Errorf("case %d: %d, want 422", i, code)
		}
	}
	// mass assignment is refused
	for _, f := range []string{"status", "number", "total_minor", "created_by", "version", "approved_by"} {
		if code := e.do(maker, "POST", "/v1/invoices", obj{"party_id": cust, "lines": []obj{line("x", 1000, 1)}, f: "x"}, nil); code != 400 {
			t.Errorf("field %s: %d, want 400", f, code)
		}
	}
	// idempotent create
	body := obj{"party_id": cust, "lines": []obj{line("x", 1000, 777)}}
	var a, b Invoice
	e.must(201, maker, "POST", "/v1/invoices", body, &a, "Idempotency-Key", "inv-key-00001")
	e.must(200, maker, "POST", "/v1/invoices", body, &b, "Idempotency-Key", "inv-key-00001")
	if a.ID != b.ID {
		t.Fatal("replay created a second invoice")
	}
	e.must(422, maker, "POST", "/v1/invoices", obj{"party_id": cust, "lines": []obj{line("x", 1000, 778)}}, nil, "Idempotency-Key", "inv-key-00001")
}
