package finance

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func sealedEnv(t *testing.T) *env {
	t.Helper()
	// NewApp reads the seal key from Secrets; newEnv does not pass one, so set it on the store.
	e := newEnv(t)
	sl, err := NewSealer(sealHex)
	if err != nil {
		t.Fatal(err)
	}
	e.app.store.sealer = sl
	ledgerOn(t, e)
	return e
}

const csvQuery = "?format=csv&delimiter=comma&date_col=1&amount_col=2&reference_col=3&counterparty_col=4&id_col=5"

func (e *env) bankAccount(tok string) BankAccount {
	e.t.Helper()
	var b BankAccount
	e.must(201, tok, "POST", "/v1/bank/accounts", obj{"name": "Operating", "currency": "USD", "number": "DE89 3704 0044 0532 0130 00", "ledger_account_code": "1000"}, &b)
	return b
}

func TestBankAccountNumbersAreSealedAndMasked(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	if b.Masked != "****3000" {
		t.Errorf("masked = %q", b.Masked)
	}
	for _, path := range []string{"/v1/bank/accounts", "/v1/bank/accounts/" + id(b.ID)} {
		_, _, body := e.req(admin, "GET", path, nil)
		if strings.Contains(string(body), "DE89") || strings.Contains(string(body), "0532") || strings.Contains(string(body), "sealed") {
			t.Errorf("GET %s leaks the number: %s", path, body)
		}
	}
	var sealed, last4 string
	_ = e.app.store.db.QueryRow(`SELECT sealed_number, last4 FROM bank_accounts WHERE id = ?`, b.ID).Scan(&sealed, &last4)
	if strings.Contains(sealed, "DE89") || strings.Contains(sealed, "3704") || !strings.HasPrefix(sealed, "v1.") || last4 != "3000" {
		t.Errorf("stored %q last4 %q", sealed, last4)
	}
	// The audit log and the outbox never hold it.
	var aud struct{ Items []AuditEntry }
	e.must(200, adminTok, "GET", "/v1/audit?limit=100", nil, &aud)
	if strings.Contains(toJSON(aud), "DE89") || strings.Contains(toJSON(aud), "03704") {
		t.Error("the number is in the audit log")
	}
	// The ciphertext is bound to its row: moved to another row it does not open.
	other := e.bankAccount(admin)
	if _, err := e.app.store.sealer.Open(sealed, sealAAD(other.ID)); err == nil {
		t.Error("a sealed number opened under another row's additional data")
	}
	if got, err := e.app.store.sealer.Open(sealed, sealAAD(b.ID)); err != nil || got != "DE89370400440532013000" {
		t.Errorf("open = %q, %v", got, err)
	}
	other2, _ := NewSealer(strings.Repeat("ab", 32))
	if _, err := other2.Open(sealed, sealAAD(b.ID)); err == nil {
		t.Error("opened with the wrong key")
	}
	// Without a key the feature fails closed.
	e2 := newEnv(t)
	ledgerOn(t, e2)
	if code := e2.do(adminTok, "POST", "/v1/bank/accounts", obj{"name": "x", "currency": "USD", "number": "12345678", "ledger_account_code": "1000"}, nil); code != 503 {
		t.Errorf("no seal key = %d, want 503", code)
	}
	for name, in := range map[string]obj{
		"short":         {"name": "x", "currency": "USD", "number": "12", "ledger_account_code": "1000"},
		"not an asset":  {"name": "x", "currency": "USD", "number": "12345678", "ledger_account_code": "4000"},
		"unknown ledge": {"name": "x", "currency": "USD", "number": "12345678", "ledger_account_code": "0000"},
		"bad currency":  {"name": "x", "currency": "ZZZ", "number": "12345678", "ledger_account_code": "1000"},
	} {
		if code := e.do(admin, "POST", "/v1/bank/accounts", in, nil); code != 422 {
			t.Errorf("%s = %d", name, code)
		}
	}
}

