package finance

import (
	"encoding/csv"
	"strings"
	"testing"
)

func (e *env) approveInvoice(maker, boss string, party int64, extra obj, lines ...obj) *Invoice {
	e.t.Helper()
	inv := e.invoice(maker, party, extra, lines...)
	var got Invoice
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, &got)
	return &got
}

func manual(date, cur string, debitCode, creditCode string, amt int64) obj {
	return obj{"entry_date": date, "currency": cur, "memo": "adjust", "lines": []obj{
		{"account_code": debitCode, "debit_minor": amt}, {"account_code": creditCode, "credit_minor": amt}}}
}

func TestLedgerRoutesRefuseScopedAndOwnOnlyTokens(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	party := e.party(adminTok, "Scope Co", "customer")
	tokens := map[string]string{
		"party-scoped":   e.token("u-party", ledAdm, []int64{party}, nil, false),
		"project-scoped": e.token("u-proj", ledAdm, nil, []string{"ALPHA"}, false),
		"own-only":       e.token("u-own", ledAdm, nil, nil, true),
		"no-ledger-perm": e.token("u-plain", []string{"finance.read", "finance.write", "finance.approve"}, nil, nil, false),
	}
	routes := []string{"/v1/ledger/accounts", "/v1/ledger/mappings", "/v1/ledger/journals", "/v1/ledger/journals/1", "/v1/ledger/reports/trial-balance?as_of=2026-12-31",
		"/v1/ledger/reports/profit-loss?from=2026-01-01&to=2026-12-31", "/v1/ledger/reports/balance-sheet?as_of=2026-12-31", "/v1/ledger/periods", "/v1/ledger/exports",
		"/v1/bank/accounts", "/v1/bank/accounts/1", "/v1/bank/transactions", "/v1/bank/accounts/1/reconciliation", "/v1/bank/transactions/1/suggestions"}
	for who, tok := range tokens {
		for _, r := range routes {
			if code := e.do(tok, "GET", r, nil, nil); code != 403 {
				t.Errorf("%s: GET %s = %d, want 403", who, r, code)
			}
		}
		for _, r := range []string{"/v1/ledger/journals", "/v1/ledger/exports", "/v1/ledger/accounts", "/v1/bank/accounts", "/v1/bank/transactions/1/confirm"} {
			if code := e.do(tok, "POST", r, obj{}, nil); code != 403 {
				t.Errorf("%s: POST %s = %d, want 403", who, r, code)
			}
		}
	}
	good := e.token("u-good", ledAdm, nil, nil, false)
	for _, r := range []string{"/v1/ledger/accounts", "/v1/ledger/mappings", "/v1/ledger/journals", "/v1/ledger/periods", "/v1/bank/accounts"} {
		if code := e.do(good, "GET", r, nil, nil); code != 200 {
			t.Errorf("unscoped ledger token: GET %s = %d", r, code)
		}
	}
	// A project- or party-scoped finance.admin token (a limited admin) is no accountant either.
	scopedAdmin := e.token("u-sadmin", []string{"finance.admin"}, []int64{party}, nil, false)
	if code := e.do(scopedAdmin, "GET", "/v1/ledger/journals", nil, nil); code != 403 {
		t.Errorf("party-scoped finance.admin read the journal: %d", code)
	}
	// An unknown id is a 404 for an authorised caller, never a different answer that tells ids apart.
	if code := e.do(good, "GET", "/v1/ledger/journals/9999", nil, nil); code != 404 {
		t.Errorf("unknown journal = %d", code)
	}
}

