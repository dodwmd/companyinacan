package finance

import (
	"context"
	"sort"
	"time"
)

// SeriesInfo is one numbering series and the last number issued from it.
type SeriesInfo struct {
	Code string `json:"code"`
	Last int64  `json:"last"`
}

// ListSeries returns the numbering series in use (read-only; numbers are only ever taken by an
// approval, in the same transaction, so they stay gap-free).
func (s *Store) ListSeries(ctx context.Context) ([]SeriesInfo, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT code, next_number - 1 FROM series ORDER BY code LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []SeriesInfo{}
	for rows.Next() {
		var si SeriesInfo
		if err := rows.Scan(&si.Code, &si.Last); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// CurrencyAmount is an amount in one currency's minor unit. Currencies are never added together.
type CurrencyAmount struct {
	Currency string `json:"currency"`
	Minor    int64  `json:"minor"`
}

// DashboardStats are aggregates only: no party, document or person is named.
type DashboardStats struct {
	AsOf              string           `json:"as_of"`
	OverdueCount      int              `json:"overdue_count"`
	Overdue           []CurrencyAmount `json:"overdue"`
	IssuedThisMonth   []CurrencyAmount `json:"issued_this_month"`
	ReceivedThisMonth []CurrencyAmount `json:"received_this_month"`
}

func sumByCurrency(m map[string]int64) []CurrencyAmount {
	out := make([]CurrencyAmount, 0, len(m))
	for c, v := range m {
		out = append(out, CurrencyAmount{Currency: c, Minor: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}

// Dashboard computes the headline numbers for the caller's scope as of a date (the month of that
// date is "this month"). Every query carries the scope clauses.
func (s *Store) Dashboard(ctx context.Context, a *Principal, asOf string) (*DashboardStats, error) {
	if !validDate(asOf) {
		return nil, invalid("as_of", "use YYYY-MM-DD")
	}
	t := parseDate(asOf)
	from := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	to := time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	cancel, rctx := readCtx(ctx)
	defer cancel()
	st := &DashboardStats{AsOf: asOf}

	var w where
	scopeWhere(a, &w, "party_id", "project")
	w.and("(kind = 'invoice' AND status IN ('approved', 'sent', 'partially_paid') AND due_date < ? AND total_minor - paid_minor - credited_minor > 0)", asOf)
	rows, err := s.rdb.QueryContext(rctx, `SELECT currency, COUNT(*), SUM(total_minor - paid_minor - credited_minor) FROM invoices WHERE `+w.sql()+` GROUP BY currency`, w.args...)
	if err != nil {
		return nil, err
	}
	over := map[string]int64{}
	for rows.Next() {
		var cur string
		var n int
		var sum int64
		if err := rows.Scan(&cur, &n, &sum); err != nil {
			_ = rows.Close()
			return nil, err
		}
		over[cur] = sum
		st.OverdueCount += n
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	st.Overdue = sumByCurrency(over)

	var wi where
	scopeWhere(a, &wi, "party_id", "project")
	wi.and("(kind = 'invoice' AND status NOT IN ('draft', 'void') AND issue_date >= ? AND issue_date <= ?)", from, to)
	if st.IssuedThisMonth, err = s.sumRows(rctx, `SELECT currency, SUM(total_minor) FROM invoices WHERE `+wi.sql()+` GROUP BY currency`, wi.args); err != nil {
		return nil, err
	}

	var wp where
	scopeWhere(a, &wp, "i.party_id", "i.project")
	wp.and("(p.received_on >= ? AND p.received_on <= ?)", from, to)
	if st.ReceivedThisMonth, err = s.sumRows(rctx, `SELECT p.currency, SUM(p.amount_minor) FROM payments p JOIN invoices i ON i.id = p.invoice_id WHERE `+wp.sql()+` GROUP BY p.currency`, wp.args); err != nil {
		return nil, err
	}
	return st, nil
}

// PartyNames returns the names of parties in scope, for report rows. It reads no contact data, so
// it is not an audited read; ids outside the scope are simply absent.
func (s *Store) PartyNames(ctx context.Context, a *Principal, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 500 {
		ids = ids[:500]
	}
	var w where
	c, args := partyClause(a, "id")
	w.scope(c, args)
	in := make([]any, len(ids))
	for i, id := range ids {
		in[i] = id
	}
	w.and("(id IN ("+placeholders(len(ids))+"))", in...)
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT id, name FROM parties WHERE `+w.sql(), w.args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

func (s *Store) sumRows(ctx context.Context, q string, args []any) ([]CurrencyAmount, error) {
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	m := map[string]int64{}
	for rows.Next() {
		var cur string
		var sum int64
		if err := rows.Scan(&cur, &sum); err != nil {
			return nil, err
		}
		m[cur] = sum
	}
	return sumByCurrency(m), rows.Err()
}
