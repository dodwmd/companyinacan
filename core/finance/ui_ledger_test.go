package finance

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The accounting pages (E15c): golden HTML, the accessibility checklist, hostile bank text, the forms, and
// who may see them at all.

type ledgerUI struct {
	u                      *uiEnv
	acct, reader, plain    *client
	inv                    *Invoice
	sealed                 BankAccount
	txnPay, txnFee, txnOut int64
}

func newLedgerUI(t *testing.T) *ledgerUI {
	t.Helper()
	u := newUIEnv(t)
	sl, _ := NewSealer(sealHex)
	u.app.store.sealer = sl
	ledgerOn(t, u.env)
	maker, boss := u.token("user:maker", rw, nil, nil, false), u.token("user:boss", app, nil, nil, false)
	admin := u.token("user:ada", ledAdm, nil, nil, false)
	party := u.party(maker, "Pay Co", "customer")
	inv := u.approveInvoice(maker, boss, party, obj{"issue_date": "2026-06-01"}, line("Consulting", 1000, 15000))
	var b BankAccount
	u.must(201, admin, "POST", "/v1/bank/accounts", obj{"name": "Operating", "currency": "USD", "number": "DE89 3704 0044 0532 0130 00", "ledger_account_code": "1000"}, &b)
	file := "date,amount,reference,name,id\n2026-06-10,150.00," + *inv.Number + ",<script>alert(1)</script> Mallory,TX1\n2026-06-11,-12.50,Bank fee,Bank,TX2\n2026-06-12,3.00,\"=HYPERLINK(\"\"http://x\"\")\",Eve,TX3\n"
	u.must(201, admin, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements"+csvQuery, file, nil)
	var txns struct{ Items []BankTxn }
	u.must(200, admin, "GET", "/v1/bank/transactions?account_id="+id(b.ID), nil, &txns)
	// a posted manual journal and a draft
	drafter, approver := u.token("user:dana", acct, nil, nil, false), u.token("user:alex", ledApp, nil, nil, false)
	var j JournalEntry
	u.must(201, drafter, "POST", "/v1/ledger/journals", manual("2026-06-15", "USD", "5000", "1000", 4200), &j)
	u.must(200, approver, "POST", "/v1/ledger/journals/"+id(j.ID)+"/post", nil, nil)
	u.must(201, drafter, "POST", "/v1/ledger/journals", manual("2026-06-16", "USD", "5000", "1000", 100), nil)
	return &ledgerUI{u: u, inv: inv, sealed: b, txnPay: txns.Items[0].ID, txnFee: txns.Items[1].ID, txnOut: txns.Items[2].ID,
		acct:   u.login(t, u.token("user:acct", []string{"finance.read", "finance.write", "finance.ledger.read", "finance.ledger.write"}, nil, nil, false)),
		reader: u.login(t, u.token("user:led-ro", []string{"finance.ledger.read"}, nil, nil, false)),
		plain:  u.login(t, u.token("user:plain", []string{"finance.read", "finance.write", "finance.approve"}, nil, nil, false))}
}

func (l *ledgerUI) shots(t *testing.T) []shot {
	t.Helper()
	var out []shot
	add := func(name string, c *client, path string) {
		w := c.get(path)
		if w.Code != 200 {
			t.Fatalf("%s %s = %d %s", name, path, w.Code, w.Body.String())
		}
		out = append(out, shot{name, w.Body.String()})
	}
	add("ledger-accounts", l.reader, "/ledger/accounts")
	add("ledger-journals", l.reader, "/ledger/journals")
	add("ledger-journals-draft", l.reader, "/ledger/journals?status=draft")
	add("ledger-journal", l.reader, "/ledger/journals/2")
	add("ledger-trial-balance", l.reader, "/ledger/trial-balance?as_of=2026-12-31")
	add("ledger-trial-balance-empty", l.reader, "/ledger/trial-balance?as_of=2020-01-01")
	add("bank-reconciliation-accountant", l.acct, "/bank/reconciliation")
	add("bank-reconciliation-reader", l.reader, "/bank/reconciliation")
	return out
}

func TestUILedgerGoldenAndAccessibility(t *testing.T) {
	l := newLedgerUI(t)
	for _, s := range l.shots(t) {
		if p := a11yProblemsUI(s.html); len(p) > 0 {
			t.Errorf("%s: %v", s.name, p)
		}
		got := normaliseUI(s.html)
		path := filepath.Join("testdata", "ui", s.name+".html")
		if *updateGolden {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // test fixture
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path) //nolint:gosec // test fixture
		if err != nil {
			t.Fatalf("%s: %v (run: go test ./core/finance -run TestUILedgerGolden -update)", s.name, err)
		}
		if string(want) != got {
			t.Errorf("%s differs from %s (run with -update after reviewing the change)", s.name, path)
		}
		if strings.Contains(got, "<script>alert") || strings.Contains(got, `onerror=`) {
			t.Errorf("%s holds raw hostile markup", s.name)
		}
	}
}

func TestUILedgerPagesShowTheRightThings(t *testing.T) {
	l := newLedgerUI(t)
	mustHave(t, l.reader.get("/ledger/accounts"), "Chart of accounts", "Accounts receivable", "Sales tax payable")
	w := l.reader.get("/ledger/journals/2")
	mustHave(t, w, "JE-00002", "5000 General expenses", "42.00", "A posted entry cannot be changed")
	if strings.Contains(w.Body.String(), "<form") && strings.Contains(w.Body.String(), "post") {
		// the layout has the logout form only
		if strings.Count(w.Body.String(), "<form") != 1 {
			t.Error("the journal page has a form: it is read-only")
		}
	}
	mustHave(t, l.reader.get("/ledger/trial-balance?as_of=2026-12-31"), "Debits equal credits", "USD")
	q := l.acct.get("/bank/reconciliation")
	mustHave(t, q, "Operating (USD, ****3000)", "Suggested invoice", "amount and reference", "Confirm match", "Money out: cannot be matched")
	if strings.Contains(q.Body.String(), "DE89") || strings.Contains(q.Body.String(), "0532") {
		t.Error("the reconciliation page shows the account number")
	}
	// A reader sees the queue but has no forms.
	if r := l.reader.get("/bank/reconciliation").Body.String(); strings.Contains(r, "Confirm match") || strings.Contains(r, "/confirm") {
		t.Error("a read-only token is offered the confirm form")
	}
	// Navigation shows the accounting pages only to those who may open them.
	if !strings.Contains(l.reader.get("/ledger/accounts").Body.String(), `href="/bank/reconciliation"`) {
		t.Error("no bank link in the navigation of a ledger reader")
	}
	if strings.Contains(l.plain.get("/").Body.String(), "/ledger/accounts") {
		t.Error("the accounting link is shown to a token without ledger permissions")
	}
}

func TestUIConfirmAndIgnoreForms(t *testing.T) {
	l := newLedgerUI(t)
	post := func(c *client, path string, f url.Values) int {
		f.Set("version", "1") // these forms carry no record version; this skips the page-open step of the helper
		return c.post(path, f).Code
	}
	pay := "/bank/transactions/" + id(l.txnPay)
	// Without a valid idem key the form is refused, never given a server-made one.
	if code := post(l.acct, pay+"/confirm", url.Values{"invoice_id": {id(l.inv.ID)}}); code != 422 {
		t.Errorf("confirm without idem = %d", code)
	}
	if code := post(l.reader, pay+"/confirm", url.Values{"invoice_id": {id(l.inv.ID)}, "idem": {"idem-confirm-001"}}); code != 403 {
		t.Errorf("confirm with a read-only token = %d", code)
	}
	w := l.acct.post(pay+"/confirm", url.Values{"invoice_id": {id(l.inv.ID)}, "idem": {"idem-confirm-001"}, "account": {id(l.sealed.ID)}, "version": {"1"}})
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/bank/reconciliation?account="+id(l.sealed.ID)) {
		t.Fatalf("confirm = %d %q", w.Code, w.Header().Get("Location"))
	}
	var inv Invoice
	l.u.must(200, adminTok, "GET", "/v1/invoices/"+id(l.inv.ID), nil, &inv)
	if inv.Status != "paid" {
		t.Errorf("the invoice is %s after the confirm", inv.Status)
	}
	if code := post(l.acct, pay+"/confirm", url.Values{"invoice_id": {id(l.inv.ID)}, "idem": {"idem-confirm-002"}}); code != 409 {
		t.Errorf("confirming twice = %d", code)
	}
	if code := post(l.acct, "/bank/transactions/"+id(l.txnOut)+"/confirm", url.Values{"invoice_id": {"abc"}, "idem": {"idem-confirm-003"}}); code != 422 {
		t.Errorf("a bad invoice id = %d", code)
	}
	fee := "/bank/transactions/" + id(l.txnFee) + "/ignore"
	if code := post(l.acct, fee, url.Values{"reason": {""}}); code != 422 {
		t.Errorf("ignore without a reason = %d", code)
	}
	if code := post(l.acct, fee, url.Values{"reason": {"bank fee"}}); code != 303 {
		t.Errorf("ignore = %d", code)
	}
	if code := post(l.acct, fee, url.Values{"reason": {"again"}}); code != 409 {
		t.Errorf("ignore twice = %d", code)
	}
	// No CSRF token, no action.
	f := url.Values{"reason": {"x"}, "csrf": {"wrong"}}
	if c := l.acct.do("POST", "/bank/transactions/"+id(l.txnOut)+"/ignore", f, "Sec-Fetch-Site", "same-origin"); c.Code != 403 {
		t.Errorf("a bad csrf token = %d", c.Code)
	}
}