func TestManualJournalNeedsAnotherApprover(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	drafter := e.token("dana", acct, nil, nil, false)
	both := e.token("both", []string{"finance.ledger.write", "finance.ledger.approve"}, nil, nil, false)
	approver := e.token("alex", ledApp, nil, nil, false)

	var j JournalEntry
	e.must(201, drafter, "POST", "/v1/ledger/journals", manual("2026-03-10", "USD", "5000", "1000", 12345), &j, "Idempotency-Key", "journal-key-0001")
	if j.Status != "draft" || j.Number != nil {
		t.Fatalf("draft = %+v", j)
	}
	var again JournalEntry
	if code := e.do(drafter, "POST", "/v1/ledger/journals", manual("2026-03-10", "USD", "5000", "1000", 12345), &again, "Idempotency-Key", "journal-key-0001"); code != 200 || again.ID != j.ID {
		t.Errorf("idempotent retry = %d id %d, want 200 id %d", code, again.ID, j.ID)
	}
	path := "/v1/ledger/journals/" + id(j.ID)
	if code := e.do(drafter, "POST", path+"/post", nil, nil); code != 403 {
		t.Errorf("a drafter without ledger.approve posted: %d", code)
	}
	// Holding both permissions does not let one person do both halves.
	var mine JournalEntry
	e.must(201, both, "POST", "/v1/ledger/journals", manual("2026-03-10", "USD", "5000", "1000", 700), &mine)
	if code := e.do(both, "POST", "/v1/ledger/journals/"+id(mine.ID)+"/post", nil, nil); code != 403 {
		t.Errorf("poster approved their own journal: %d", code)
	}
	var posted JournalEntry
	e.must(200, approver, "POST", path+"/post", nil, &posted)
	if posted.Status != "posted" || posted.Number == nil || *posted.Number != "JE-00001" || posted.PostedBy == nil || *posted.PostedBy != "alex" {
		t.Fatalf("posted = %+v", posted)
	}
	if code := e.do(approver, "POST", path+"/post", nil, nil); code != 409 {
		t.Errorf("posting twice = %d", code)
	}
	if code := e.do(drafter, "DELETE", path, nil, nil); code != 409 {
		t.Errorf("deleting a posted entry = %d", code)
	}
	for name, body := range map[string]obj{
		"unbalanced":    {"entry_date": "2026-03-10", "currency": "USD", "lines": []obj{{"account_code": "5000", "debit_minor": 100}, {"account_code": "1000", "credit_minor": 99}}},
		"one line":      {"entry_date": "2026-03-10", "currency": "USD", "lines": []obj{{"account_code": "5000", "debit_minor": 100}}},
		"both sides":    {"entry_date": "2026-03-10", "currency": "USD", "lines": []obj{{"account_code": "5000", "debit_minor": 100, "credit_minor": 100}, {"account_code": "1000", "credit_minor": 0}}},
		"negative":      {"entry_date": "2026-03-10", "currency": "USD", "lines": []obj{{"account_code": "5000", "debit_minor": -5}, {"account_code": "1000", "credit_minor": -5}}},
		"unknown acct":  {"entry_date": "2026-03-10", "currency": "USD", "lines": []obj{{"account_code": "9999", "debit_minor": 5}, {"account_code": "1000", "credit_minor": 5}}},
		"bad date":      {"entry_date": "2026-13-10", "currency": "USD", "lines": []obj{{"account_code": "5000", "debit_minor": 5}, {"account_code": "1000", "credit_minor": 5}}},
		"unknown cur":   {"entry_date": "2026-03-10", "currency": "XXX", "lines": []obj{{"account_code": "5000", "debit_minor": 5}, {"account_code": "1000", "credit_minor": 5}}},
		"float amounts": {"entry_date": "2026-03-10", "currency": "USD", "lines": []obj{{"account_code": "5000", "debit_minor": 1.5}, {"account_code": "1000", "credit_minor": 1.5}}},
	} {
		if code := e.do(drafter, "POST", "/v1/ledger/journals", body, nil); code != 422 && code != 400 {
			t.Errorf("%s: %d, want 422 or 400", name, code)
		}
	}

	// Reversal: by someone other than the drafter, once, in the same or a later period.
	if code := e.do(drafter, "POST", path+"/reverse", obj{"date": "2026-03-11", "reason": "typo"}, nil); code != 403 {
		t.Errorf("reversal without ledger.approve = %d", code)
	}
	var rev JournalEntry
	e.must(201, approver, "POST", path+"/reverse", obj{"date": "2026-03-11", "reason": "typo"}, &rev)
	if rev.ReversalOf == nil || *rev.ReversalOf != j.ID || rev.Source != "reversal" || rev.TotalMinor != 12345 {
		t.Fatalf("reversal = %+v", rev)
	}
	if code := e.do(approver, "POST", path+"/reverse", obj{"date": "2026-03-11", "reason": "again"}, nil); code != 409 {
		t.Errorf("reversing twice = %d", code)
	}
	if code := e.do(approver, "POST", "/v1/ledger/journals/"+id(rev.ID)+"/reverse", obj{"date": "2026-03-12", "reason": "undo"}, nil); code != 409 && code != 403 {
		t.Errorf("reversing a reversal = %d", code)
	}
	var tb TrialBalance
	e.must(200, approver, "GET", "/v1/ledger/reports/trial-balance?as_of=2026-12-31", nil, &tb)
	for _, c := range tb.Currencies {
		for _, r := range c.Rows {
			if r.BalanceMinor != 0 {
				t.Errorf("%s %s balance %d after the reversal", c.Currency, r.Code, r.BalanceMinor)
			}
		}
	}
	// Every step is in the audit log.
	var aud struct{ Items []AuditEntry }
	e.must(200, adminTok, "GET", "/v1/audit?limit=100", nil, &aud)
	seen := map[string]bool{}
	for _, a := range aud.Items {
		seen[a.Action] = true
	}
	for _, want := range []string{"journal.draft", "journal.post", "journal.reverse"} {
		if !seen[want] {
			t.Errorf("no audit entry %s", want)
		}
	}
}

