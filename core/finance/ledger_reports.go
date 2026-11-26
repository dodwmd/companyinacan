package finance

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func sortStrings(s []string) { sort.Strings(s) }

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Reports read the posted journal only, as of a date, per currency, in integer minor units. Each query
// is bounded: it runs under the read deadline, returns at most maxReportRows rows and says so when it was
// cut. Nothing is converted between currencies and nothing is revalued.

// TrialRow is one account of a trial balance.
type TrialRow struct {
	AccountID   int64  `json:"account_id"`
	Code        string `json:"code"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	DebitMinor  int64  `json:"debit_minor"`
	CreditMinor int64  `json:"credit_minor"`
	// BalanceMinor is debit minus credit: positive is a debit balance.
	BalanceMinor int64 `json:"balance_minor"`
}

// TrialCurrency is the trial balance of one currency.
type TrialCurrency struct {
	Currency    string     `json:"currency"`
	Rows        []TrialRow `json:"rows"`
	DebitMinor  int64      `json:"debit_minor"`
	CreditMinor int64      `json:"credit_minor"`
	Balanced    bool       `json:"balanced"`
}

// TrialBalance is the trial balance as of a date.
type TrialBalance struct {
	AsOf       string          `json:"as_of"`
	Currencies []TrialCurrency `json:"currencies"`
	Truncated  bool            `json:"truncated"`
}

func (s *Store) accountSums(ctx context.Context, from, to string) ([]struct {
	cur string
	row TrialRow
}, bool, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	var w where
	w.and("(e.status = 'posted')")
	if from != "" {
		w.and("(e.entry_date >= ?)", from)
	}
	w.and("(e.entry_date <= ?)", to)
	rows, err := s.rdb.QueryContext(rctx, `SELECT e.currency, a.id, a.code, a.name, a.type, SUM(l.debit_minor), SUM(l.credit_minor)
		FROM journal_lines l JOIN journal_entries e ON e.id = l.entry_id JOIN ledger_accounts a ON a.id = l.account_id
		WHERE `+w.sql()+` GROUP BY e.currency, a.id ORDER BY e.currency, a.code LIMIT ?`, append(w.args, maxReportRows+1)...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var out []struct {
		cur string
		row TrialRow
	}
	for rows.Next() {
		var cur string
		var r TrialRow
		if err := rows.Scan(&cur, &r.AccountID, &r.Code, &r.Name, &r.Type, &r.DebitMinor, &r.CreditMinor); err != nil {
			return nil, false, err
		}
		r.BalanceMinor = r.DebitMinor - r.CreditMinor
		out = append(out, struct {
			cur string
			row TrialRow
		}{cur, r})
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	trunc := len(out) > maxReportRows
	if trunc {
		out = out[:maxReportRows]
	}
	return out, trunc, nil
}

// TrialBalanceAsOf reports every account's debits and credits up to and including asOf.
func (s *Store) TrialBalanceAsOf(ctx context.Context, a *Principal, asOf string) (*TrialBalance, error) {
	if !validDate(asOf) {
		return nil, invalid("as_of", "use YYYY-MM-DD")
	}
	sums, trunc, err := s.accountSums(ctx, "", asOf)
	if err != nil {
		return nil, err
	}
	tb := &TrialBalance{AsOf: asOf, Currencies: []TrialCurrency{}, Truncated: trunc}
	var cur *TrialCurrency
	for _, x := range sums {
		if cur == nil || cur.Currency != x.cur {
			tb.Currencies = append(tb.Currencies, TrialCurrency{Currency: x.cur, Rows: []TrialRow{}})
			cur = &tb.Currencies[len(tb.Currencies)-1]
		}
		cur.Rows = append(cur.Rows, x.row)
		cur.DebitMinor += x.row.DebitMinor
		cur.CreditMinor += x.row.CreditMinor
	}
	for i := range tb.Currencies {
		tb.Currencies[i].Balanced = tb.Currencies[i].DebitMinor == tb.Currencies[i].CreditMinor
	}
	return tb, nil
}

// StatementLine is one account of a profit and loss or balance sheet. Amount is in the account's
// natural sign: income, liabilities and equity as credit minus debit, assets and expenses as debit
// minus credit.
type StatementLine struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	AmountMinor int64  `json:"amount_minor"`
}

// ProfitLossCurrency is the profit and loss of one currency.
type ProfitLossCurrency struct {
	Currency      string          `json:"currency"`
	Income        []StatementLine `json:"income"`
	Expenses      []StatementLine `json:"expenses"`
	IncomeMinor   int64           `json:"income_minor"`
	ExpensesMinor int64           `json:"expenses_minor"`
	NetMinor      int64           `json:"net_minor"`
}

// ProfitLoss is income less expenses between two dates.
type ProfitLoss struct {
	From       string               `json:"from"`
	To         string               `json:"to"`
	Currencies []ProfitLossCurrency `json:"currencies"`
	Truncated  bool                 `json:"truncated"`
}

// ProfitLossBetween reports income and expense accounts between from and to inclusive.
func (s *Store) ProfitLossBetween(ctx context.Context, a *Principal, from, to string) (*ProfitLoss, error) {
	if !validDate(from) || !validDate(to) || from > to {
		return nil, invalid("from", "use YYYY-MM-DD dates with from not after to")
	}
	sums, trunc, err := s.accountSums(ctx, from, to)
	if err != nil {
		return nil, err
	}
	rep := &ProfitLoss{From: from, To: to, Currencies: []ProfitLossCurrency{}, Truncated: trunc}
	var cur *ProfitLossCurrency
	for _, x := range sums {
		if x.row.Type != AcIncome && x.row.Type != AcExpense {
			continue
		}
		if cur == nil || cur.Currency != x.cur {
			rep.Currencies = append(rep.Currencies, ProfitLossCurrency{Currency: x.cur, Income: []StatementLine{}, Expenses: []StatementLine{}})
			cur = &rep.Currencies[len(rep.Currencies)-1]
		}
		if x.row.Type == AcIncome {
			amt := -x.row.BalanceMinor
			cur.Income = append(cur.Income, StatementLine{x.row.Code, x.row.Name, amt})
			cur.IncomeMinor += amt
		} else {
			cur.Expenses = append(cur.Expenses, StatementLine{x.row.Code, x.row.Name, x.row.BalanceMinor})
			cur.ExpensesMinor += x.row.BalanceMinor
		}
	}
	for i := range rep.Currencies {
		rep.Currencies[i].NetMinor = rep.Currencies[i].IncomeMinor - rep.Currencies[i].ExpensesMinor
	}
	return rep, nil
}

// BalanceSheetCurrency is the balance sheet of one currency. Earnings is income less expenses to date,
// which is the part of equity no closing entry has moved yet.
type BalanceSheetCurrency struct {
	Currency         string          `json:"currency"`
	Assets           []StatementLine `json:"assets"`
	Liabilities      []StatementLine `json:"liabilities"`
	Equity           []StatementLine `json:"equity"`
	AssetsMinor      int64           `json:"assets_minor"`
	LiabilitiesMinor int64           `json:"liabilities_minor"`
	EquityMinor      int64           `json:"equity_minor"`
	EarningsMinor    int64           `json:"earnings_minor"`
	Balanced         bool            `json:"balanced"`
}

// BalanceSheet is the position as of a date.
type BalanceSheet struct {
	AsOf       string                 `json:"as_of"`
	Currencies []BalanceSheetCurrency `json:"currencies"`
	Truncated  bool                   `json:"truncated"`
}

// BalanceSheetAsOf reports assets, liabilities and equity up to and including asOf.
func (s *Store) BalanceSheetAsOf(ctx context.Context, a *Principal, asOf string) (*BalanceSheet, error) {
	if !validDate(asOf) {
		return nil, invalid("as_of", "use YYYY-MM-DD")
	}
	sums, trunc, err := s.accountSums(ctx, "", asOf)
	if err != nil {
		return nil, err
	}
	rep := &BalanceSheet{AsOf: asOf, Currencies: []BalanceSheetCurrency{}, Truncated: trunc}
	var cur *BalanceSheetCurrency
	for _, x := range sums {
		if cur == nil || cur.Currency != x.cur {
			rep.Currencies = append(rep.Currencies, BalanceSheetCurrency{Currency: x.cur, Assets: []StatementLine{}, Liabilities: []StatementLine{}, Equity: []StatementLine{}})
			cur = &rep.Currencies[len(rep.Currencies)-1]
		}
		switch x.row.Type {
		case AcAsset:
			cur.Assets = append(cur.Assets, StatementLine{x.row.Code, x.row.Name, x.row.BalanceMinor})
			cur.AssetsMinor += x.row.BalanceMinor
		case AcLiability:
			cur.Liabilities = append(cur.Liabilities, StatementLine{x.row.Code, x.row.Name, -x.row.BalanceMinor})
			cur.LiabilitiesMinor -= x.row.BalanceMinor
		case AcEquity:
			cur.Equity = append(cur.Equity, StatementLine{x.row.Code, x.row.Name, -x.row.BalanceMinor})
			cur.EquityMinor -= x.row.BalanceMinor
		default: // income and expense: earnings to date, credit minus debit
			cur.EarningsMinor -= x.row.BalanceMinor
		}
	}
	for i := range rep.Currencies {
		c := &rep.Currencies[i]
		c.Balanced = c.AssetsMinor == c.LiabilitiesMinor+c.EquityMinor+c.EarningsMinor
	}
	return rep, nil
}

// LedgerLine is one posted line in a general ledger, with the running balance of its currency.
type LedgerLine struct {
	EntryID      int64  `json:"entry_id"`
	Number       string `json:"number"`
	EntryDate    string `json:"entry_date"`
	Source       string `json:"source"`
	Memo         string `json:"memo"`
	Currency     string `json:"currency"`
	DebitMinor   int64  `json:"debit_minor"`
	CreditMinor  int64  `json:"credit_minor"`
	BalanceMinor int64  `json:"balance_minor"`
}

// GeneralLedger is the lines of one account between two dates, oldest first, with opening balances.
type GeneralLedger struct {
	Account    Account          `json:"account"`
	From       string           `json:"from"`
	To         string           `json:"to"`
	Opening    map[string]int64 `json:"opening_minor"`
	Lines      []LedgerLine     `json:"lines"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

func encodeGLCursor(date string, lineID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(date + "|" + strconv.FormatInt(lineID, 10)))
}

