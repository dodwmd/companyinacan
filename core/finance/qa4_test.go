package finance

import (
	"strings"
	"testing"
)

func (e *env) plan(q string, args ...any) string {
	e.t.Helper()
	rows, err := e.app.store.db.Query(`EXPLAIN QUERY PLAN `+q, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var all []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			e.t.Fatal(err)
		}
		all = append(all, detail)
	}
	return strings.Join(all, "; ")
}

// QA of #46, 1: the purge deletes invoices one foreign key check per row, and asks per invoice whether a
// credit note points at it. Without an index on each referencing column both scan a table per invoice, on the
// only writer connection.
func TestPurgeLooksUpInvoiceReferencesByIndex(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ query, index string }{
		// What SQLite runs for the foreign keys when an invoice is deleted.
		{`SELECT 1 FROM invoices WHERE original_id = ?`, "invoices_original"},
		{`SELECT 1 FROM time_entries WHERE invoice_id = ?`, "time_entries_invoice"},
		{purgeUnbillTimeQuery, "time_entries_invoice"},
		{purgeSettledInvoicesQuery, "invoices_original"},
	} {
		if got := e.plan(c.query, "2020-01-01"); !strings.Contains(got, c.index) {
			t.Errorf("%s: plan %q, want %s", c.query, got, c.index)
		}
	}
}

// QA of #46, 2: the CSV mapping is part of the request an idempotency key stands for.
func TestStatementImportIdempotencyCoversTheMapping(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	base := "/v1/bank/accounts/" + id(b.ID) + "/statements?format=csv&delimiter=comma&date_col=1&amount_col=2"
	file := "d,a,r,x\n2026-06-10,1.00,one,uno\n"
	e.must(201, admin, "POST", base+"&reference_col=3", file, nil, "Idempotency-Key", "import-key-0001")
	e.must(200, admin, "POST", base+"&reference_col=3", file, nil, "Idempotency-Key", "import-key-0001")
	e.must(422, admin, "POST", base+"&reference_col=4", file, nil, "Idempotency-Key", "import-key-0001")
}

// QA of #46, 3: an unusable mapping is the caller's mistake (422), not a conflict with the earlier import.
func TestForceWithAnInvalidMappingIsInvalid(t *testing.T) {
	e := sealedEnv(t)
	admin := e.token("ada", ledAdm, nil, nil, false)
	b := e.bankAccount(admin)
	url := "/v1/bank/accounts/" + id(b.ID) + "/statements?format=csv&delimiter=comma&date_col=1"
	file := "d,a\n2026-06-10,1.00\n"
	e.must(201, admin, "POST", url+"&amount_col=2", file, nil)
	for _, bad := range []string{"", "&amount_col=2&debit_col=2", "&amount_col=2&date_layout=nonsense"} {
		if code := e.do(admin, "POST", url+bad+"&force_duplicates=true", file, nil); code != 422 {
			t.Errorf("force with mapping %q = %d, want 422", bad, code)
		}
	}
	e.must(422, admin, "POST", "/v1/bank/accounts/"+id(b.ID)+"/statements?format=xls&force_duplicates=true", file, nil)
}