func TestPeriodCloseLocksTheLedgerAndReopenIsAudited(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	drafter := e.token("dana", acct, nil, nil, false)
	approver := e.token("alex", ledApp, nil, nil, false)
	admin := e.token("ada", ledAdm, nil, nil, false)
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	party := e.party(maker, "Closed Co", "customer")

	if code := e.do(drafter, "POST", "/v1/ledger/periods/close", obj{"closed_through": "2026-03-31"}, nil); code != 403 {
		t.Errorf("close without ledger.admin = %d", code)
	}
	var draft JournalEntry
	e.must(201, drafter, "POST", "/v1/ledger/journals", manual("2026-03-15", "USD", "5000", "1000", 100), &draft)
	if code := e.do(admin, "POST", "/v1/ledger/periods/close", obj{"closed_through": "2026-03-31"}, nil); code != 409 {
		t.Errorf("closing over a draft = %d, want 409", code)
	}
	e.must(204, drafter, "DELETE", "/v1/ledger/journals/"+id(draft.ID), nil, nil)
	e.must(200, admin, "POST", "/v1/ledger/periods/close", obj{"closed_through": "2026-03-31"}, nil)

	// Nothing dated in the closed period can be drafted-and-posted, auto-posted, or reversed into it.
	var d2 JournalEntry
	e.must(201, drafter, "POST", "/v1/ledger/journals", manual("2026-03-20", "USD", "5000", "1000", 100), &d2)
	if code := e.do(approver, "POST", "/v1/ledger/journals/"+id(d2.ID)+"/post", nil, nil); code != 409 {
		t.Errorf("posting into a closed period = %d, want 409", code)
	}
	inv := e.invoice(maker, party, obj{"issue_date": "2026-03-05"}, line("x", 1000, 1000))
	if code := e.do(boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, nil); code != 409 {
		t.Errorf("approving an invoice dated in the closed period = %d, want 409", code)
	}
	var again Invoice
	e.must(200, maker, "GET", "/v1/invoices/"+id(inv.ID), nil, &again)
	if again.Status != "draft" {
		t.Errorf("the failed approval left the invoice %s", again.Status)
	}
	// The database refuses even if the store is bypassed.
	if _, err := e.app.store.db.Exec(`UPDATE journal_entries SET status='posted', number=99, number_text='JE-99' WHERE id = ?`, d2.ID); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("the trigger let a closed-period entry post: %v", err)
	}
	// A close can only move forward; reopening needs admin and a reason, and is audited.
	if code := e.do(admin, "POST", "/v1/ledger/periods/close", obj{"closed_through": "2026-02-28"}, nil); code != 409 {
		t.Errorf("moving the close back through close = %d", code)
	}
	if code := e.do(drafter, "POST", "/v1/ledger/periods/reopen", obj{"closed_through": "2026-02-28", "reason": "fix"}, nil); code != 403 {
		t.Errorf("reopen without ledger.admin = %d", code)
	}
	if code := e.do(admin, "POST", "/v1/ledger/periods/reopen", obj{"closed_through": "2026-02-28"}, nil); code != 422 {
		t.Errorf("reopen without a reason = %d", code)
	}
	e.must(200, admin, "POST", "/v1/ledger/periods/reopen", obj{"closed_through": "2026-02-28", "reason": "late supplier invoice"}, nil)
	e.must(200, approver, "POST", "/v1/ledger/journals/"+id(d2.ID)+"/post", nil, nil)
	var per struct {
		Through *string      `json:"closed_through"`
		History []CloseEvent `json:"history"`
	}
	e.must(200, admin, "GET", "/v1/ledger/periods", nil, &per)
	if per.Through == nil || *per.Through != "2026-02-28" || len(per.History) != 2 || per.History[0].Action != "reopen" || per.History[0].Reason == "" {
		t.Errorf("periods = %+v", per)
	}
	var aud struct{ Items []AuditEntry }
	e.must(200, adminTok, "GET", "/v1/audit?limit=50", nil, &aud)
	found := false
	for _, a := range aud.Items {
		found = found || a.Action == "ledger.period.reopen" && a.Actor == "ada"
	}
	if !found {
		t.Error("the reopen is not in the audit log")
	}
	// The close history cannot be edited.
	if _, err := e.app.store.db.Exec(`UPDATE ledger_closes SET closed_through = '' WHERE id = 1`); err == nil {
		t.Error("the close history was edited")
	}
}

