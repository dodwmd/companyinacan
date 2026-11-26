package finance

// Property tests of the journal (E15c, specs/035-accounting). They were written before the ledger
// code: every posted entry balances to zero per currency, a reversal nets the original to zero on
// every account, the database refuses what the store refuses, and invoice rounding never leaks a
// minor unit between the document and its journal entry.

import (
	"context"
	"database/sql"
	"math/rand"
	"strings"
	"testing"
)

func ledgerStore(t *testing.T) *Store {
	t.Helper()
	e := newEnv(t)
	return e.app.store
}

func seedChart(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	ids := map[string]int64{}
	for _, a := range []AccountInput{
		{Code: "1000", Name: "Bank", Type: "asset"}, {Code: "1100", Name: "Receivable", Type: "asset"},
		{Code: "2100", Name: "Tax payable", Type: "liability"}, {Code: "3000", Name: "Equity", Type: "equity"},
		{Code: "4000", Name: "Sales", Type: "income"}, {Code: "5000", Name: "Costs", Type: "expense"},
	} {
		acc, err := s.CreateAccount(context.Background(), &Principal{Subject: "boss", Permissions: []string{PermAdmin}}, a)
		if err != nil {
			t.Fatal(err)
		}
		ids[a.Code] = acc.ID
	}
	return ids
}

func journalNetByCurrency(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	rows, err := s.db.Query(`SELECT e.currency, SUM(l.debit_minor) - SUM(l.credit_minor) FROM journal_lines l JOIN journal_entries e ON e.id = l.entry_id WHERE e.status = 'posted' GROUP BY e.currency`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var c string
		var n int64
		if err := rows.Scan(&c, &n); err != nil {
			t.Fatal(err)
		}
		out[c] = n
	}
	return out
}

// randomBalanced builds a balanced set of lines: n debit lines and one credit line (or the reverse).
func randomBalanced(r *rand.Rand, ids map[string]int64) []lineSpec {
	accts := []int64{ids["1000"], ids["1100"], ids["2100"], ids["4000"], ids["5000"]}
	n := 1 + r.Intn(5)
	var lines []lineSpec
	var total int64
	for i := 0; i < n; i++ {
		v := 1 + r.Int63n(1_000_000_000)
		total += v
		lines = append(lines, lineSpec{AccountID: accts[r.Intn(len(accts))], Debit: v})
	}
	lines = append(lines, lineSpec{AccountID: accts[r.Intn(len(accts))], Credit: total})
	if r.Intn(2) == 0 {
		for i := range lines {
			lines[i].Debit, lines[i].Credit = lines[i].Credit, lines[i].Debit
		}
	}
	return lines
}

func TestEveryPostedEntryBalancesPerCurrency(t *testing.T) {
	s := ledgerStore(t)
	ids := seedChart(t, s)
	r := rand.New(rand.NewSource(7))
	ctx := context.Background()
	for i := 0; i < 300; i++ {
		cur := []string{"USD", "EUR", "JPY", "KWD"}[r.Intn(4)]
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			_, err := s.postEntryTx(ctx, tx, postSpec{Source: "manual", Date: "2026-03-01", Currency: cur, By: "p", Lines: randomBalanced(r, ids)})
			return err
		})
		if err != nil {
			t.Fatalf("balanced entry %d rejected: %v", i, err)
		}
	}
	for cur, n := range journalNetByCurrency(t, s) {
		if n != 0 {
			t.Errorf("%s: debits minus credits = %d, want 0", cur, n)
		}
	}
}

