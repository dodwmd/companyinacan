package finance

import (
	"context"
	"encoding/csv"
	"io"
	"strconv"
	"strings"
)

// Num is a cell already formatted as a number by this package (never user text), so it is
// written as is. Everything else in a CSV is text and goes through csvSafe.
type Num string

// csvSafe defuses spreadsheet formula injection (OWASP "CSV injection"): a text cell that
// begins with = + - @ or a tab or carriage return (after optional spaces) would be run as a
// formula by Excel, LibreOffice or Sheets, so it is prefixed with an apostrophe, which the
// spreadsheet shows as plain text.
func csvSafe(s string) string {
	t := strings.TrimLeft(s, " ")
	if t != "" && strings.ContainsRune("=+-@\t\r\n", rune(t[0])) {
		return "'" + s
	}
	return s
}

type csvOut struct{ cw *csv.Writer }

func newCSV(w io.Writer, header []string) (*csvOut, error) {
	c := &csvOut{cw: csv.NewWriter(w)}
	return c, c.cw.Write(header)
}

// row writes one record: Num cells as they are, every other cell as defused text.
func (c *csvOut) row(cells ...any) error {
	rec := make([]string, len(cells))
	for i, cell := range cells {
		switch v := cell.(type) {
		case Num:
			rec[i] = string(v)
		case string:
			rec[i] = csvSafe(strings.ReplaceAll(v, "\x00", ""))
		}
	}
	return c.cw.Write(rec)
}

func (c *csvOut) close() error { c.cw.Flush(); return c.cw.Error() }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

const exportLimit = 50000

// ExportInvoicesCSV writes invoices in scope as CSV. Exports are audited.
func (s *Store) ExportInvoicesCSV(ctx context.Context, a *Principal, f InvoiceFilter, w io.Writer) error {
	if err := s.auditRead(ctx, a, "export.invoices", "", map[string]any{"status": f.Status, "from": f.From, "to": f.To}); err != nil {
		return err
	}
	var wh where
	scopeWhere(a, &wh, "party_id", "project")
	if f.Status != "" {
		wh.and("(status = ?)", f.Status)
	}
	if f.From != "" {
		wh.and("(issue_date >= ?)", f.From)
	}
	if f.To != "" {
		wh.and("(issue_date <= ?)", f.To)
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+invCols+` FROM invoices WHERE `+wh.sql()+` ORDER BY id LIMIT ?`, append(wh.args, exportLimit)...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	out, err := newCSV(w, []string{"number", "kind", "status", "party_id", "project", "currency", "issue_date", "due_date", "total", "paid", "credited", "balance", "base_currency", "base_total"})
	if err != nil {
		return err
	}
	for rows.Next() {
		v, err := scanInvoice(rows)
		if err != nil {
			return err
		}
		if err := out.row(deref(v.Number), v.Kind, v.Status, Num(itoa(v.PartyID)), v.Project, v.Currency, v.IssueDate, v.DueDate,
			Num(FormatMinor(v.TotalMinor, v.Currency)), Num(FormatMinor(v.PaidMinor, v.Currency)), Num(FormatMinor(v.CreditedMinor, v.Currency)), Num(FormatMinor(v.BalanceMinor, v.Currency)),
			v.BaseCurrency, Num(FormatMinor(v.BaseTotalMinor, v.BaseCurrency))); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return out.close()
}

// ExportPaymentsCSV writes the payments of invoices in scope as CSV. Exports are audited.
func (s *Store) ExportPaymentsCSV(ctx context.Context, a *Principal, from, to string, w io.Writer) error {
	if err := s.auditRead(ctx, a, "export.payments", "", map[string]any{"from": from, "to": to}); err != nil {
		return err
	}
	var wh where
	scopeWhere(a, &wh, "i.party_id", "i.project")
	if from != "" {
		wh.and("(p.received_on >= ?)", from)
	}
	if to != "" {
		wh.and("(p.received_on <= ?)", to)
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT p.id, i.number_text, p.amount_minor, p.currency, p.method, p.reference, p.received_on, p.source, p.external_id FROM payments p JOIN invoices i ON i.id = p.invoice_id WHERE `+wh.sql()+` ORDER BY p.id LIMIT ?`, append(wh.args, exportLimit)...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	out, err := newCSV(w, []string{"payment_id", "invoice", "amount", "currency", "method", "reference", "received_on", "source", "external_id"})
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, amt int64
		var num, cur, method, ref, on, src, ext string
		if err := rows.Scan(&id, &num, &amt, &cur, &method, &ref, &on, &src, &ext); err != nil {
			return err
		}
		if err := out.row(Num(itoa(id)), num, Num(FormatMinor(amt, cur)), cur, method, ref, on, src, ext); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return out.close()
}

// ExportExpensesCSV writes expenses in scope as CSV. Exports are audited.
func (s *Store) ExportExpensesCSV(ctx context.Context, a *Principal, status string, w io.Writer) error {
	if err := s.auditRead(ctx, a, "export.expenses", "", map[string]any{"status": status}); err != nil {
		return err
	}
	var wh where
	c, args := ownerClause(a, "owner")
	wh.scope(c, args)
	c, args = projectClause(a, "project")
	wh.scope(c, args)
	if len(a.Parties) > 0 {
		wh.and("(1 = 0)") // party-scoped tokens have no business with party-less records
	}
	if status != "" {
		wh.and("(status = ?)", status)
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT id, owner, description, amount_minor, currency, category, incurred_on, project, status, reimbursement_ref FROM expenses WHERE `+wh.sql()+` ORDER BY id LIMIT ?`, append(wh.args, exportLimit)...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	out, err := newCSV(w, []string{"expense_id", "person", "description", "amount", "currency", "category", "incurred_on", "project", "status", "reimbursement_ref"})
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, amt int64
		var owner, desc, cur, cat, on, proj, st, ref string
		if err := rows.Scan(&id, &owner, &desc, &amt, &cur, &cat, &on, &proj, &st, &ref); err != nil {
			return err
		}
		if err := out.row(Num(itoa(id)), owner, desc, Num(FormatMinor(amt, cur)), cur, cat, on, proj, st, ref); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return out.close()
}