func TestMappingsAreValidated(t *testing.T) {
	e := newEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	e.must(200, admin, "POST", "/v1/ledger/accounts/template", nil, nil)
	var m Mappings
	e.must(200, admin, "GET", "/v1/ledger/mappings", nil, &m)
	if m.Enabled || len(m.Missing) != 6 {
		t.Fatalf("a fresh ledger has mappings: %+v", m)
	}
	// Without mappings nothing posts: finance behaves as before.
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	party := e.party(maker, "Plain Co", "customer")
	e.approveInvoice(maker, boss, party, nil, line("x", 1000, 5000))
	var js struct{ Items []JournalEntry }
	e.must(200, admin, "GET", "/v1/ledger/journals", nil, &js)
	if len(js.Items) != 0 {
		t.Fatalf("posted with no mappings: %+v", js.Items)
	}
	for name, mp := range map[string]map[string]string{
		"wrong type":  {"ar": "4000"},
		"missing":     {"ar": "7777"},
		"unknown key": {"bogus": "1100"},
		"empty":       {},
	} {
		if code := e.do(admin, "PUT", "/v1/ledger/mappings", obj{"mappings": mp}, nil); code != 422 {
			t.Errorf("%s: %d, want 422", name, code)
		}
	}
	all := map[string]string{"cash": "1000", "ar": "1100", "tax_payable": "2100", "expense_payable": "2200", "income": "4000", "expense": "5000"}
	e.must(200, admin, "PUT", "/v1/ledger/mappings", obj{"mappings": all}, &m)
	if !m.Enabled {
		t.Fatalf("not enabled: %+v", m)
	}
	var accts struct{ Items []Account }
	e.must(200, admin, "GET", "/v1/ledger/accounts", nil, &accts)
	var arID int64
	for _, a := range accts.Items {
		if a.Code == "1100" {
			arID = a.ID
		}
	}
	if code := e.do(admin, "PATCH", "/v1/ledger/accounts/"+id(arID), obj{"active": false}, nil); code != 409 {
		t.Errorf("deactivating a mapped account = %d, want 409", code)
	}
	// Used accounts keep their code and type.
	e.approveInvoice(maker, boss, party, nil, line("y", 1000, 5000))
	if code := e.do(admin, "PATCH", "/v1/ledger/accounts/"+id(arID), obj{"code": "1101"}, nil); code != 409 {
		t.Errorf("renumbering a used account = %d, want 409", code)
	}
	if code := e.do(admin, "PATCH", "/v1/ledger/accounts/"+id(arID), obj{"name": "Debtors"}, nil); code != 200 {
		t.Errorf("renaming a used account = %d", code)
	}
	if code := e.do(admin, "POST", "/v1/ledger/accounts", obj{"code": "1100", "name": "Dup", "type": "asset"}, nil); code != 409 {
		t.Errorf("duplicate code = %d", code)
	}
	if code := e.do(admin, "POST", "/v1/ledger/accounts", obj{"code": "6000", "name": "Bad type", "type": "revenue"}, nil); code != 422 {
		t.Errorf("unknown type = %d", code)
	}
	if code := e.do(admin, "POST", "/v1/ledger/accounts", obj{"code": "6001", "name": "Bad tax", "type": "liability", "tax_code": "NOPE"}, nil); code != 422 {
		t.Errorf("unknown tax code = %d", code)
	}
}

