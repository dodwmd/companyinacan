package finance

// Regression tests for the E15c QA findings (PR #46), one or more per finding.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
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
	doc := "<Document><BkToCstmrStmt><Stmt><Ntry><Amt Ccy=\"USD\">1.00</Amt><CdtDtInd>CRDT</CdtDtInd><BookgDt><Dt>2026-06-01</Dt></BookgDt><NtryDtls><TxDtls><RmtInf>" +
		strings.Repeat("<Ustrd>\u00e9\u00e9\u00e9\u00e9\u00e9\u00e9</Ustrd>", 60000) + "</RmtInf></TxDtls></NtryDtls></Ntry></Stmt></BkToCstmrStmt></Document>"
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

// QA 11 and QA2 3: a batch is bounded by the bytes of its file, even with the longest multibyte fields, and
// successive batches cover every entry once.
func TestQAExportBatchIsBoundedByBytes(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	s := e.app.store
	var a, b int64
	_ = s.db.QueryRow(`SELECT id FROM ledger_accounts WHERE code = '1000'`).Scan(&a)
	_ = s.db.QueryRow(`SELECT id FROM ledger_accounts WHERE code = '4000'`).Scan(&b)
	wide := func(n int) string { return strings.Repeat("\U0001F9FE\u5e33", n/2) }
	if _, err := s.db.Exec(`UPDATE ledger_accounts SET name = ?`, wide(maxName)); err != nil {
		t.Fatal(err)
	}
	var lines []lineSpec
	for i := 0; i < maxJournalLines/2; i++ {
		lines = append(lines, lineSpec{AccountID: a, Debit: 1, Memo: wide(200)}, lineSpec{AccountID: b, Credit: 1, Memo: wide(200)})
	}
	const entries = 7
	for i := 0; i < entries; i++ {
		if err := s.inTx(context.Background(), func(tx *sql.Tx) error {
			_, err := s.postEntryTx(context.Background(), tx, postSpec{Source: "manual", Date: "2026-05-01", Currency: "USD", By: "p", Memo: wide(500), Lines: lines})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := &Principal{Subject: "dana", Permissions: []string{PermAdmin}}
	total, batches := 0, 0
	for {
		batch, _, err := s.ExportJournal(context.Background(), p, "csv", "")
		if err != nil {
			t.Fatal(err)
		}
		if batch.EntryCount == 0 {
			break
		}
		_, content, err := s.ExportContent(context.Background(), p, batch.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(content) > exportMaxBytes || batch.FirstNumber != int64(total+1) {
			t.Fatalf("batch of %d bytes starting at %d after %d entries", len(content), batch.FirstNumber, total)
		}
		if rows, err := csv.NewReader(bytes.NewReader(content)).ReadAll(); err != nil || len(rows) != 1+batch.EntryCount*maxJournalLines {
			t.Fatalf("batch is not whole entries: %d rows, %v", len(rows), err)
		}
		total, batches = total+batch.EntryCount, batches+1
	}
	if total != entries || batches < 3 {
		t.Errorf("%d entries in %d batches", total, batches)
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

// QA 14 and 15: a payment already recorded is linked instead of recorded again, the reserved external id cannot
// be planted, and no bank text reaches the payment or the idempotency cache.
func TestQAConfirmLinksInsteadOfDoubleRecording(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	acc := e.token("dana", acct, nil, nil, false)
	party := e.party(maker, "Link Co", "customer")
	inv := e.approveInvoice(maker, boss, party, obj{"issue_date": "2026-06-01"}, line("work", 1000, 10000))
	if code := e.do(maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 100, "external_id": "bank:1:1"}, nil); code != 422 {
		t.Errorf("planting the reserved external id = %d", code)
	}
	var manualPay Payment
	e.must(201, maker, "POST", "/v1/invoices/"+id(inv.ID)+"/payments", obj{"amount_minor": 4000}, &manualPay)
	b := e.bankAccount(admin)
	e.must(201, acc, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+csvQuery, "d,a,r,n,i\n2026-06-10,40.00,secret words,Jane Private,A\n2026-06-11,40.00,more,Jane Private,B\n", nil)
	var txns struct{ Items []BankTxn }
	e.must(200, acc, "GET", "/v1/bank/transactions", nil, &txns)
	confirm := func(i int, body obj) int {
		return e.do(acc, "POST", "/v1/bank/transactions/"+id(txns.Items[i].ID)+"/confirm", body, nil, "Idempotency-Key", "qa14-key-"+toJSON(body)+id(int64(i)))
	}
	if code := confirm(0, obj{"invoice_id": inv.ID}); code != 409 {
		t.Fatalf("a confirm over an unlinked payment = %d, want 409", code)
	}
	if code := confirm(0, obj{"invoice_id": inv.ID, "payment_id": manualPay.ID}); code != 200 {
		t.Fatalf("link = %d", code)
	}
	if code := confirm(1, obj{"invoice_id": inv.ID, "payment_id": manualPay.ID}); code != 422 {
		t.Errorf("linking one payment twice = %d", code)
	}
	var count int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM payments`).Scan(&count)
	if count != 1 {
		t.Errorf("%d payments after linking, want 1", count)
	}
	if code := confirm(1, obj{"invoice_id": inv.ID, "record_new": true}); code != 200 {
		t.Fatalf("record a new payment = %d", code)
	}
	var dump string
	_ = e.app.store.db.QueryRow(`SELECT COALESCE((SELECT group_concat(reference) FROM payments), '') || COALESCE((SELECT group_concat(response) FROM idempotency WHERE response LIKE '%Jane%' OR response LIKE '%secret%'), '')`).Scan(&dump)
	if strings.Contains(dump, "Jane") || strings.Contains(dump, "secret") {
		t.Errorf("bank text was copied: %s", dump)
	}
}

// QA 15: the erasure report names the retained bank data.
func TestQAErasureReportListsRetainedBankData(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	acc := e.token("dana", acct, nil, nil, false)
	party := e.party(maker, "Forget Me", "customer")
	inv := e.approveInvoice(maker, boss, party, obj{"issue_date": "2026-06-01"}, line("work", 1000, 100))
	b := e.bankAccount(admin)
	e.must(201, acc, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+csvQuery, "d,a,r,n,i\n2026-06-10,1.00,r,Forget Me,A\n", nil)
	var txns struct{ Items []BankTxn }
	e.must(200, acc, "GET", "/v1/bank/transactions", nil, &txns)
	e.must(200, acc, "POST", "/v1/bank/transactions/"+id(txns.Items[0].ID)+"/confirm", obj{"invoice_id": inv.ID}, nil)
	var rep ErasureReport
	e.must(200, adminTok, "POST", "/v1/parties/"+id(party)+"/erase", obj{}, &rep)
	for _, r := range rep.Retained {
		if r.Class == "finance-bank" && r.Count == 1 && r.Until != "" {
			return
		}
	}
	t.Errorf("retained %+v", rep.Retained)
}

// QA 16: empty unclosed leaves do not reach the depth cap, a CURRENCY aggregate is honoured, and CURDEF is checked.
func TestQAOFXEmptyLeavesAndCurrencyAggregate(t *testing.T) {
	trn := func(i, extra string) string {
		return "<STMTTRN><DTPOSTED>20260601<CHECKNUM><REFNUM><MEMO><TRNAMT>1.00<FITID>" + i + extra + "</STMTTRN>"
	}
	var many strings.Builder
	for i := 0; i < 50; i++ {
		many.WriteString(trn(id(int64(i)), "<NAME><PAYEEID>"))
	}
	ps, err := ParseOFX(strings.NewReader("<OFX><STMTRS><CURDEF>USD<BANKTRANLIST>"+many.String()+"</BANKTRANLIST></STMTRS></OFX>"), "USD")
	if err != nil || len(ps.Txns) != 50 || ps.Txns[49].AmountMinor != 100 {
		t.Fatalf("empty leaves: %v", err)
	}
	wrap := func(body, def string) error {
		_, err := ParseOFX(strings.NewReader("<OFX><STMTRS><CURDEF>"+def+"<BANKTRANLIST>"+body+"</BANKTRANLIST></STMTRS></OFX>"), "USD")
		return err
	}
	if wrap(trn("1", "<CURRENCY><CURRATE>1.1<CURSYM>EUR</CURRENCY>"), "USD") == nil {
		t.Error("a EUR amount (CURRENCY aggregate) was accepted into a USD account")
	}
	if err := wrap(trn("1", "<ORIGCURRENCY><CURRATE>1.1<CURSYM>EUR</ORIGCURRENCY>"), "USD"); err != nil {
		t.Errorf("an already converted amount: %v", err)
	}
	if wrap(trn("1", ""), "EUR") == nil {
		t.Error("a EUR statement (CURDEF) was accepted")
	}
}

// QA 17: a provider payment that would post into a closed period is recorded as unmatched, not retried forever.
func TestQAWebhookIntoAClosedPeriodIsRecordedUnmatched(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	party := e.party(maker, "Hook Co", "customer")
	inv := e.approveInvoice(maker, boss, party, obj{"issue_date": "2026-06-01"}, line("x", 1000, 1000))
	e.must(200, adminTok, "POST", "/v1/ledger/periods/close", obj{"closed_through": "2026-12-31"}, nil)
	body := `{"id":"evt-closed","type":"payment.succeeded","invoice_id":` + id(inv.ID) + `,"amount_minor":100,"currency":"USD"}`
	var r struct{ Status string }
	code := e.do("", "POST", "/v1/webhooks/payments", body, &r, "X-Fake-Signature", SignFake([]byte(provSec), e.now, []byte(body)))
	if code != 200 || r.Status != "unmatched" {
		t.Fatalf("webhook = %d %q", code, r.Status)
	}
	var n, pays int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'payment.unmatched' AND target = 'evt-closed'`).Scan(&n)
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM payments`).Scan(&pays)
	if n != 1 || pays != 0 {
		t.Errorf("unmatched records %d, payments %d", n, pays)
	}
}

// QA 18: a draft's line cannot be moved into a posted entry.
func TestQALineCannotMoveIntoAPostedEntry(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	drafter, approver := e.token("dana", acct, nil, nil, false), e.token("alex", ledApp, nil, nil, false)
	var posted, draft JournalEntry
	e.must(201, drafter, "POST", "/v1/ledger/journals", manual("2026-05-01", "USD", "5000", "1000", 100), &posted)
	e.must(200, approver, "POST", "/v1/ledger/journals/"+id(posted.ID)+"/post", nil, nil)
	e.must(201, drafter, "POST", "/v1/ledger/journals", manual("2026-05-02", "USD", "5000", "1000", 7), &draft)
	if _, err := e.app.store.db.Exec(`UPDATE journal_lines SET entry_id = ?, position = position + 10 WHERE entry_id = ?`, posted.ID, draft.ID); err == nil {
		t.Error("lines were moved into a posted entry")
	}
}

func (e *env) importCSV(tok string, bank int64, force bool, rows string) StatementResult {
	e.t.Helper()
	path := "/v1/bank/accounts/" + id(bank) + "/statements?format=csv&delimiter=comma&date_col=1&amount_col=2&reference_col=3"
	if force {
		path += "&force_duplicates=true"
	}
	var res StatementResult
	if code := e.do(tok, "POST", path, "d,a,r\n"+rows, &res); code != 201 && code != 200 {
		e.t.Fatalf("import = %d", code)
	}
	return res
}

func (e *env) bankRows() int {
	var n int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM bank_transactions`).Scan(&n)
	return n
}

// QA round 2, 1: force works on the very file whose rows were skipped, and adds them once.
func TestQA2ForceOnAnAlreadyImportedFile(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	e.importCSV(admin, b.ID, false, "2026-06-10,10.00,coffee\n")
	second := "2026-06-10,10.00,coffee\n2026-06-11,1.00,x\n"
	if r := e.importCSV(admin, b.ID, false, second); r.NewCount != 1 || len(r.Skipped) != 1 {
		t.Fatalf("%+v", r)
	}
	r := e.importCSV(admin, b.ID, true, second)
	if !r.AlreadyImported || r.NewCount != 1 || r.DuplicateCount != 1 || r.TransactionCnt != 2 || e.bankRows() != 3 {
		t.Fatalf("force on the imported file: %+v, %d rows", r, e.bankRows())
	}
	if r := e.importCSV(admin, b.ID, true, second); r.NewCount != 0 || e.bankRows() != 3 {
		t.Errorf("forced twice: %+v, %d rows", r, e.bankRows())
	}
	if r := e.importCSV(admin, b.ID, true, "2026-06-10,10.00,coffee\n"); r.NewCount != 0 || e.bankRows() != 3 {
		t.Errorf("forcing a file that owns its rows: %+v, %d rows", r, e.bankRows())
	}
}

// QA round 2, 2: a forced row is recognised by the next cumulative statement.
func TestQA2ForcedRowIsNotImportedAgainByACumulativeFile(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	e.importCSV(admin, b.ID, false, "2026-06-10,10.00,coffee\n")
	e.importCSV(admin, b.ID, true, "2026-06-10,10.00,coffee\n2026-06-12,2.00,y\n") // a second, real coffee
	before := e.bankRows()
	r := e.importCSV(admin, b.ID, false, "2026-06-10,10.00,coffee\n2026-06-10,10.00,coffee\n2026-06-12,2.00,y\n2026-06-13,3.00,z\n")
	var coffee int
	_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM bank_transactions WHERE reference = 'coffee'`).Scan(&coffee)
	if coffee != 2 || r.NewCount != 1 || e.bankRows() != before+1 {
		t.Errorf("%d coffee rows for 2 real, %+v", coffee, r)
	}
}

// QA round 2, 4: a long reason in a multibyte script is cut between characters.
func TestQA2ReversalMemoIsCutByCharacters(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	drafter, alex, bea := e.token("dana", acct, nil, nil, false), e.token("alex", ledApp, nil, nil, false), e.token("bea", ledApp, nil, nil, false)
	var j, rev JournalEntry
	e.must(201, drafter, "POST", "/v1/ledger/journals", manual("2026-05-01", "USD", "5000", "1000", 100), &j)
	e.must(200, alex, "POST", "/v1/ledger/journals/"+id(j.ID)+"/post", nil, nil)
	e.must(201, bea, "POST", "/v1/ledger/journals/"+id(j.ID)+"/reverse", obj{"date": "2026-05-02", "reason": strings.Repeat("\u5e33", 300)}, &rev)
	if !utf8.ValidString(rev.Memo) || strings.ContainsRune(rev.Memo, utf8.RuneError) || !strings.HasSuffix(rev.Memo, "\u5e33") {
		t.Errorf("memo %q", rev.Memo)
	}
	if got := truncateRunes(strings.Repeat("\u5e33", 600), 500); utf8.RuneCountInString(got) != 500 || !utf8.ValidString(got) {
		t.Errorf("%d characters", utf8.RuneCountInString(got))
	}
}

// QA round 2, 5: other camt message types, and entries outside a statement, are refused.
func TestQA2OnlyCamt053StatementsAreRead(t *testing.T) {
	entry := `<Ntry><Amt Ccy="USD">1.00</Amt><CdtDtInd>CRDT</CdtDtInd><BookgDt><Dt>2026-06-01</Dt></BookgDt></Ntry>`
	for name, doc := range map[string]string{
		"camt.052":           `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.052.001.02"><BkToCstmrAcctRpt><Rpt><Acct><Id><IBAN>GB29NWBK60161331926819</IBAN></Id></Acct>` + entry + `</Rpt></BkToCstmrAcctRpt></Document>`,
		"052 no namespace":   `<Document><BkToCstmrAcctRpt><Rpt>` + entry + `</Rpt></BkToCstmrAcctRpt></Document>`,
		"052 namespace only": `<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.052.001.02"><BkToCstmrStmt><Stmt>` + entry + `</Stmt></BkToCstmrStmt></Document>`,
		"camt.054":           `<Document><BkToCstmrDbtCdtNtfctn><Ntfctn>` + entry + `</Ntfctn></BkToCstmrDbtCdtNtfctn></Document>`,
		"entry outside Stmt": `<Document><BkToCstmrStmt>` + entry + `</BkToCstmrStmt></Document>`,
		"entry in a report":  `<Document><BkToCstmrStmt><Rpt>` + entry + `</Rpt></BkToCstmrStmt></Document>`,
		"another root":       `<Stmt>` + entry + `</Stmt>`,
	} {
		if _, err := ParseCAMT053(strings.NewReader(doc), "USD"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseCAMT053(strings.NewReader(`<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.08"><BkToCstmrStmt><Stmt>`+entry+`</Stmt></BkToCstmrStmt></Document>`), "USD"); err != nil {
		t.Errorf("camt.053: %v", err)
	}
}

// QA round 2, 6: one AcctSvcrRef for a payment and its fee, in one file or two.
func TestQA2ReusedServicerReference(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	entry := func(amt, ind, info string) string {
		return `<Ntry><Amt Ccy="USD">` + amt + `</Amt><CdtDtInd>` + ind + `</CdtDtInd><BookgDt><Dt>2026-06-01</Dt></BookgDt><AcctSvcrRef>OP1</AcctSvcrRef><AddtlNtryInf>` + info + `</AddtlNtryInf></Ntry>`
	}
	send := func(entries ...string) StatementResult {
		var r StatementResult
		code := e.do(admin, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements?format=camt053", "<Document><BkToCstmrStmt><Stmt>"+strings.Join(entries, "")+"</Stmt></BkToCstmrStmt></Document>", &r)
		if code != 201 && code != 200 {
			t.Fatalf("import = %d", code)
		}
		return r
	}
	if r := send(entry("100.00", "CRDT", "payment")); r.NewCount != 1 {
		t.Fatalf("payment %+v", r)
	}
	if r := send(entry("1.50", "DBIT", "fee")); r.NewCount != 1 {
		t.Fatalf("the fee in another file %+v", r)
	}
	if r := send(entry("100.00", "CRDT", "payment, reworded"), entry("1.50", "DBIT", "fee")); r.NewCount != 0 || len(r.Skipped) != 2 {
		t.Errorf("cumulative %+v", r)
	}
	if r := send(entry("200.00", "CRDT", "p2"), entry("2.00", "DBIT", "f2"), entry("2.00", "DBIT", "f2 again")); r.NewCount != 3 {
		t.Errorf("one file, shared reference %+v", r)
	}
}

// QA round 2, 6: a statement with no transactions is kept until its own age passes retention.
func TestQA2RecentEmptyStatementSurvivesThePurge(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	e.importCSV(admin, b.ID, false, "2026-06-10,10.00,coffee\n")
	e.importCSV(admin, b.ID, false, "2026-06-10,10.00,coffee\n\n") // another file, nothing new
	statements := func() int {
		var n int
		_ = e.app.store.db.QueryRow(`SELECT COUNT(*) FROM bank_statements`).Scan(&n)
		return n
	}
	purge := func() {
		if _, err := e.app.Purge(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	purge()
	if n := statements(); n != 2 {
		t.Fatalf("%d statements after a purge the same day, want 2", n)
	}
	e.advance(8 * 365 * 24 * time.Hour)
	purge()
	if n := statements(); n != 1 { // the one whose unmatched row is kept
		t.Errorf("%d statements after the retention period, want 1", n)
	}
}

// QA round 3, 1: forcing reads the statement's rows once. With tens of thousands of rows on the account and a
// full-size file, a per-row scan of the account takes minutes on the only writer connection.
func TestQA3ForceIsNotQuadratic(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	e.importCSV(admin, b.ID, false, "2026-06-01,1.00,seed\n")
	if _, err := e.app.store.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 30000)
		INSERT INTO bank_transactions (bank_account_id, statement_id, txn_key, booked_on, amount_minor, currency, reference, created_at)
		SELECT ?, 1, 'seed' || i, '2026-05-01', i, 'USD', 'old ' || i, 1 FROM n`, b.ID); err != nil {
		t.Fatal(err)
	}
	var rows strings.Builder
	for i := 0; i < maxStatementRows; i++ {
		rows.WriteString("2026-06-02,2.00,row " + id(int64(i)) + "\n")
	}
	begin := time.Now()
	e.importCSV(admin, b.ID, false, rows.String())
	plain := time.Since(begin) // the yardstick: the same file imported without force, on this machine
	// The same rows in another file are all duplicates; forcing them takes every one.
	again := rows.String() + "\n" // other bytes, the same rows
	start := time.Now()
	r := e.importCSV(admin, b.ID, true, again)
	r2 := e.importCSV(admin, b.ID, true, again)
	if d := time.Since(start); d > 20*plain+5*time.Second {
		t.Errorf("two forced imports took %s, a plain import of the file %s", d, plain)
	}
	if r.NewCount != maxStatementRows || r2.NewCount != 0 {
		t.Errorf("forced %d then %d", r.NewCount, r2.NewCount)
	}
	// The row-visit bound: the one query over the statement uses its index and never scans the table.
	plan, err := e.app.store.db.Query(`EXPLAIN QUERY PLAN `+ownedGroupsQuery, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plan.Close() }()
	var all []string
	for plan.Next() {
		var a, b, c int
		var detail string
		if err := plan.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		all = append(all, detail)
	}
	got := strings.Join(all, "; ")
	if !strings.Contains(got, "bank_transactions_statement") || strings.Contains(got, "SCAN") {
		t.Errorf("query plan: %s", got)
	}
}

// QA round 3, 3: a second genuine entry with the same reference, date and amount can be forced in, once, and a
// cumulative statement then recognises both.
func TestQA3SecondEqualCamtEntryIsForceable(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	entry := func(info string) string {
		return `<Ntry><Amt Ccy="USD">2.00</Amt><CdtDtInd>DBIT</CdtDtInd><BookgDt><Dt>2026-06-01</Dt></BookgDt><AcctSvcrRef>OP9</AcctSvcrRef><AddtlNtryInf>` + info + `</AddtlNtryInf></Ntry>`
	}
	send := func(q string, entries ...string) StatementResult {
		var r StatementResult
		if code := e.do(admin, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements?format=camt053"+q, "<Document><BkToCstmrStmt><Stmt>"+strings.Join(entries, "")+"</Stmt></BkToCstmrStmt></Document>", &r); code != 201 && code != 200 {
			t.Fatalf("import = %d", code)
		}
		return r
	}
	send("", entry("first"))
	r := send("", entry("second"))
	if r.NewCount != 0 || len(r.Skipped) != 1 || !r.Skipped[0].Forceable {
		t.Fatalf("%+v", r)
	}
	if r := send("&force_duplicates=true", entry("second")); r.NewCount != 1 || e.bankRows() != 2 {
		t.Fatalf("forced %+v, %d rows", r, e.bankRows())
	}
	if r := send("&force_duplicates=true", entry("second")); r.NewCount != 0 {
		t.Errorf("forced twice %+v", r)
	}
	if r := send("", entry("first"), entry("second")); r.NewCount != 0 || e.bankRows() != 2 {
		t.Errorf("cumulative %+v, %d rows", r, e.bankRows())
	}
}

// QA round 3, 2: forcing a file with another mapping is refused instead of duplicating every row.
func TestQA3ForceWithAnotherMappingIsRefused(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	base := "/v1/bank/accounts/" + id(b.ID) + "/statements?format=csv&delimiter=comma&date_col=1&amount_col=2"
	file := "d,a,r,x\n2026-06-10,1.00,one,uno\n2026-06-11,2.00,two,dos\n"
	e.must(201, admin, "POST", base+"&reference_col=3", file, nil)
	for _, other := range []string{"&reference_col=4", "", "&reference_col=3&description_col=4"} {
		if code := e.do(admin, "POST", base+other+"&force_duplicates=true", file, nil); code != 409 {
			t.Errorf("force with mapping %q = %d, want 409", other, code)
		}
	}
	// Naming a default is the same mapping.
	var r StatementResult
	e.must(200, admin, "POST", base+"&reference_col=3&decimal=dot&header=true&date_layout=2006-01-02&force_duplicates=true", file, &r)
	if r.NewCount != 0 || e.bankRows() != 2 {
		t.Errorf("%+v, %d rows", r, e.bankRows())
	}
}

// QA round 3, 4: with every download slot taken, a request is told to come back instead of outliving the server's
// write timeout.
func TestQA3DownloadWaitIsBounded(t *testing.T) {
	e := newEnv(t)
	if exportDownloadWait >= newServer(nil).WriteTimeout/2 {
		t.Errorf("the wait %s is too close to the write timeout", exportDownloadWait)
	}
	s := e.app.store
	s.downloadWait = 20 * time.Millisecond
	for range exportDownloads {
		s.downloads <- struct{}{}
	}
	code, hdr, body := e.req(adminTok, "GET", "/v1/ledger/exports/1/download", nil)
	if code != 503 || hdr.Get("Retry-After") == "" {
		t.Fatalf("busy = %d %q %s", code, hdr.Get("Retry-After"), body)
	}
	<-s.downloads
	if code, _, _ := e.req(adminTok, "GET", "/v1/ledger/exports/1/download", nil); code != 404 {
		t.Errorf("with a free slot = %d, want 404 for a batch that does not exist", code)
	}
}

// QA round 3, 5: a session that can post journals or change the ledger re-authenticates as often as an approver's.
func TestQA3LedgerPrivilegeUsesTheShortRecheck(t *testing.T) {
	u := &UI{RecheckEvery: uiRecheckDefault}
	for perm, want := range map[string]time.Duration{
		PermLedgerApprove: uiRecheckPrivil, PermLedgerAdmin: uiRecheckPrivil, PermApprove: uiRecheckPrivil, PermAdmin: uiRecheckPrivil,
		PermLedgerRead: uiRecheckDefault, PermRead: uiRecheckDefault,
	} {
		if got := u.recheckFor(&Principal{Permissions: []string{perm}}); got != want {
			t.Errorf("%s: recheck every %s, want %s", perm, got, want)
		}
	}
}