func TestUILedgerRoutesRefuseScopedAndOwnOnlyTokens(t *testing.T) {
	l := newLedgerUI(t)
	for name, tok := range map[string]string{
		"project-scoped": l.u.token("user:sp", ledAdm, nil, []string{"ALPHA"}, false),
		"own-only":       l.u.token("user:oo", ledAdm, nil, nil, true),
		"no ledger perm": l.u.token("user:nl", []string{"finance.admin"}, []int64{1}, nil, false),
	} {
		c := l.u.login(t, tok)
		for _, p := range []string{"/ledger/accounts", "/ledger/journals", "/ledger/journals/1", "/ledger/trial-balance", "/bank/reconciliation"} {
			if code := c.get(p).Code; code != 403 {
				t.Errorf("%s: GET %s = %d, want 403", name, p, code)
			}
		}
		if code := c.post("/bank/transactions/1/ignore", url.Values{"reason": {"x"}, "version": {"1"}}).Code; code != 403 {
			t.Errorf("%s: POST ignore = %d", name, code)
		}
		if strings.Contains(c.get("/").Body.String(), "/ledger/accounts") {
			t.Errorf("%s: the navigation offers the accounting pages", name)
		}
	}
	if code := l.reader.get("/ledger/journals/999").Code; code != 404 {
		t.Errorf("unknown journal = %d", code)
	}
	if code := l.reader.get("/ledger/trial-balance?as_of=nope").Code; code != 422 && code != 400 {
		t.Errorf("a bad date = %d", code)
	}
}