func TestFinanceEventsPostToTheLedger(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	reader := e.token("rita", []string{"finance.read", "finance.ledger.read"}, nil, nil, false)
	party := e.party(maker, "Flow Co", "customer")
	l1 := line("work", 2000, 10000)
	l1["tax_code"] = "VAT"
	inv := e.approveInvoice(maker, boss, party, obj{"issue_date": "2026-04-01"}, l1, line("untaxed", 1000, 500))
	var tb TrialBalance
	bal := func(code string) int64 {
		e.must(200, reader, "GET", "/v1/ledger/reports/trial-balance?as_of=2026-12-31", nil, &tb)
		for _, c := range tb.Currencies {
			for _, r := range c.Rows {
				if r.Code == code {
					return r.BalanceMinor
				}
			}
		}
		return 0
	}
	if got := bal("1100"); got != inv.TotalMinor {
		t.Errorf("receivable %d, want invoice total %d", got, inv.TotalMinor)
	}
	if got := bal("2100"); got != -inv.TaxMinor {
		t.Errorf("tax payable %d, want %d credit", got, inv.TaxMinor)
	}
	if got := bal("4000"); got != -(inv.SubtotalMinor - inv.DiscountMinor) {
		t.Errorf("income %d", got)
	}

	// A part payment: bank up, receivable down.
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 5000, "received_on": "2026-04-10"}, nil)
	if bal("1000") != 5000 || bal("1100") != inv.TotalMinor-5000 {
		t.Errorf("after payment bank %d receivable %d", bal("1000"), bal("1100"))
	}
	// A credit note reduces receivable and income/tax pro rata to its own lines.
	var cn Invoice
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/credit-notes", obj{"reason": "returned", "lines": []obj{line("returned", 1000, 500)}}, &cn)
	e.must(200, boss, "POST", "/v1/invoices/"+id(cn.ID)+"/approve", nil, &cn)
	if got := bal("1100"); got != inv.TotalMinor-5000-cn.TotalMinor {
		t.Errorf("receivable after credit note %d", got)
	}
	// Voiding an unpaid invoice reverses its entry.
	inv2 := e.approveInvoice(maker, boss, party, nil, line("void me", 1000, 7000))
	before := bal("1100")
	e.must(200, boss, "POST", "/v1/invoices/"+id(inv2.ID)+"/void", obj{"reason": "mistake"}, nil)
	if got := bal("1100"); got != before-inv2.TotalMinor {
		t.Errorf("receivable after void %d, want %d", got, before-inv2.TotalMinor)
	}
	// An expense posts on approval and again when reimbursed.
	emp := e.token("person_9", rw, nil, nil, true)
	var ex Expense
	e.must(201, emp, "POST", "/v1/expenses", obj{"description": "Train", "amount_minor": 4500, "currency": "USD", "category": "travel", "incurred_on": "2026-04-12"}, &ex)
	e.must(200, emp, "POST", "/v1/expenses/"+id(ex.ID)+"/submit", nil, nil)
	e.must(200, boss, "POST", "/v1/expenses/"+id(ex.ID)+"/approve", nil, nil)
	if bal("5000") != 4500 || bal("2200") != -4500 {
		t.Errorf("expense: costs %d payable %d", bal("5000"), bal("2200"))
	}
	e.must(200, maker, "POST", "/v1/expenses/"+id(ex.ID)+"/reimburse", obj{"reference": "TX1"}, nil)
	if bal("2200") != 0 {
		t.Errorf("payable after reimbursement %d", bal("2200"))
	}
	// Reports tie out.
	var bs BalanceSheet
	e.must(200, reader, "GET", "/v1/ledger/reports/balance-sheet?as_of=2026-12-31", nil, &bs)
	for _, c := range bs.Currencies {
		if !c.Balanced {
			t.Errorf("balance sheet %s does not balance: %+v", c.Currency, c)
		}
	}
	var pl ProfitLoss
	e.must(200, reader, "GET", "/v1/ledger/reports/profit-loss?from=2026-04-01&to=2026-04-30", nil, &pl)
	if len(pl.Currencies) != 1 || pl.Currencies[0].NetMinor != pl.Currencies[0].IncomeMinor-pl.Currencies[0].ExpensesMinor || pl.Currencies[0].ExpensesMinor != 4500 {
		t.Errorf("profit and loss %+v", pl)
	}
	var gl GeneralLedger
	e.must(200, reader, "GET", "/v1/ledger/reports/general-ledger?account=1100&from=2026-01-01&to=2026-12-31&limit=2", nil, &gl)
	if len(gl.Lines) != 2 || gl.NextCursor == "" || gl.Lines[0].BalanceMinor != inv.TotalMinor {
		t.Fatalf("general ledger page 1 = %+v", gl)
	}
	var gl2 GeneralLedger
	e.must(200, reader, "GET", "/v1/ledger/reports/general-ledger?account=1100&from=2026-01-01&to=2026-12-31&limit=2&cursor="+gl.NextCursor, nil, &gl2)
	if len(gl2.Lines) == 0 || gl2.Lines[0].BalanceMinor != gl.Lines[1].BalanceMinor+gl2.Lines[0].DebitMinor-gl2.Lines[0].CreditMinor {
		t.Errorf("general ledger running balance breaks across pages: %+v then %+v", gl.Lines, gl2.Lines)
	}
	// Events carry ids and counts only.
	evs := e.outbox("journal.posted.v1")
	if len(evs) < 5 {
		t.Fatalf("journal.posted.v1 events: %d", len(evs))
	}
	for _, ev := range evs {
		b := strings.ToLower(toJSON(ev))
		for _, bad := range []string{"flow co", "memo", "description", "@"} {
			if strings.Contains(b, bad) {
				t.Errorf("event leaks %q: %s", bad, b)
			}
		}
	}
}

