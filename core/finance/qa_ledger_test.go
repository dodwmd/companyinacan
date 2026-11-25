package finance

// Regression tests for the E15c QA findings (PR #46), one or more per finding.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// QA 1: a number that is short once normalised panicked inside a transaction, which was never rolled back, so
// the only writer connection leaked and every later write hung.
func TestQAShortBankNumberAndPanicDoNotWedgeTheWriter(t *testing.T) {
	e := sealedEnv(t)
	for _, n := range []string{"1   2", "1 - - - 2", "12 - 34"} {
		if code := e.do(adminTok, "POST", "/v1/bank/accounts", obj{"name": "x", "currency": "USD", "number": n, "ledger_account_code": "1000"}, nil); code != 422 {
			t.Errorf("%q = %d, want 422", n, code)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the test panic did not propagate")
			}
		}()
		_ = e.app.store.inTx(context.Background(), func(*sql.Tx) error { panic("boom") })
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.app.store.inTx(ctx, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("the writer connection is wedged after a panic: %v", err)
	}
	e.must(201, adminTok, "POST", "/v1/parties", obj{"name": "Still Writes", "kind": "customer"}, nil)
}

// QA 2: an entry drafted before an export and posted after it has a lower id than exported entries; the cursor is
// the posting number, so it is still exported.
func TestQADraftPostedAfterAnExportIsStillExported(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	drafter := e.token("dana", acct, nil, nil, false)
	approver := e.token("alex", ledApp, nil, nil, false)
	draft := func(amt int64) int64 {
		var j JournalEntry
		e.must(201, drafter, "POST", "/v1/ledger/journals", manual("2026-05-01", "USD", "5000", "1000", amt), &j)
		return j.ID
	}
	post := func(jid int64) { e.must(200, approver, "POST", "/v1/ledger/journals/"+id(jid)+"/post", nil, nil) }
	late, early := draft(111), draft(222)
	post(early)
	var b ExportBatch
	e.must(201, drafter, "POST", "/v1/ledger/exports", obj{"adapter": "csv"}, &b)
	if b.EntryCount != 1 {
		t.Fatalf("first batch %+v", b)
	}
	post(late) // lower id, higher posting number
	e.must(201, drafter, "POST", "/v1/ledger/exports", obj{"adapter": "csv"}, &b)
	if b.EntryCount != 1 || b.FirstNumber != 2 {
		t.Fatalf("the late-posted draft was not exported: %+v", b)
	}
	_, _, body := e.req(drafter, "GET", "/v1/ledger/exports/"+id(b.ID)+"/download", nil)
	if !strings.Contains(string(body), "1.11") {
		t.Errorf("batch content %s", body)
	}
}

// QA 3: NtryRef restarts in every statement, so it is not a deduplication key.
func TestQACamtNtryRefIsNotADedupKey(t *testing.T) {
	doc := func(date, amt string) string {
		return `<Document><BkToCstmrStmt><Stmt><Ntry><NtryRef>1</NtryRef><Amt Ccy="USD">` + amt + `</Amt><CdtDtInd>CRDT</CdtDtInd><BookgDt><Dt>` + date + `</Dt></BookgDt></Ntry></Stmt></BkToCstmrStmt></Document>`
	}
	a, err := ParseCAMT053(strings.NewReader(doc("2026-06-01", "10.00")), "USD")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ParseCAMT053(strings.NewReader(doc("2026-07-01", "20.00")), "USD")
	c, _ := ParseCAMT053(strings.NewReader(doc("2026-07-01", "10.00")), "USD")
	if a.Txns[0].Key == "1" || a.Txns[0].Key == b.Txns[0].Key || a.Txns[0].Key == c.Txns[0].Key {
		t.Errorf("keys %q %q %q", a.Txns[0].Key, b.Txns[0].Key, c.Txns[0].Key)
	}
}