func decodeGLCursor(s string) (date string, id int64, ok bool) {
	if s == "" {
		return "", 0, true
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return "", 0, false
	}
	d, n, found := strings.Cut(string(b), "|")
	id, err = strconv.ParseInt(n, 10, 64)
	return d, id, found && err == nil && validDate(d) && id > 0
}

// GeneralLedgerOf lists an account's posted lines. The balance column is debit minus credit per currency,
// carried across pages by recomputing the balance before the cursor.
func (s *Store) GeneralLedgerOf(ctx context.Context, a *Principal, code, from, to, cursor string, limit int) (*GeneralLedger, error) {
	if !validDate(from) || !validDate(to) || from > to {
		return nil, invalid("from", "use YYYY-MM-DD dates with from not after to")
	}
	curDate, curID, ok := decodeGLCursor(cursor)
	if !ok {
		return nil, invalid("cursor", "invalid cursor")
	}
	limit = clampLimit(limit)
	cancel, rctx := readCtx(ctx)
	defer cancel()
	ac, err := scanAccount(s.rdb.QueryRowContext(rctx, `SELECT `+accountCols+` FROM ledger_accounts WHERE code = ?`, code))
	if err != nil {
		return nil, ErrNotFound
	}
	startDate, startID := from, int64(0)
	if curID > 0 {
		startDate, startID = curDate, curID
	}
	// Balance of everything strictly before the first line of this page.
	opening := map[string]int64{}
	rows, err := s.rdb.QueryContext(rctx, `SELECT e.currency, SUM(l.debit_minor) - SUM(l.credit_minor) FROM journal_lines l JOIN journal_entries e ON e.id = l.entry_id
		WHERE (l.account_id = ?) AND (e.status = 'posted') AND ((e.entry_date < ?) OR (e.entry_date = ? AND l.id < ?)) GROUP BY e.currency`, ac.ID, startDate, startDate, startID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c string
		var n int64
		if err := rows.Scan(&c, &n); err != nil {
			_ = rows.Close()
			return nil, err
		}
		opening[c] = n
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	lr, err := s.rdb.QueryContext(rctx, `SELECT l.id, e.id, e.number_text, e.entry_date, e.source, e.memo, e.currency, l.debit_minor, l.credit_minor
		FROM journal_lines l JOIN journal_entries e ON e.id = l.entry_id
		WHERE (l.account_id = ?) AND (e.status = 'posted') AND (e.entry_date <= ?) AND ((e.entry_date > ?) OR (e.entry_date = ? AND l.id >= ?))
		ORDER BY e.entry_date, l.id LIMIT ?`, ac.ID, to, startDate, startDate, startID, limit+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lr.Close() }()
	gl := &GeneralLedger{Account: *ac, From: from, To: to, Opening: opening, Lines: []LedgerLine{}}
	running := map[string]int64{}
	for k, v := range opening {
		running[k] = v
	}
	for lr.Next() {
		var lid int64
		var l LedgerLine
		if err := lr.Scan(&lid, &l.EntryID, &l.Number, &l.EntryDate, &l.Source, &l.Memo, &l.Currency, &l.DebitMinor, &l.CreditMinor); err != nil {
			return nil, err
		}
		if len(gl.Lines) == limit {
			gl.NextCursor = encodeGLCursor(l.EntryDate, lid)
			break
		}
		running[l.Currency] += l.DebitMinor - l.CreditMinor
		l.BalanceMinor = running[l.Currency]
		gl.Lines = append(gl.Lines, l)
	}
	if err := lr.Err(); err != nil {
		return nil, fmt.Errorf("general ledger: %w", err)
	}
	return gl, nil
}