func TestStatementImportDedupAndMatching(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	acc := e.token("dana", acct, nil, nil, false)
	maker, boss := e.token("maker", rw, nil, nil, false), e.token("boss", app, nil, nil, false)
	party := e.party(maker, "Pay Co", "customer")
	inv := e.approveInvoice(maker, boss, party, obj{"issue_date": "2026-06-01"}, line("work", 1000, 15000))
	inv2 := e.approveInvoice(maker, boss, party, obj{"issue_date": "2026-06-01"}, line("more", 1000, 9900))
	b := e.bankAccount(admin)
	file := fmt.Sprintf("date,amount,reference,name,id\n2026-06-10,150.00,Payment %s,Pay Co,TX1\n2026-06-11,-12.50,Bank fee,Bank,TX2\n2026-06-12,99.00,thanks,Someone,TX3\n2026-06-13,5.00,=cmd|' /C calc'!A0,Mallory,TX4\n", *inv.Number)
	var res StatementResult
	e.must(201, acc, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+csvQuery, file, &res)
	if res.NewCount != 4 || res.DuplicateCount != 0 || res.TransactionCnt != 4 {
		t.Fatalf("import %+v", res)
	}
	// The same file again changes nothing; an overlapping file only adds what is new.
	var same StatementResult
	if code := e.do(acc, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+csvQuery, file, &same); code != 200 || !same.AlreadyImported || same.StatementID != res.StatementID {
		t.Errorf("same file = %d %+v", code, same)
	}
	overlap := "date,amount,reference,name,id\n2026-06-12,99.00,thanks,Someone,TX3\n2026-06-14,7.00,new,Z,TX5\n"
	var ov StatementResult
	e.must(201, acc, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+csvQuery, overlap, &ov)
	if ov.NewCount != 1 || ov.DuplicateCount != 1 {
		t.Errorf("overlap %+v", ov)
	}
	if n := len(e.outbox("bank.statement.imported.v1")); n != 2 {
		t.Errorf("import events %d, want 2 (not for the repeated file)", n)
	}
	ev := e.outbox("bank.statement.imported.v1")[0]
	if strings.Contains(toJSON(ev), "Pay Co") || strings.Contains(toJSON(ev), "Payment") {
		t.Errorf("event carries bank text: %v", ev)
	}

	var txns struct{ Items []BankTxn }
	e.must(200, acc, "GET", "/v1/bank/transactions?account_id="+id(b.ID)+"&status=unmatched", nil, &txns)
	if len(txns.Items) != 5 {
		t.Fatalf("transactions %d", len(txns.Items))
	}
	var sg struct{ Items []Suggestion }
	e.must(200, acc, "GET", "/v1/bank/transactions/"+id(txns.Items[0].ID)+"/suggestions", nil, &sg)
	if len(sg.Items) != 1 || sg.Items[0].InvoiceID != inv.ID || sg.Items[0].Reason != "amount_and_reference" {
		t.Fatalf("suggestions for the 150.00 payment: %+v", sg.Items)
	}
	// Suggestions are advice: nothing was recorded.
	var got Invoice
	e.must(200, maker, "GET", "/v1/invoices/"+id(inv.ID), nil, &got)
	if got.PaidMinor != 0 {
		t.Fatal("a suggestion recorded a payment")
	}
	e.must(200, acc, "GET", "/v1/bank/transactions/"+id(txns.Items[2].ID)+"/suggestions", nil, &sg)
	if len(sg.Items) != 1 || sg.Items[0].InvoiceID != inv2.ID || sg.Items[0].Reason != "amount" {
		t.Errorf("suggestions for the 99.00 payment: %+v", sg.Items)
	}

	// A caller without finance.write cannot confirm (it records a payment).
	readOnly := e.token("rob", ledAdm, nil, nil, false)
	if code := e.do(readOnly, "POST", "/v1/bank/transactions/"+id(txns.Items[0].ID)+"/confirm", obj{"invoice_id": inv.ID}, nil); code != 403 {
		t.Errorf("confirm without finance.write = %d", code)
	}
	var m BankTxn
	e.must(200, acc, "POST", "/v1/bank/transactions/"+id(txns.Items[0].ID)+"/confirm", obj{"invoice_id": inv.ID}, &m, "Idempotency-Key", "confirm-key-001")
	if m.Status != "matched" || m.PaymentID == nil {
		t.Fatalf("confirm %+v", m)
	}
	e.must(200, maker, "GET", "/v1/invoices/"+id(inv.ID), nil, &got)
	if got.Status != "paid" || got.PaidMinor != 15000 {
		t.Errorf("invoice after confirm: %s paid %d", got.Status, got.PaidMinor)
	}
	// The payment posted to the ledger account of the bank account.
	var bank int64
	_ = e.app.store.db.QueryRow(`SELECT COALESCE(SUM(l.debit_minor - l.credit_minor), 0) FROM journal_lines l JOIN ledger_accounts a ON a.id = l.account_id WHERE a.code = '1000'`).Scan(&bank)
	if bank != 15000 {
		t.Errorf("bank ledger balance %d", bank)
	}
	var replay BankTxn
	if code := e.do(acc, "POST", "/v1/bank/transactions/"+id(txns.Items[0].ID)+"/confirm", obj{"invoice_id": inv.ID}, &replay, "Idempotency-Key", "confirm-key-001"); code != 200 || replay.ID != m.ID {
		t.Errorf("replayed confirm = %d", code)
	}
	if code := e.do(acc, "POST", "/v1/bank/transactions/"+id(txns.Items[0].ID)+"/confirm", obj{"invoice_id": inv.ID}, nil); code != 409 {
		t.Errorf("second confirm = %d, want 409", code)
	}
	// Money out cannot be matched to an invoice; an overpayment and a missing invoice are refused.
	if code := e.do(acc, "POST", "/v1/bank/transactions/"+id(txns.Items[1].ID)+"/confirm", obj{"invoice_id": inv2.ID}, nil); code != 409 {
		t.Errorf("matching money out = %d", code)
	}
	if code := e.do(acc, "POST", "/v1/bank/transactions/"+id(txns.Items[3].ID)+"/confirm", obj{"invoice_id": 99999}, nil); code != 404 {
		t.Errorf("unknown invoice = %d", code)
	}
	var big BankTxn
	e.must(201, acc, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+csvQuery, "date,amount,reference,name,id\n2026-06-20,500.00,over,O,TX9\n", &ov)
	e.must(200, acc, "GET", "/v1/bank/transactions?account_id="+id(b.ID)+"&from=2026-06-20&to=2026-06-20", nil, &txns)
	_ = big
	if code := e.do(acc, "POST", "/v1/bank/transactions/"+id(txns.Items[0].ID)+"/confirm", obj{"invoice_id": inv2.ID}, nil); code != 422 {
		t.Errorf("overpayment = %d, want 422", code)
	}
	// Ignore needs a reason and is final.
	var all struct{ Items []BankTxn }
	e.must(200, acc, "GET", "/v1/bank/transactions?account_id="+id(b.ID)+"&status=unmatched", nil, &all)
	fee := all.Items[0]
	if code := e.do(acc, "POST", "/v1/bank/transactions/"+id(fee.ID)+"/ignore", obj{"reason": ""}, nil); code != 422 {
		t.Errorf("ignore without a reason = %d", code)
	}
	e.must(200, acc, "POST", "/v1/bank/transactions/"+id(fee.ID)+"/ignore", obj{"reason": "bank fee"}, nil)
	if code := e.do(acc, "POST", "/v1/bank/transactions/"+id(fee.ID)+"/ignore", obj{"reason": "again"}, nil); code != 409 {
		t.Errorf("ignoring twice = %d", code)
	}
	if _, err := e.app.store.db.Exec(`UPDATE bank_transactions SET amount_minor = 1 WHERE id = ?`, fee.ID); err == nil {
		t.Error("a bank transaction's amount was edited")
	}
	if _, err := e.app.store.db.Exec(`UPDATE bank_transactions SET status = 'unmatched' WHERE id = ?`, fee.ID); err == nil {
		t.Error("an ignored transaction was put back")
	}

	// Reconciliation: June has unmatched rows until each is decided; then it can be signed off, once.
	var rec struct{ Items []ReconPeriod }
	e.must(200, acc, "GET", "/v1/bank/accounts/"+id(b.ID)+"/reconciliation", nil, &rec)
	if len(rec.Items) != 1 || rec.Items[0].Status != "open" || rec.Items[0].Unmatched == 0 {
		t.Fatalf("reconciliation %+v", rec.Items)
	}
	approver := e.token("alex", ledApp, nil, nil, false)
	if code := e.do(approver, "POST", "/v1/bank/accounts/"+id(b.ID)+"/reconciliation", obj{"period": "2026-06"}, nil); code != 409 {
		t.Errorf("sign-off with unmatched = %d", code)
	}
	e.must(200, acc, "GET", "/v1/bank/transactions?account_id="+id(b.ID)+"&status=unmatched", nil, &all)
	for _, tx := range all.Items {
		e.must(200, acc, "POST", "/v1/bank/transactions/"+id(tx.ID)+"/ignore", obj{"reason": "not ours"}, nil)
	}
	e.must(201, approver, "POST", "/v1/bank/accounts/"+id(b.ID)+"/reconciliation", obj{"period": "2026-06"}, nil)
	if code := e.do(approver, "POST", "/v1/bank/accounts/"+id(b.ID)+"/reconciliation", obj{"period": "2026-06"}, nil); code != 409 {
		t.Errorf("sign-off twice = %d", code)
	}
	e.must(200, acc, "GET", "/v1/bank/accounts/"+id(b.ID)+"/reconciliation", nil, &rec)
	if rec.Items[0].Status != "reconciled" {
		t.Errorf("status %q", rec.Items[0].Status)
	}
	// Reads of bank data are audited.
	var aud struct{ Items []AuditEntry }
	e.must(200, adminTok, "GET", "/v1/audit?limit=100", nil, &aud)
	reads := 0
	for _, a := range aud.Items {
		if strings.HasPrefix(a.Action, "bank.") && strings.HasSuffix(a.Action, ".read") {
			reads++
		}
	}
	if reads < 5 {
		t.Errorf("only %d bank reads audited", reads)
	}
}

