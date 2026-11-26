package finance

import (
	"context"
	"testing"
	"time"
)

func TestSummaryCountsByCurrencyWithinScope(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", []string{"finance.read", "finance.approve"}, nil, nil, false)
	a := e.party(maker, "Acme Fake Ltd", "customer")
	b := e.party(maker, "Bolt Fake Ltd", "customer")
	// Overdue (due before today, 2026-11-25), not yet due, and a draft that is nobody's receivable.
	e.approveInvoice(maker, boss, a, obj{"currency": "AUD", "fx_rate": "0.65", "issue_date": "2026-10-01", "due_date": "2026-10-31"}, line("x", 1000, 1000))
	e.approveInvoice(maker, boss, b, obj{"currency": "EUR", "fx_rate": "1.1", "issue_date": "2026-10-01", "due_date": "2026-10-31"}, line("x", 1000, 500))
	e.approveInvoice(maker, boss, a, obj{"currency": "AUD", "fx_rate": "0.65", "issue_date": "2026-11-20", "due_date": "2026-12-20"}, line("x", 1000, 2000))
	e.invoice(maker, a, obj{"currency": "AUD", "fx_rate": "0.65"}, line("x", 1000, 9999))

	contract := func(party int64, end string, send, sign bool) {
		var c Contract
		e.must(201, maker, "POST", "/v1/contracts", obj{"party_id": party, "title": "C", "body": "Plain text.", "start_date": "2026-01-01", "end_date": end}, &c)
		if send {
			e.must(200, maker, "POST", "/v1/contracts/"+id(c.ID)+"/send", obj{"manual": true}, &c)
		}
		if sign {
			e.must(200, maker, "POST", "/v1/contracts/"+id(c.ID)+"/signature", obj{"sha256": c.SHA256}, &c)
		}
	}
	contract(a, "2026-12-31", true, true)  // ends within 60 days
	contract(b, "2027-12-31", true, true)  // ends later
	contract(a, "2027-12-31", true, false) // awaiting signature
	contract(a, "2026-12-31", false, false)

	build := summaryBuilder(e.app.store, func() time.Time { return e.now })
	figures := func(perms ...string) map[string]string {
		t.Helper()
		fs, err := build(context.Background(), "user:alice", perms)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, f := range fs {
			s := id(f.Count)
			for _, m := range f.Amounts {
				s += " " + m.Currency + ":" + id(m.Minor)
			}
			out[f.Key] = s
		}
		return out
	}
	got := figures("finance.read")
	for k, want := range map[string]string{
		"invoices.overdue": "2 AUD:1000 EUR:500", "invoices.outstanding": "3 AUD:3000 EUR:500",
		"contracts.awaiting_signature": "1", "contracts.expiring": "1",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// write, approve and admin carry read; ledger permissions and nothing at all do not.
	for _, p := range []string{"finance.write", "finance.approve", "finance.admin"} {
		if len(figures(p)) != 4 {
			t.Errorf("%s sees nothing", p)
		}
	}
	for _, p := range [][]string{nil, {"finance.ledger.read"}, {"finance.ledger.admin"}, {"work.read"}} {
		if g := figures(p...); len(g) != 0 {
			t.Errorf("%v sees %v", p, g)
		}
	}
	// A party-scoped principal gets only its party's figures.
	st, err := e.app.store.Summary(context.Background(), &Principal{Subject: "x", Permissions: []string{PermRead}, Parties: []int64{b}}, "2026-11-25")
	if err != nil || st.OverdueCount != 1 || len(st.Overdue) != 1 || st.Overdue[0].Currency != "EUR" || st.AwaitingSignature != 0 || st.ContractsSoon != 0 {
		t.Fatalf("%+v %v", st, err)
	}
}