func TestUnbalancedEntriesAreRefusedByStoreAndDatabase(t *testing.T) {
	s := ledgerStore(t)
	ids := seedChart(t, s)
	ctx := context.Background()
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := s.postEntryTx(ctx, tx, postSpec{Source: "manual", Date: "2026-03-01", Currency: "USD", By: "p",
			Lines: []lineSpec{{AccountID: ids["1000"], Debit: 100}, {AccountID: ids["4000"], Credit: 99}}})
		return err
	})
	if err == nil {
		t.Fatal("an unbalanced entry was posted")
	}
	// Bypass the store: the trigger must refuse on its own.
	tx, _ := s.db.Begin()
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT INTO journal_entries (status, source, entry_date, currency, created_by, created_at) VALUES ('draft', 'manual', '2026-03-01', 'USD', 'x', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	eid, _ := res.LastInsertId()
	_, _ = tx.Exec(`INSERT INTO journal_lines (entry_id, position, account_id, debit_minor) VALUES (?, 1, ?, 100)`, eid, ids["1000"])
	_, _ = tx.Exec(`INSERT INTO journal_lines (entry_id, position, account_id, credit_minor) VALUES (?, 2, ?, 99)`, eid, ids["4000"])
	if _, err := tx.Exec(`UPDATE journal_entries SET status = 'posted', number = 1, number_text = 'JE-1' WHERE id = ?`, eid); err == nil || !strings.Contains(err.Error(), "must balance") {
		t.Errorf("the database posted an unbalanced entry: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO journal_entries (status, source, entry_date, currency, number, number_text, created_by, created_at) VALUES ('posted', 'manual', '2026-03-01', 'USD', 2, 'JE-2', 'x', 1)`); err == nil {
		t.Error("an entry can be inserted already posted, skipping the balance check")
	}
	// A line must be a debit or a credit, never both nor neither.
	if _, err := tx.Exec(`INSERT INTO journal_lines (entry_id, position, account_id, debit_minor, credit_minor) VALUES (?, 9, ?, 5, 5)`, eid, ids["1000"]); err == nil {
		t.Error("a line with both a debit and a credit was accepted")
	}
}

func TestReversalNetsEveryAccountToZero(t *testing.T) {
	s := ledgerStore(t)
	ids := seedChart(t, s)
	r := rand.New(rand.NewSource(11))
	ctx := context.Background()
	a := &Principal{Subject: "boss", Permissions: []string{PermAdmin}}
	for i := 0; i < 50; i++ {
		var id int64
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = s.postEntryTx(ctx, tx, postSpec{Source: "manual", Date: "2026-03-01", Currency: "EUR", By: "p", Lines: randomBalanced(r, ids)})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		rev, err := s.reverseEntry(ctx, a, id, "2026-03-02", "test")
		if err != nil {
			t.Fatal(err)
		}
		if rev.ReversalOf == nil || *rev.ReversalOf != id {
			t.Fatalf("reversal does not point at the original: %+v", rev)
		}
		if _, err := s.reverseEntry(ctx, a, id, "2026-03-02", "again"); err == nil {
			t.Fatal("an entry was reversed twice")
		}
	}
	rows, err := s.db.Query(`SELECT l.account_id, SUM(l.debit_minor) - SUM(l.credit_minor) FROM journal_lines l GROUP BY l.account_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var acc, n int64
		_ = rows.Scan(&acc, &n)
		if n != 0 {
			t.Errorf("account %d nets to %d after every entry was reversed", acc, n)
		}
	}
}

func TestPostedEntriesAreImmutable(t *testing.T) {
	s := ledgerStore(t)
	ids := seedChart(t, s)
	ctx := context.Background()
	var id int64
	if err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.postEntryTx(ctx, tx, postSpec{Source: "manual", Date: "2026-03-01", Currency: "USD", By: "p",
			Lines: []lineSpec{{AccountID: ids["1000"], Debit: 100}, {AccountID: ids["4000"], Credit: 100}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE journal_entries SET memo = 'changed' WHERE id = ?`,
		`UPDATE journal_entries SET entry_date = '2025-01-01' WHERE id = ?`,
		`UPDATE journal_entries SET status = 'draft' WHERE id = ?`,
		`DELETE FROM journal_entries WHERE id = ?`,
		`UPDATE journal_lines SET debit_minor = 1 WHERE entry_id = ?`,
		`DELETE FROM journal_lines WHERE entry_id = ?`,
	} {
		if _, err := s.db.Exec(q, id); err == nil {
			t.Errorf("%q succeeded on a posted entry", q)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO journal_lines (entry_id, position, account_id, debit_minor) VALUES (?, 9, ?, 5)`, id, ids["1000"]); err == nil {
		t.Error("a line was added to a posted entry")
	}
	// An account that carries a posting keeps its code and type.
	if _, err := s.db.Exec(`UPDATE ledger_accounts SET code = '9999' WHERE id = ?`, ids["1000"]); err == nil {
		t.Error("a used account changed its code")
	}
	if _, err := s.db.Exec(`DELETE FROM ledger_accounts WHERE id = ?`, ids["1000"]); err == nil {
		t.Error("a used account was deleted")
	}
	if _, err := s.db.Exec(`UPDATE ledger_accounts SET code = '9999' WHERE id = ?`, ids["5000"]); err != nil {
		t.Errorf("an unused account could not be renumbered: %v", err)
	}
}

// Invoice rounding: the journal entry of an approved invoice carries the document's own totals, line by
// line rounded, so receivable = income + tax to the minor unit whatever the quantities and rates.
func TestInvoiceJournalMatchesDocumentTotals(t *testing.T) {
	e := newEnv(t)
	ledgerOn(t, e)
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	party := e.party(maker, "Acme Round Ltd", "customer")
	r := rand.New(rand.NewSource(3))
	for i := 0; i < 25; i++ {
		var lines []obj
		for j := 0; j < 1+r.Intn(4); j++ {
			l := line("item", 1+r.Int63n(9999), 1+r.Int63n(99999))
			l["tax_code"] = "VAT"
			lines = append(lines, l)
		}
		inv := e.invoice(maker, party, nil, lines...)
		var got Invoice
		e.must(200, boss, "POST", "/v1/invoices/"+id(inv.ID)+"/approve", nil, &got)
		var debit, credit, ar, income, tax int64
		rows, err := e.app.store.db.Query(`SELECT l.debit_minor, l.credit_minor, a.code FROM journal_lines l JOIN journal_entries j ON j.id = l.entry_id JOIN ledger_accounts a ON a.id = l.account_id WHERE j.source = 'invoice' AND j.source_ref = ?`, "invoice:"+id(inv.ID))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var d, c int64
			var code string
			_ = rows.Scan(&d, &c, &code)
			debit, credit = debit+d, credit+c
			switch code {
			case "1100":
				ar += d
			case "4000":
				income += c
			case "2100":
				tax += c
			}
		}
		_ = rows.Close()
		if debit != credit {
			t.Fatalf("invoice %d: debits %d credits %d", inv.ID, debit, credit)
		}
		if ar != got.TotalMinor || tax != got.TaxMinor || income != got.SubtotalMinor-got.DiscountMinor {
			t.Fatalf("invoice %d: journal ar=%d income=%d tax=%d, document total=%d subtotal-discount=%d tax=%d", inv.ID, ar, income, tax, got.TotalMinor, got.SubtotalMinor-got.DiscountMinor, got.TaxMinor)
		}
	}
}

func FuzzAmountToMinor(f *testing.F) {
	for _, s := range []string{"1.00", "-0.5", "1,234.56", "", "9999999999999999999999", "1e5", "0.001", ".5", "1.", "--1", "٣.٠٠"} {
		f.Add(s, 2)
	}
	f.Fuzz(func(t *testing.T, s string, exp int) {
		if exp < 0 || exp > 3 {
			return
		}
		v, err := parseDecimalMinor(s, exp, '.', ',')
		if err == nil && (v > maxMinor || v < -maxMinor) {
			t.Fatalf("%q parsed to %d beyond the cap", s, v)
		}
	})
}