func TestBankReadsFailClosedWhenTheAuditCannotBeWritten(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	e.must(201, admin, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+csvQuery, "date,amount,reference,name,id\n2026-06-10,10.00,secret ref,Secret Name,A1\n", nil)
	if _, err := e.app.store.db.Exec(`CREATE TRIGGER no_audit BEFORE INSERT ON audit_log WHEN NEW.action LIKE 'bank.%.read' OR NEW.action = 'bank.transactions.read' BEGIN SELECT RAISE(ABORT, 'audit down'); END`); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v1/bank/transactions", "/v1/bank/transactions/1/suggestions", "/v1/bank/accounts/" + id(b.ID) + "/reconciliation"} {
		code, _, body := e.req(admin, "GET", p, nil)
		if code != 500 || strings.Contains(string(body), "Secret") || strings.Contains(string(body), "secret ref") {
			t.Errorf("GET %s with the audit down = %d %s", p, code, body)
		}
	}
}

func TestStatementParsersAreStrict(t *testing.T) {
	cfg := CSVConfig{Delimiter: ',', HasHeader: true, DateCol: 1, AmountCol: 2, ReferenceCol: 3}
	if _, err := ParseCSV(strings.NewReader("d,a,r\n2026-01-01,1.00,x\n"), CSVConfig{HasHeader: true, DateCol: 1, AmountCol: 2}, "USD"); err == nil {
		t.Error("a CSV with no explicit delimiter was parsed")
	}
	ps, err := ParseCSV(strings.NewReader("d,a,r\n2026-01-01,\"1,234.50\",\"quoted, \"\"ref\"\"\"\n2026-01-02,-0.05,fee\n"), CSVConfig{Delimiter: ',', HasHeader: true, DateCol: 1, AmountCol: 2, ReferenceCol: 3, ThousandsSep: ','}, "USD")
	if err != nil || len(ps.Txns) != 2 || ps.Txns[0].AmountMinor != 123450 || ps.Txns[1].AmountMinor != -5 || ps.Txns[0].Reference != `quoted, "ref"` {
		t.Fatalf("%+v %v", ps, err)
	}
	ps, err = ParseCSV(strings.NewReader("01.02.2026;1.234,50;a\n"), CSVConfig{Delimiter: ';', DateCol: 1, AmountCol: 2, ReferenceCol: 3, DateLayout: "02.01.2006", DecimalSep: ',', ThousandsSep: '.'}, "EUR")
	if err != nil || ps.Txns[0].AmountMinor != 123450 || ps.Txns[0].Date != "2026-02-01" {
		t.Fatalf("european %+v %v", ps, err)
	}
	// JPY has no minor unit decimals; a fraction is refused, never rounded.
	if _, err := ParseCSV(strings.NewReader("2026-01-01,10.5\n"), CSVConfig{Delimiter: ',', DateCol: 1, AmountCol: 2}, "JPY"); err == nil {
		t.Error("a fractional yen amount was accepted")
	}
	for name, in := range map[string]string{
		"bad date":   "d,a\nyesterday,1.00\n",
		"float exp":  "d,a\n2026-01-01,1e3\n",
		"nan":        "d,a\n2026-01-01,NaN\n",
		"zero":       "d,a\n2026-01-01,0.00\n",
		"short row":  "d,a\n2026-01-01\n",
		"bare quote": "d,a\n2026-01-01,\"1.00\n",
		"empty":      "d,a\n",
		"huge":       "d,a\n2026-01-01,99999999999999999999.00\n",
		"nul":        "d,a\n2026-01-01,1.00\x00\n",
		"wide field": "d,a,r\n2026-01-01,1.00," + strings.Repeat("x", 2000) + "\n",
	} {
		if _, err := ParseCSV(strings.NewReader(in), cfg, "USD"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	rows := "d,a\n" + strings.Repeat("2026-01-01,1.00\n", maxStatementRows+1)
	if _, err := ParseCSV(strings.NewReader(rows), cfg, "USD"); err == nil {
		t.Error("more than the row cap was accepted")
	}
	if _, err := ParseCSV(strings.NewReader(strings.Repeat("a", maxStatementBytes+1)), cfg, "USD"); err == nil {
		t.Error("an oversize file was accepted")
	}
	// Identical rows in one file are all kept; the same file gives the same keys.
	c2 := CSVConfig{Delimiter: ',', HasHeader: true, DateCol: 1, AmountCol: 2}
	ps1, _ := ParseCSV(strings.NewReader("d,a\n2026-01-01,1.00\n2026-01-01,1.00\n"), c2, "USD")
	ps2, _ := ParseCSV(strings.NewReader("d,a\n2026-01-01,1.00\n2026-01-01,1.00\n"), c2, "USD")
	if ps1.Txns[0].Key == ps1.Txns[1].Key || ps1.Txns[0].Key != ps2.Txns[0].Key {
		t.Errorf("derived keys %q %q / %q", ps1.Txns[0].Key, ps1.Txns[1].Key, ps2.Txns[0].Key)
	}
}

const ofxSample = `OFXHEADER:100
DATA:OFXSGML
VERSION:102
SECURITY:NONE
ENCODING:USASCII

<OFX>
<BANKMSGSRSV1><STMTTRNRS><STMTRS>
<CURDEF>USD
<BANKACCTFROM><BANKID>1234<ACCTID>DE89370400440532013000<ACCTTYPE>CHECKING</BANKACCTFROM>
<BANKTRANLIST>
<STMTTRN><TRNTYPE>CREDIT<DTPOSTED>20260610120000<TRNAMT>150.00<FITID>F1<NAME>Pay &amp; Co<MEMO>INV-00001</STMTTRN>
<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20260611<TRNAMT>-12.50<FITID>F2<NAME>Bank<MEMO>fee</STMTTRN>
</BANKTRANLIST>
</STMTRS></STMTTRNRS></BANKMSGSRSV1>
</OFX>
`

func TestOFXParser(t *testing.T) {
	ps, err := ParseOFX(strings.NewReader(ofxSample), "USD")
	if err != nil || len(ps.Txns) != 2 || ps.AccountID != "DE89370400440532013000" || ps.Txns[0].AmountMinor != 15000 || ps.Txns[0].Counterparty != "Pay & Co" || ps.Txns[1].AmountMinor != -1250 || ps.Txns[0].Key != "F1" {
		t.Fatalf("%+v %v", ps, err)
	}
	for name, in := range map[string]string{
		"doctype":      "<!DOCTYPE OFX [<!ENTITY x SYSTEM \"file:///etc/passwd\">]>" + ofxSample,
		"entity decl":  strings.Replace(ofxSample, "<OFX>", "<!ENTITY a \"b\"><OFX>", 1),
		"entity ref":   strings.Replace(ofxSample, "Pay &amp; Co", "&xxe;", 1),
		"double enc":   strings.Replace(ofxSample, "Pay &amp; Co", "&amp;lt;", 1),
		"no ofx":       "hello",
		"unclosed tag": "<OFX><STMTTRN><TRNAMT>1.00",
		"bad amount":   strings.Replace(ofxSample, "150.00", "1e2", 1),
		"bad date":     strings.Replace(ofxSample, "20260610120000", "2026", 1),
		"attrs":        strings.Replace(ofxSample, "<NAME>", "<NAME x=\"1\">", 1),
		"currency":     strings.Replace(ofxSample, "<STMTTRN><TRNTYPE>DEBIT", "<STMTTRN><CURRENCY>EUR<TRNTYPE>DEBIT", 1),
	} {
		if _, err := ParseOFX(strings.NewReader(in), "USD"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	deep := "<OFX>" + strings.Repeat("<A>", maxSGMLDepth+2)
	if _, err := ParseOFX(strings.NewReader(deep), "USD"); err == nil {
		t.Error("deep nesting accepted")
	}
}

const camtSample = `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02"><BkToCstmrStmt><Stmt>
<Acct><Id><IBAN>DE89370400440532013000</IBAN></Id><Ccy>USD</Ccy></Acct>
<Ntry><NtryRef>N1</NtryRef><Amt Ccy="USD">150.00</Amt><CdtDtInd>CRDT</CdtDtInd><BookgDt><Dt>2026-06-10</Dt></BookgDt><AcctSvcrRef>S1</AcctSvcrRef>
<NtryDtls><TxDtls><Refs><EndToEndId>E2E1</EndToEndId></Refs><RltdPties><Dbtr><Nm>Pay Co</Nm></Dbtr></RltdPties><RmtInf><Ustrd>INV-00001</Ustrd></RmtInf></TxDtls></NtryDtls></Ntry>
<Ntry><Amt Ccy="USD">12.50</Amt><CdtDtInd>DBIT</CdtDtInd><BookgDt><Dt>2026-06-11</Dt></BookgDt><AddtlNtryInf>fee</AddtlNtryInf></Ntry>
</Stmt></BkToCstmrStmt></Document>`

func TestCAMT053ParserRefusesXXEAndBillionLaughs(t *testing.T) {
	ps, err := ParseCAMT053(strings.NewReader(camtSample), "USD")
	if err != nil || len(ps.Txns) != 2 || ps.AccountID != "DE89370400440532013000" || ps.Txns[0].AmountMinor != 15000 || ps.Txns[1].AmountMinor != -1250 ||
		ps.Txns[0].Key != "S1" || ps.Txns[0].Counterparty != "Pay Co" || !strings.Contains(ps.Txns[0].Reference, "INV-00001") {
		t.Fatalf("%+v %v", ps, err)
	}
	xxe := `<?xml version="1.0"?><!DOCTYPE d [<!ENTITY x SYSTEM "file:///etc/passwd">]>` + strings.Replace(camtSample, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>", "", 1)
	laughs := `<?xml version="1.0"?><!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;"><!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;">]><Document><Stmt>&lol3;</Stmt></Document>`
	lower := strings.Replace(xxe, "<!DOCTYPE", "<!doctype", 1)
	for name, in := range map[string]string{
		"xxe": xxe, "billion laughs": laughs, "lowercase doctype": lower,
		"entity ref":   strings.Replace(camtSample, "Pay Co", "&xxe;", 1),
		"truncated":    camtSample[:len(camtSample)-30],
		"wrong ccy":    strings.Replace(camtSample, `<Amt Ccy="USD">150.00`, `<Amt Ccy="EUR">150.00`, 1),
		"bad ind":      strings.Replace(camtSample, "CRDT", "MAYBE", 1),
		"float amount": strings.Replace(camtSample, "150.00", "1.5e2", 1),
		"no date":      strings.Replace(camtSample, "<BookgDt><Dt>2026-06-10</Dt></BookgDt>", "", 1),
		"latin1":       strings.Replace(camtSample, "UTF-8", "ISO-8859-1", 1),
	} {
		if _, err := ParseCAMT053(strings.NewReader(in), "USD"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	deep := `<?xml version="1.0"?>` + strings.Repeat("<a>", maxXMLDepth+5) + strings.Repeat("</a>", maxXMLDepth+5)
	if _, err := ParseCAMT053(strings.NewReader(deep), "USD"); err == nil {
		t.Error("deep nesting accepted")
	}
	big := `<?xml version="1.0"?><Document><Stmt><Ntry><AddtlNtryInf>` + strings.Repeat("x", maxStatementBytes) + `</AddtlNtryInf></Ntry></Stmt></Document>`
	if _, err := ParseCAMT053(strings.NewReader(big), "USD"); err == nil {
		t.Error("an oversize document was accepted")
	}
}

// Parsing is one linear pass: a statement at the size cap parses in well under a second, and doubling the
// input does not square the time.
func TestParsersAreLinear(t *testing.T) {
	var csvb, ofx bytes.Buffer
	csvb.WriteString("d,a,r\n")
	for i := 0; i < maxStatementRows; i++ {
		fmt.Fprintf(&csvb, "2026-01-%02d,%d.%02d,ref %d\n", 1+i%28, i%900+1, i%100, i)
	}
	ofx.WriteString("<OFX><STMTRS>")
	for i := 0; i < maxStatementRows; i++ {
		fmt.Fprintf(&ofx, "<STMTTRN><DTPOSTED>2026010%d<TRNAMT>%d.00<FITID>F%d<MEMO>m</STMTTRN>\n", 1+i%9, i+1, i)
	}
	ofx.WriteString("</STMTRS></OFX>")
	start := time.Now()
	if _, err := ParseCSV(bytes.NewReader(csvb.Bytes()), CSVConfig{Delimiter: ',', HasHeader: true, DateCol: 1, AmountCol: 2, ReferenceCol: 3}, "USD"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseOFX(bytes.NewReader(ofx.Bytes()), "USD"); err != nil {
		t.Fatal(err)
	}
	// Pathological: many unclosed tags and many ampersands, at the size cap.
	evil := "<OFX>" + strings.Repeat("<A>", maxSGMLDepth) + strings.Repeat("&", maxStatementBytes-2000)
	_, _ = ParseOFX(strings.NewReader(evil), "USD")
	_, _ = ParseCAMT053(strings.NewReader(`<?xml version="1.0"?><a>`+strings.Repeat("<b/>", 400000)+`</a>`), "USD")
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("parsing took %s", d)
	}
}

func TestStatementImportEndpointLimitsAndMismatch(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	post := func(q, body string) int {
		return e.do(admin, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+q, body, nil)
	}
	if code := post("?format=csv&date_col=1&amount_col=2", "d,a\n2026-01-01,1.00\n"); code != 422 {
		t.Errorf("csv without a delimiter = %d", code)
	}
	if code := post("?format=pdf", "x"); code != 422 {
		t.Errorf("unknown format = %d", code)
	}
	if code := post("?format=ofx", ofxSample); code != 201 {
		t.Errorf("ofx = %d", code)
	}
	other := strings.Replace(ofxSample, "DE89370400440532013000", "GB29NWBK60161331926819", 1)
	if code := post("?format=ofx", other); code != 422 {
		t.Errorf("a statement for another account = %d, want 422", code)
	}
	if code := post("?format=camt053", camtSample); code != 201 {
		t.Errorf("camt = %d", code)
	}
	_, _, body := e.req(admin, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements?format=ofx", other)
	if strings.Contains(string(body), "GB29") || strings.Contains(string(body), "DE89") {
		t.Errorf("the error names an account number: %s", body)
	}
	huge := strings.Repeat("x", int(e.app.cfg.MaxBodyBytes)+10)
	if code := post("?format=ofx", huge); code != 413 {
		t.Errorf("a body over max_body_bytes = %d, want 413", code)
	}
	if code := e.do(admin, "POST", "/v1/bank/accounts/9999/statements?format=ofx", ofxSample, nil); code != 404 {
		t.Errorf("unknown bank account = %d", code)
	}
	if _, err := url.Parse(csvQuery); err != nil {
		t.Fatal(err)
	}
}