// QA 4: every statement block is held to one account and currency, and an over-long identifier is refused
// rather than blanked (which skipped the "is this your account" check).
func TestQAMultiAccountAndLongIDStatementsAreRefused(t *testing.T) {
	stmt := func(acct, ccy string) string {
		id := ""
		if acct != "" {
			id = "<Id><IBAN>" + acct + "</IBAN></Id>"
		}
		return "<Stmt><Acct>" + id + "<Ccy>" + ccy + "</Ccy></Acct><Ntry><Amt Ccy=\"USD\">1.00</Amt><CdtDtInd>CRDT</CdtDtInd><BookgDt><Dt>2026-06-01</Dt></BookgDt></Ntry></Stmt>"
	}
	camt := func(s ...string) error {
		_, err := ParseCAMT053(strings.NewReader("<Document><BkToCstmrStmt>"+strings.Join(s, "")+"</BkToCstmrStmt></Document>"), "USD")
		return err
	}
	long := strings.Repeat("A", 65)
	for name, err := range map[string]error{
		"two accounts":   camt(stmt("DE111111", "USD"), stmt("DE222222", "USD")),
		"one unnamed":    camt(stmt("DE111111", "USD"), stmt("", "USD")),
		"two currencies": camt(stmt("DE111111", "USD"), stmt("DE111111", "EUR")),
		"long id":        camt(stmt(long, "USD")),
		"other currency": camt(stmt("", "EUR")),
	} {
		if err == nil {
			t.Errorf("camt %s: accepted", name)
		}
	}
	if err := camt(stmt("DE111111", "USD"), stmt("DE11 1111", "USD")); err != nil {
		t.Errorf("two statements of one account: %v", err)
	}
	block := func(acct, ccy string) string {
		return "<STMTRS><CURDEF>" + ccy + "<BANKACCTFROM><ACCTID>" + acct + "</BANKACCTFROM><STMTTRN><DTPOSTED>20260601<TRNAMT>1.00<FITID>" + acct + ccy + "</STMTTRN></STMTRS>"
	}
	ofx := func(s ...string) error {
		_, err := ParseOFX(strings.NewReader("<OFX>"+strings.Join(s, "")+"</OFX>"), "USD")
		return err
	}
	for name, err := range map[string]error{
		"two accounts":    ofx(block("111111", "USD"), block("222222", "USD")),
		"two currencies":  ofx(block("111111", "USD"), block("111111", "EUR")),
		"long id":         ofx(block(long, "USD")),
		"other currency":  ofx(strings.Replace(block("111111", "EUR"), "<ACCTID>111111", "", 1)),
		"two ids a block": ofx(strings.Replace(block("111111", "USD"), "</BANKACCTFROM>", "<ACCTID>222222</BANKACCTFROM>", 1)),
	} {
		if err == nil {
			t.Errorf("ofx %s: accepted", name)
		}
	}
}

// QA 5: a bank-matched payment on an invoice that also had a credit note made every later purge fail on the
// foreign key.
func TestQAPurgeWithCreditNoteAndMatchedPayment(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	acc := e.token("dana", acct, nil, nil, false)
	party := e.party(maker, "Purge Co", "customer")
	inv := e.approveInvoice(maker, boss, party, obj{"issue_date": "2026-06-01"}, line("work", 1000, 15000), line("extra", 1000, 500))
	var cn Invoice
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/credit-notes", obj{"reason": "returned", "lines": []obj{line("extra", 1000, 500)}}, &cn)
	e.must(200, boss, "POST", "/v1/invoices/"+id(cn.ID)+"/approve", nil, nil)
	b := e.bankAccount(admin)
	// Booked late, so the bank row itself survives the purge and must be unlinked.
	e.must(201, acc, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+csvQuery, "date,amount,reference,name,id\n2033-06-10,150.00,r,n,A1\n", nil)
	var txns struct{ Items []BankTxn }
	e.must(200, acc, "GET", "/v1/bank/transactions?account_id="+id(b.ID), nil, &txns)
	e.must(200, acc, "POST", "/v1/bank/transactions/"+id(txns.Items[0].ID)+"/confirm", obj{"invoice_id": inv.ID}, nil)
	e.advance(8 * 365 * 24 * time.Hour)
	for i := 0; i < 2; i++ {
		if _, err := e.app.Purge(context.Background()); err != nil {
			t.Fatalf("purge %d: %v", i+1, err)
		}
	}
	var invoices, linked int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM invoices`).Scan(&invoices)
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM bank_transactions WHERE matched_payment_id IS NOT NULL`).Scan(&linked)
	if invoices != 0 || linked != 0 {
		t.Errorf("%d invoices left, %d bank rows still linked", invoices, linked)
	}
}