func TestJournalExportIsIdempotentAndSafe(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	drafter := e.token("dana", acct, nil, nil, false)
	approver := e.token("alex", ledApp, nil, nil, false)
	post := func(memo string, amt int64) {
		body := manual("2026-05-01", "USD", "5000", "1000", amt)
		body["memo"] = memo
		var j JournalEntry
		e.must(201, drafter, "POST", "/v1/ledger/journals", body, &j)
		e.must(200, approver, "POST", "/v1/ledger/journals/"+id(j.ID)+"/post", nil, nil)
	}
	post("=HYPERLINK(\"http://x\")", 100)
	post("plain", 250)
	var b1, b2, b3 ExportBatch
	e.must(201, drafter, "POST", "/v1/ledger/exports", obj{"adapter": "csv"}, &b1, "Idempotency-Key", "export-key-0001")
	if b1.EntryCount != 2 || b1.ID == 0 {
		t.Fatalf("first batch %+v", b1)
	}
	var replay ExportBatch
	if code := e.do(drafter, "POST", "/v1/ledger/exports", obj{"adapter": "csv"}, &replay, "Idempotency-Key", "export-key-0001"); code != 200 || replay.ID != b1.ID {
		t.Errorf("replay = %d id %d, want 200 id %d", code, replay.ID, b1.ID)
	}
	e.must(201, drafter, "POST", "/v1/ledger/exports", obj{"adapter": "csv"}, &b2) // no key: still nothing to export twice
	if b2.EntryCount != 0 || b2.ID != 0 {
		t.Errorf("second export re-exported: %+v", b2)
	}
	post("later", 999)
	e.must(201, drafter, "POST", "/v1/ledger/exports", obj{"adapter": "csv"}, &b3)
	if b3.EntryCount != 1 || b3.FirstEntry <= b1.LastEntry {
		t.Errorf("third batch %+v after %+v", b3, b1)
	}
	code, hdr, body := e.req(drafter, "GET", "/v1/ledger/exports/"+id(b1.ID)+"/download", nil)
	if code != 200 || hdr.Get("X-Content-SHA256") != b1.SHA256 {
		t.Fatalf("download = %d sha %s want %s", code, hdr.Get("X-Content-SHA256"), b1.SHA256)
	}
	recs, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil || len(recs) != 5 { // header + 2 entries x 2 lines
		t.Fatalf("csv: %v, %d rows", err, len(recs))
	}
	for _, r := range recs[1:] {
		if strings.HasPrefix(r[4], "=") || strings.HasPrefix(r[4], "+") {
			t.Errorf("formula injection in the memo cell: %q", r[4])
		}
	}
	if recs[1][9] != "1.00" || recs[2][10] != "1.00" {
		t.Errorf("amounts: %q %q", recs[1][9], recs[2][10])
	}
	code2, _, body2 := e.req(drafter, "GET", "/v1/ledger/exports/"+id(b1.ID)+"/download", nil)
	if code2 != 200 || string(body2) != string(body) {
		t.Error("a re-download is not byte for byte the same")
	}
	// The documented stubs say so and touch nothing.
	for _, ad := range []string{"xero", "quickbooks", "wave"} {
		if code := e.do(drafter, "POST", "/v1/ledger/exports", obj{"adapter": ad}, nil); code != 501 {
			t.Errorf("%s = %d, want 501", ad, code)
		}
	}
	if code := e.do(drafter, "POST", "/v1/ledger/exports", obj{"adapter": "nope"}, nil); code != 422 {
		t.Errorf("unknown adapter = %d", code)
	}
	var cur int64
	_ = e.app.store.db.QueryRow(`SELECT COALESCE(MAX(last_entry_id), 0) FROM export_cursors WHERE adapter <> 'csv'`).Scan(&cur)
	if cur != 0 {
		t.Error("a stub moved a cursor")
	}
	if _, err := e.app.store.db.Exec(`UPDATE export_batches SET content = 'x' WHERE id = ?`, b1.ID); err == nil {
		t.Error("an export batch was edited")
	}
	var aud struct{ Items []AuditEntry }
	e.must(200, adminTok, "GET", "/v1/audit?limit=100", nil, &aud)
	seen := map[string]int{}
	for _, a := range aud.Items {
		seen[a.Action]++
	}
	if seen["ledger.export"] != 2 || seen["ledger.export.download"] != 2 {
		t.Errorf("audit actions %v", seen)
	}
}

