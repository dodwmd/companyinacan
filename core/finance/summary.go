package finance

import (
	"context"
	"net/http"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// The `summary` capability (specs/053-business-overview): overdue invoices and receivables by
// currency, contracts awaiting signature and contracts ending or renewing soon. Counts and sums
// only, under the same party and project scope clauses as every other read.

// contractHorizonDays is how far ahead a contract's end or renewal date is reported.
const contractHorizonDays = 60

// SummaryStats are the figures of one principal. Currencies are never added together.
type SummaryStats struct {
	OverdueCount, OutstandingCount   int
	Overdue, Outstanding             []CurrencyAmount
	AwaitingSignature, ContractsSoon int
}

func (s *Store) outstanding(ctx context.Context, a *Principal, overdueBefore string) (int, []CurrencyAmount, error) {
	var w where
	scopeWhere(a, &w, "party_id", "project")
	w.and("(kind = 'invoice' AND status IN ('approved', 'sent', 'partially_paid') AND total_minor - paid_minor - credited_minor > 0)")
	if overdueBefore != "" {
		w.and("(due_date < ?)", overdueBefore)
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT currency, COUNT(*), SUM(total_minor - paid_minor - credited_minor) FROM invoices WHERE `+w.sql()+` GROUP BY currency`, w.args...)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()
	total, by := 0, map[string]int64{}
	for rows.Next() {
		var cur string
		var n int
		var sum int64
		if err := rows.Scan(&cur, &n, &sum); err != nil {
			return 0, nil, err
		}
		by[cur], total = sum, total+n
	}
	return total, sumByCurrency(by), rows.Err()
}

// Summary computes the figures for the caller's scope as of a date.
func (s *Store) Summary(ctx context.Context, a *Principal, asOf string) (*SummaryStats, error) {
	if !validDate(asOf) {
		return nil, invalid("as_of", "use YYYY-MM-DD")
	}
	until := parseDate(asOf).AddDate(0, 0, contractHorizonDays).Format(time.DateOnly)
	cancel, rctx := readCtx(ctx)
	defer cancel()
	st := &SummaryStats{}
	var err error
	if st.OverdueCount, st.Overdue, err = s.outstanding(rctx, a, asOf); err != nil {
		return nil, err
	}
	if st.OutstandingCount, st.Outstanding, err = s.outstanding(rctx, a, ""); err != nil {
		return nil, err
	}
	// Contracts are scoped by party, as in ListContracts.
	var w where
	w.scope(partyClause(a, "c.party_id"))
	w.and("(c.status = 'sent')")
	if err := s.rdb.QueryRowContext(rctx, `SELECT COUNT(*) FROM contracts c WHERE `+w.sql(), w.args...).Scan(&st.AwaitingSignature); err != nil {
		return nil, err
	}
	var x where
	x.scope(partyClause(a, "c.party_id"))
	x.and("(c.status IN ('signed', 'active') AND ((c.end_date >= ? AND c.end_date <= ?) OR (c.renewal_date >= ? AND c.renewal_date <= ?)))", asOf, until, asOf, until)
	if err := s.rdb.QueryRowContext(rctx, `SELECT COUNT(*) FROM contracts c WHERE `+x.sql(), x.args...).Scan(&st.ContractsSoon); err != nil {
		return nil, err
	}
	return st, nil
}

func summaryAmounts(in []CurrencyAmount) []modclient.SummaryAmount {
	out := make([]modclient.SummaryAmount, len(in))
	for i, c := range in {
		out[i] = modclient.SummaryAmount{Currency: c.Currency, Minor: c.Minor}
	}
	return out
}

// summaryBuilder answers for a person as can-core describes them, with the same mapping as their
// personal access tokens and sessions. Without finance.read (which write, approve and admin carry)
// there is nothing to see; ledger permissions alone do not carry it.
func summaryBuilder(s *Store, now func() time.Time) modclient.SummaryBuilder {
	return func(ctx context.Context, subject string, permissions []string) ([]modclient.SummaryFigure, error) {
		p, ok := personalPrincipal(subject, permissions)
		if !ok || !p.Can(PermRead) {
			return nil, nil
		}
		st, err := s.Summary(ctx, p, now().UTC().Format(time.DateOnly))
		if err != nil {
			return nil, err
		}
		return []modclient.SummaryFigure{
			{Key: "invoices.overdue", Count: int64(st.OverdueCount), Amounts: summaryAmounts(st.Overdue)},
			{Key: "invoices.outstanding", Count: int64(st.OutstandingCount), Amounts: summaryAmounts(st.Outstanding)},
			{Key: "contracts.awaiting_signature", Count: int64(st.AwaitingSignature)},
			{Key: "contracts.expiring", Count: int64(st.ContractsSoon)},
		}, nil
	}
}

// summaryLimit counts failures on /v1/summary like every other failed authentication.
func (a *api) summaryLimit() modclient.SummaryLimit {
	return modclient.SummaryLimit{
		Client: func(r *http.Request) string { return clientIP(r, a.trustProxy) },
		Failed: func(ip string) bool { ok, _ := a.failures.allow(ip); return ok },
	}
}
