package finance

import (
	"context"
	"sort"
)

// AgingBuckets are outstanding balances by how overdue they are, in one currency's minor
// unit. Current means not yet due.
type AgingBuckets struct {
	Current   int64 `json:"current"`
	Days1to30 int64 `json:"days_1_30"`
	Days31_60 int64 `json:"days_31_60"`
	Days61_90 int64 `json:"days_61_90"`
	Over90    int64 `json:"over_90"`
	Total     int64 `json:"total"`
}

func (b *AgingBuckets) add(days, bal int64) {
	switch {
	case days <= 0:
		b.Current += bal
	case days <= 30:
		b.Days1to30 += bal
	case days <= 60:
		b.Days31_60 += bal
	case days <= 90:
		b.Days61_90 += bal
	default:
		b.Over90 += bal
	}
	b.Total += bal
}

// AgingParty is one customer's buckets within a currency.
type AgingParty struct {
	PartyID int64 `json:"party_id"`
	AgingBuckets
}

// AgingCurrency is the report for one currency. Currencies are never added together:
// converting at today's rate would change what the documents say.
type AgingCurrency struct {
	Currency string       `json:"currency"`
	Totals   AgingBuckets `json:"totals"`
	Parties  []AgingParty `json:"parties"`
}

// AgingReport is the receivables aging as of a date.
type AgingReport struct {
	AsOf       string          `json:"as_of"`
	Currencies []AgingCurrency `json:"currencies"`
}

// Aging reports the unpaid balance of issued invoices in scope, by days past due as of the
// given date. Credit notes reduce the balance of their invoice; void and paid invoices
// are excluded.
func (s *Store) Aging(ctx context.Context, a *Principal, asOf string) (*AgingReport, error) {
	if !validDate(asOf) {
		return nil, invalid("as_of", "use YYYY-MM-DD")
	}
	var w where
	scopeWhere(a, &w, "party_id", "project")
	w.and("(kind = 'invoice' AND status IN ('approved', 'sent', 'partially_paid') AND total_minor - paid_minor - credited_minor > 0)")
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT currency, party_id, total_minor - paid_minor - credited_minor, CAST(julianday(?) - julianday(due_date) AS INTEGER) FROM invoices WHERE `+w.sql(), append([]any{asOf}, w.args...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	byCur := map[string]*AgingCurrency{}
	byParty := map[string]map[int64]*AgingParty{}
	for rows.Next() {
		var cur string
		var party, bal, days int64
		if err := rows.Scan(&cur, &party, &bal, &days); err != nil {
			return nil, err
		}
		c := byCur[cur]
		if c == nil {
			c = &AgingCurrency{Currency: cur, Parties: []AgingParty{}}
			byCur[cur] = c
			byParty[cur] = map[int64]*AgingParty{}
		}
		c.Totals.add(days, bal)
		p := byParty[cur][party]
		if p == nil {
			p = &AgingParty{PartyID: party}
			byParty[cur][party] = p
		}
		p.add(days, bal)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rep := &AgingReport{AsOf: asOf, Currencies: []AgingCurrency{}}
	for cur, c := range byCur {
		for _, p := range byParty[cur] {
			c.Parties = append(c.Parties, *p)
		}
		sort.Slice(c.Parties, func(i, j int) bool { return c.Parties[i].PartyID < c.Parties[j].PartyID })
		rep.Currencies = append(rep.Currencies, *c)
	}
	sort.Slice(rep.Currencies, func(i, j int) bool { return rep.Currencies[i].Currency < rep.Currencies[j].Currency })
	return rep, nil
}
