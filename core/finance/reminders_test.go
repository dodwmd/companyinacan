package finance

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// queued returns the data of the outbox events of one type, oldest first.
func (e *env) queued(typ string) []map[string]any {
	e.t.Helper()
	rows, err := e.app.store.db.Query(`SELECT payload FROM outbox WHERE type = ? ORDER BY id`, typ)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var p string
		var ev struct {
			Data map[string]any `json:"data"`
		}
		if err := rows.Scan(&p); err != nil {
			e.t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(p), &ev); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, ev.Data)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// dueToday issues an invoice due on the clock's date.
func (e *env) dueToday(maker string, party int64) int64 {
	e.t.Helper()
	var inv Invoice
	e.must(201, maker, "POST", "/v1/invoices", obj{"party_id": party, "lines": []obj{line("work", 1000, 5000)}, "due_days": 0}, &inv)
	e.must(200, adminTok, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil)
	return inv.ID
}

func (e *env) sweepOverdue(asOf string) int {
	e.t.Helper()
	var out struct {
		Invoices int `json:"invoices"`
	}
	e.must(200, adminTok, "POST", "/v1/invoices/overdue-sweep", obj{"as_of": asOf}, &out)
	return out.Invoices
}

func TestOverdueReminderOnceAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.db")
	same := func(c *Config, _ *Options) { c.DBPath = path }
	e := newEnv(t, same)
	maker := e.token("maker", []string{"finance.write"}, nil, nil, false)
	party := e.party(adminTok, "Customer One", "customer")
	inv := e.dueToday(maker, party)
	paid := e.dueToday(maker, party)
	e.must(201, maker, "POST", "/v1/invoices/"+id(paid)+"/payments", obj{"amount_minor": 5000}, nil)

	if n := e.sweepOverdue("2026-11-25"); n != 0 {
		t.Fatalf("an invoice due today is not overdue: %d", n)
	}
	if n := e.sweepOverdue("2026-11-28"); n != 1 {
		t.Fatalf("one unpaid overdue invoice: %d", n)
	}
	if n := e.sweepOverdue("2026-11-29"); n != 0 {
		t.Fatalf("announced twice: %d", n)
	}
	if err := e.app.Close(); err != nil {
		t.Fatal(err)
	}
	e = newEnv(t, same) // a restart
	if n := e.sweepOverdue("2026-12-30"); n != 0 {
		t.Fatalf("announced again after a restart: %d", n)
	}
	got := e.queued(EventInvoiceOverdue)
	if len(got) != 1 || got[0]["invoice_id"] != float64(inv) || got[0]["days_overdue"] != float64(3) || got[0]["count"] != nil {
		t.Fatalf("%v", got)
	}
}

func TestOverdueBacklogIsOneEvent(t *testing.T) {
	e := newEnv(t)
	maker := e.token("maker", []string{"finance.write"}, nil, nil, false)
	party := e.party(adminTok, "Customer One", "customer")
	for range summaryAbove + 3 {
		e.dueToday(maker, party)
	}
	if n := e.sweepOverdue("2026-12-01"); n != summaryAbove+3 {
		t.Fatalf("%d", n)
	}
	if n := e.sweepOverdue("2026-12-02"); n != 0 {
		t.Fatalf("again: %d", n)
	}
	got := e.queued(EventInvoiceOverdue)
	if len(got) != 1 || got[0]["count"] != float64(summaryAbove+3) || len(got[0]) != 1 {
		t.Fatalf("%v", got)
	}
	// One more falls due later: it is announced by itself.
	e.dueToday(maker, party)
	if n := e.sweepOverdue("2026-12-03"); n != 1 || len(e.queued(EventInvoiceOverdue)) != 2 {
		t.Fatalf("%d", n)
	}
}

func TestOverdueWithEventsOffMarksNothing(t *testing.T) {
	e := newEnv(t, func(c *Config, _ *Options) { c.Webhook.URL = "" })
	maker := e.token("maker", []string{"finance.write"}, nil, nil, false)
	e.dueToday(maker, e.party(adminTok, "Customer One", "customer"))
	if n := e.sweepOverdue("2026-12-01"); n != 0 {
		t.Fatalf("%d", n)
	}
	var marked int
	if err := e.app.store.db.QueryRow(`SELECT COUNT(*) FROM overdue_notices`).Scan(&marked); err != nil || marked != 0 {
		t.Fatalf("marked %d: %v", marked, err)
	}
}

func TestSubmittedAndDecidedEventsCarryIdsOnly(t *testing.T) {
	e := newEnv(t)
	alice := e.token("user:alice", []string{"finance.write"}, nil, nil, true)
	var ex Expense
	e.must(201, alice, "POST", "/v1/expenses", obj{"description": "Secret trip", "amount_minor": 4500, "currency": "USD", "category": "travel", "incurred_on": "2026-11-20"}, &ex)
	e.must(200, alice, "POST", "/v1/expenses/"+id(ex.ID)+"/submit", nil, nil)
	e.must(200, adminTok, "POST", "/v1/expenses/"+id(ex.ID)+"/reject", obj{"reason": "Secret reason"}, nil)
	for _, typ := range []string{EventExpenseSubmitted, EventExpenseDecided} {
		got := e.queued(typ)
		if len(got) != 1 || len(got[0]) != 2 || got[0]["expense_id"] != float64(ex.ID) || got[0]["owner"] != "user:alice" {
			t.Fatalf("%s: %v", typ, got)
		}
	}
}

func TestDeclinedSigningRaisesAnEventWithoutTheReason(t *testing.T) {
	e := newSignEnv(t)
	e.send(false, ann)
	if w := e.public("POST", e.link(ann[1]), url.Values{"action": {"decline"}, "reason": {"Wrong price"}}); w.Code != 200 || !strings.Contains(w.Body.String(), "You declined") {
		t.Fatalf("%d", w.Code)
	}
	got := e.queued(EventContractDeclined)
	if len(got) != 1 || len(got[0]) != 3 || got[0]["contract_id"] != float64(e.ct.ID) {
		t.Fatalf("%v", got)
	}
}
