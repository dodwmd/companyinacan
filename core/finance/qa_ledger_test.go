package finance

// Regression tests for the E15c QA findings (PR #46), one or more per finding.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
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