// QA 6: turning the ledger on over existing documents posts what they still owe, and every later hook relieves
// it: receivable and claim payable end at zero, never negative.
func TestQACutOverCoversEveryHook(t *testing.T) {
	e := newEnv(t)
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	emp := e.token("person_9", rw, nil, nil, true)
	party := e.party(maker, "Old Co", "customer")
	paid := e.approveInvoice(maker, boss, party, obj{"issue_date": "2026-06-01"}, line("a", 1000, 10000), line("b", 1000, 1000))
	voided := e.approveInvoice(maker, boss, party, obj{"issue_date": "2026-06-01"}, line("c", 1000, 3000))
	e.must(201, maker, "POST", "/v1/invoices/"+id(paid.ID)+"/payments", obj{"amount_minor": 2000}, nil)
	var ex Expense
	e.must(201, emp, "POST", "/v1/expenses", obj{"description": "Train", "amount_minor": 4500, "currency": "USD", "category": "travel", "incurred_on": "2026-06-02"}, &ex)
	e.must(200, emp, "POST", "/v1/expenses/"+id(ex.ID)+"/submit", nil, nil)
	e.must(200, boss, "POST", "/v1/expenses/"+id(ex.ID)+"/approve", nil, nil)

	ledgerOn(t, e)
	bal := func(code string) int64 {
		var n int64
		_ = e.app.store.db.QueryRow(`SELECT COALESCE(SUM(l.debit_minor - l.credit_minor), 0) FROM journal_lines l JOIN ledger_accounts a ON a.id = l.account_id JOIN journal_entries j ON j.id = l.entry_id WHERE a.code = ? AND j.status = 'posted'`, code).Scan(&n)
		return n
	}
	if bal("1100") != 9000+3000 || bal("2200") != -4500 || bal("3000") != -(12000-4500) {
		t.Fatalf("opening: ar %d claims %d equity %d", bal("1100"), bal("2200"), bal("3000"))
	}
	// A credit note, a back-dated payment, a void and a reimbursement of old documents.
	var cn Invoice
	e.must(201, maker, "POST", "/v1/invoices/"+id(paid.ID)+"/credit-notes", obj{"reason": "r", "lines": []obj{line("b", 1000, 1000)}}, &cn)
	e.must(200, boss, "POST", "/v1/invoices/"+id(cn.ID)+"/approve", nil, nil)
	e.must(201, maker, "POST", "/v1/invoices/"+id(paid.ID)+"/payments", obj{"amount_minor": 8000, "received_on": "2026-06-05"}, nil)
	e.must(200, boss, "POST", "/v1/invoices/"+id(voided.ID)+"/void", obj{"reason": "mistake"}, nil)
	e.must(200, maker, "POST", "/v1/expenses/"+id(ex.ID)+"/reimburse", obj{"reference": "TX"}, nil)
	if bal("1100") != 0 || bal("2200") != 0 {
		t.Errorf("after settling: receivable %d claims %d, want 0 and 0", bal("1100"), bal("2200"))
	}
	var early int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE source = 'payment' AND entry_date < '2026-11-25'`).Scan(&early)
	if early != 0 {
		t.Error("a follow-on posting of an old document is dated before the cut-over")
	}
	// The cut-over happens once.
	e.must(200, adminTok, "PUT", "/v1/ledger/mappings", obj{"mappings": map[string]string{"cash": "1000"}}, nil)
	var n int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE source = 'opening'`).Scan(&n)
	if n != 1 {
		t.Errorf("%d opening entries", n)
	}
}

// QA 7: only manual and payroll journals are reversed by hand, and never by their poster.
func TestQAOnlyManualJournalsAreReversedByHand(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	alex, bea := e.token("alex", ledApp, nil, nil, false), e.token("bea", ledApp, nil, nil, false)
	drafter := e.token("dana", acct, nil, nil, false)
	party := e.party(maker, "Rev Co", "customer")
	inv := e.approveInvoice(maker, boss, party, nil, line("x", 1000, 5000))
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 100}, nil)
	var js struct{ Items []JournalEntry }
	e.must(200, alex, "GET", "/v1/ledger/journals", nil, &js)
	if len(js.Items) != 2 {
		t.Fatalf("%d entries", len(js.Items))
	}
	for _, j := range js.Items {
		if code := e.do(alex, "POST", "/v1/ledger/journals/"+id(j.ID)+"/reverse", obj{"date": "2026-12-01", "reason": "no"}, nil); code != 409 {
			t.Errorf("reversing a %s entry = %d, want 409", j.Source, code)
		}
	}
	var j JournalEntry
	e.must(201, drafter, "POST", "/v1/ledger/journals", manual("2026-05-01", "USD", "5000", "1000", 100), &j)
	e.must(200, alex, "POST", "/v1/ledger/journals/"+id(j.ID)+"/post", nil, nil)
	if code := e.do(alex, "POST", "/v1/ledger/journals/"+id(j.ID)+"/reverse", obj{"date": "2026-05-02", "reason": "r"}, nil); code != 403 {
		t.Errorf("the poster reversing = %d, want 403", code)
	}
	e.must(201, bea, "POST", "/v1/ledger/journals/"+id(j.ID)+"/reverse", obj{"date": "2026-05-02", "reason": "r"}, nil)
}