func TestPayrollImportMakesDraftsOnly(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	drafter := e.token("dana", acct, nil, nil, false)
	approver := e.token("alex", ledApp, nil, nil, false)
	file := "date,account_code,debit,credit,currency,memo\n2026-06-30,5000,1000.00,,USD,Gross wages\n2026-06-30,1000,,1000.00,USD,Net pay\n"
	var res PayrollImportResult
	e.must(201, drafter, "POST", "/v1/ledger/payroll-import?adapter=csv", file, &res)
	if len(res.DraftIDs) != 1 || res.Disclaimer == "" {
		t.Fatalf("result %+v", res)
	}
	var j JournalEntry
	e.must(200, drafter, "GET", "/v1/ledger/journals/"+id(res.DraftIDs[0]), nil, &j)
	if j.Status != "draft" || j.Source != "payroll" || j.TotalMinor != 100000 {
		t.Errorf("draft %+v", j)
	}
	var again PayrollImportResult
	if code := e.do(drafter, "POST", "/v1/ledger/payroll-import?adapter=csv", file, &again); code != 200 || !again.Already || again.DraftIDs[0] != res.DraftIDs[0] {
		t.Errorf("the same file again = %d %+v", code, again)
	}
	if code := e.do(drafter, "POST", "/v1/ledger/journals/"+id(j.ID)+"/post", nil, nil); code != 403 {
		t.Errorf("the importer posted: %d", code)
	}
	e.must(200, approver, "POST", "/v1/ledger/journals/"+id(j.ID)+"/post", nil, nil)
	for name, bad := range map[string]string{
		"unbalanced": "date,account_code,debit,credit,currency,memo\n2026-06-30,5000,10.00,,USD,a\n2026-06-30,1000,,9.00,USD,b\n",
		"header":     "date,account,debit,credit,currency,memo\n",
		"float":      "date,account_code,debit,credit,currency,memo\n2026-06-30,5000,1e3,,USD,a\n2026-06-30,1000,,1e3,USD,b\n",
		"too many":   "date,account_code,debit,credit,currency,memo\n2026-06-30,5000,0.001,,USD,a\n",
	} {
		if code := e.do(drafter, "POST", "/v1/ledger/payroll-import?adapter=csv", bad, nil); code != 422 {
			t.Errorf("%s: %d, want 422", name, code)
		}
	}
}