// QA 8: a deleted payroll draft can be imported again; journals that still exist are not duplicated.
func TestQADeletedPayrollDraftCanBeReimported(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	drafter := e.token("dana", acct, nil, nil, false)
	file := "date,account_code,debit,credit,currency,memo\n2026-05-31,5000,10.00,,USD,a\n2026-05-31,1000,,10.00,USD,b\n2026-06-30,5000,12.00,,USD,a\n2026-06-30,1000,,12.00,USD,b\n"
	var first, again PayrollImportResult
	e.must(201, drafter, "POST", "/v1/ledger/payroll-import?adapter=csv", file, &first)
	e.must(204, drafter, "DELETE", "/v1/ledger/journals/"+id(first.DraftIDs[0]), nil, nil)
	e.must(201, drafter, "POST", "/v1/ledger/payroll-import?adapter=csv", file, &again)
	if len(again.DraftIDs) != 2 || again.DraftIDs[0] == first.DraftIDs[0] || again.DraftIDs[1] != first.DraftIDs[1] || again.Already {
		t.Fatalf("first %v, again %+v", first.DraftIDs, again)
	}
	var third PayrollImportResult
	if code := e.do(drafter, "POST", "/v1/ledger/payroll-import?adapter=csv", file, &third); code != 200 || !third.Already {
		t.Errorf("third = %d %+v", code, third)
	}
	var n int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE source = 'payroll'`).Scan(&n)
	if n != 2 {
		t.Errorf("%d payroll entries", n)
	}
}

// QA 9: equal rows across files are listed, can be forced when they have no bank id, and an id reused for other
// details is called out; counterparty and description take part in the same-id check.
func TestQASkippedRowsAreListedAndForceable(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	path := "/v1/bank/accounts/" + id(b.ID) + "/statements?format=csv&delimiter=comma&date_col=1&amount_col=2&reference_col=3"
	e.must(201, admin, "POST", path, "d,a,r\n2026-06-10,10.00,coffee\n", nil)
	second := "d,a,r\n2026-06-10,10.00,coffee\n2026-06-11,1.00,x\n"
	var res StatementResult
	e.must(201, admin, "POST", path, second, &res)
	if res.NewCount != 1 || len(res.Skipped) != 1 || res.Skipped[0].Row != 1 || res.Skipped[0].Reason != "already_imported" || !res.Skipped[0].Forceable {
		t.Fatalf("%+v", res)
	}
	var forced StatementResult
	e.must(201, admin, "POST", path+"&force_duplicates=true", "d,a,r\n2026-06-10,10.00,coffee\n2026-06-12,2.00,y\n", &forced)
	if forced.NewCount != 2 || forced.DuplicateCount != 0 {
		t.Errorf("forced %+v", forced)
	}
	withID := "/v1/bank/accounts/" + id(b.ID) + "/statements" + csvQuery
	e.must(201, admin, "POST", withID, "d,a,r,n,i\n2026-06-20,5.00,r,Alice,Z1\n", nil)
	var reused StatementResult
	e.must(201, admin, "POST", withID+"&force_duplicates=true", "d,a,r,n,i\n2026-06-20,5.00,r,Mallory,Z1\n", &reused)
	if reused.NewCount != 0 || len(reused.Skipped) != 1 || reused.Skipped[0].Reason != "id_reused" || reused.Skipped[0].Forceable {
		t.Errorf("reused id %+v", reused)
	}
	if code := e.do(admin, "POST", withID, "d,a,r,n,i\n2026-06-21,5.00,r,Alice,Z2\n2026-06-21,5.00,r,Bob,Z2\n", nil); code != 422 {
		t.Errorf("one id, two counterparties in a file = %d", code)
	}
}

// QA 10: remittance lines are capped while they accumulate.
func TestQACamtManyRemittanceLinesAreLinearAndCapped(t *testing.T) {
	doc := "<Document><Stmt><Ntry><Amt Ccy=\"USD\">1.00</Amt><CdtDtInd>CRDT</CdtDtInd><BookgDt><Dt>2026-06-01</Dt></BookgDt><NtryDtls><TxDtls><RmtInf>" +
		strings.Repeat("<Ustrd>\u00e9\u00e9\u00e9\u00e9\u00e9\u00e9</Ustrd>", 60000) + "</RmtInf></TxDtls></NtryDtls></Ntry></Stmt></Document>"
	start := time.Now()
	ps, err := ParseCAMT053(strings.NewReader(doc), "USD")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(ps.Txns[0].Reference); n > maxReferenceBytes || !utf8.ValidString(ps.Txns[0].Reference) {
		t.Errorf("reference %d bytes", n)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
}

// QA 11: a batch is bounded by lines, and successive batches cover every entry once.
func TestQAExportBatchIsBoundedByLines(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	s := e.app.store
	var a, b int64
	_ = s.db.QueryRow(`SELECT id FROM ledger_accounts WHERE code = '1000'`).Scan(&a)
	_ = s.db.QueryRow(`SELECT id FROM ledger_accounts WHERE code = '4000'`).Scan(&b)
	var lines []lineSpec
	for i := 0; i < maxJournalLines/2; i++ {
		lines = append(lines, lineSpec{AccountID: a, Debit: 1}, lineSpec{AccountID: b, Credit: 1})
	}
	for i := 0; i < 40; i++ {
		if err := s.inTx(context.Background(), func(tx *sql.Tx) error {
			_, err := s.postEntryTx(context.Background(), tx, postSpec{Source: "manual", Date: "2026-05-01", Currency: "USD", By: "p", Lines: lines})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := &Principal{Subject: "dana", Permissions: []string{PermAdmin}}
	var got []int
	for {
		batch, _, err := s.ExportJournal(context.Background(), p, "csv", "")
		if err != nil {
			t.Fatal(err)
		}
		if batch.EntryCount == 0 {
			break
		}
		got = append(got, batch.EntryCount)
	}
	if len(got) != 2 || got[0] != exportMaxLines/maxJournalLines || got[0]+got[1] != 40 {
		t.Errorf("batches %v", got)
	}
}

// QA 12: digit groups must have three digits.
func TestQAThousandsGroupsAreChecked(t *testing.T) {
	for _, bad := range []string{"12,50", "1,23.45", "1,2345", ",123", "1,,000", "1234,567", "1,000,", "1,00,000.00"} {
		if v, err := parseDecimalMinor(bad, 2, '.', ','); err == nil {
			t.Errorf("%q = %d, want an error", bad, v)
		}
	}
	for in, want := range map[string]int64{"1,000": 100000, "-12,345,678.90": -1234567890, "999": 99900, "1000.5": 100050} {
		if v, err := parseDecimalMinor(in, 2, '.', ','); err != nil || v != want {
			t.Errorf("%q = %d %v, want %d", in, v, err, want)
		}
	}
}

// QA 13: a month that gained rows after its sign-off can be signed off again.
func TestQAMonthCanBeSignedOffAgain(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	approver := e.token("alex", ledApp, nil, nil, false)
	b := e.bankAccount(admin)
	base := "/v1/bank/accounts/" + id(b.ID)
	settle := func(rows string) {
		e.must(201, admin, "POST", base+"/statements"+csvQuery, "d,a,r,n,i\n"+rows, nil)
		var all struct{ Items []BankTxn }
		e.must(200, admin, "GET", "/v1/bank/transactions?status=unmatched", nil, &all)
		for _, x := range all.Items {
			e.must(200, admin, "POST", "/v1/bank/transactions/"+id(x.ID)+"/ignore", obj{"reason": "r"}, nil)
		}
	}
	status := func() string {
		var rec struct{ Items []ReconPeriod }
		e.must(200, admin, "GET", base+"/reconciliation", nil, &rec)
		return rec.Items[0].Status
	}
	settle("2026-06-01,1.00,r,n,A\n")
	e.must(201, approver, "POST", base+"/reconciliation", obj{"period": "2026-06"}, nil)
	settle("2026-06-02,2.00,r,n,B\n")
	if s := status(); s != "ready" {
		t.Errorf("after new rows: %s, want ready", s)
	}
	e.must(201, approver, "POST", base+"/reconciliation", obj{"period": "2026-06"}, nil)
	if s := status(); s != "reconciled" {
		t.Errorf("after signing again: %s", s)
	}
	e.must(409, approver, "POST", base+"/reconciliation", obj{"period": "2026-06"}, nil)
}
