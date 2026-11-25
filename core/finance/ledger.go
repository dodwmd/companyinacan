package finance

// The ledger (E15c, specs/035-accounting): chart of accounts, double-entry journal, period close and
// the reports built on them. This is a tool, not accounting, tax or legal advice (docs/accounting.md).
//
// Money rules are the finance module's: int64 minor units, no floats. An entry is in one currency, so
// "balanced" means sum(debit) = sum(credit) in that currency's minor unit; balances are reported per
// currency and are never converted (there is no revaluation).

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	EventJournalPosted = "journal.posted.v1"

	// maxJournalLines bounds one entry.
	maxJournalLines = 200
	// maxReportRows bounds the rows a report query may return; more is reported as truncated.
	maxReportRows = 2000
)

// Account types.
const (
	AcAsset     = "asset"
	AcLiability = "liability"
	AcEquity    = "equity"
	AcIncome    = "income"
	AcExpense   = "expense"
)

var accountTypes = []string{AcAsset, AcLiability, AcEquity, AcIncome, AcExpense}

// Mapping keys and the account type each must point at.
var mappingTypes = map[string]string{
	"ar": AcAsset, "cash": AcAsset, "income": AcIncome, "tax_payable": AcLiability,
	"expense": AcExpense, "expense_payable": AcLiability, "opening_equity": AcEquity,
}

//go:embed ledger_template.json
var ledgerTemplateJSON []byte

// Account is one account of the chart.
type Account struct {
	ID        int64     `json:"id"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	TaxCode   string    `json:"tax_code"`
	Active    bool      `json:"active"`
	Used      bool      `json:"used"`
	Version   int64     `json:"version"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// AccountInput creates an account.
type AccountInput struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	TaxCode string `json:"tax_code"`
}

// AccountPatch edits an account. Code, type and tax code may change only while the account is unused.
type AccountPatch struct {
	Code    *string `json:"code"`
	Name    *string `json:"name"`
	Type    *string `json:"type"`
	TaxCode *string `json:"tax_code"`
	Active  *bool   `json:"active"`
}

const accountCols = `id, code, name, type, tax_code, active, used, version, created_by, created_at`

func scanAccount(sc scanner) (*Account, error) {
	var a Account
	var act, used int
	var c int64
	if err := sc.Scan(&a.ID, &a.Code, &a.Name, &a.Type, &a.TaxCode, &act, &used, &a.Version, &a.CreatedBy, &c); err != nil {
		return nil, err
	}
	a.Active, a.Used, a.CreatedAt = act == 1, used == 1, fromUnix(c)
	return &a, nil
}

func (in *AccountInput) check(ctx context.Context, q dbq) error {
	var p problems
	if !codeRE.MatchString(in.Code) {
		p.add("code", "1 to 30 letters, digits, dot, dash or underscore, starting with a letter or digit")
	}
	if strings.TrimSpace(in.Name) == "" || !oneLineText(in.Name, maxName) {
		p.add("name", "required, one line, at most %d characters", maxName)
	}
	if !contains(accountTypes, in.Type) {
		p.add("type", "must be one of %s", strings.Join(accountTypes, ", "))
	}
	if in.TaxCode != "" {
		var one int
		if err := q.QueryRowContext(ctx, `SELECT 1 FROM tax_rates WHERE code = ?`, in.TaxCode).Scan(&one); err != nil {
			p.add("tax_code", "no such tax rate")
		}
	}
	return p.err()
}

func contains(ls []string, v string) bool {
	for _, l := range ls {
		if l == v {
			return true
		}
	}
	return false
}

// CreateAccount adds an account to the chart.
func (s *Store) CreateAccount(ctx context.Context, a *Principal, in AccountInput) (*Account, error) {
	var out *Account
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.createAccountTx(ctx, tx, a.Subject, in)
		return err
	})
	return out, err
}

func (s *Store) createAccountTx(ctx context.Context, tx dbq, actor string, in AccountInput) (*Account, error) {
	if err := in.check(ctx, tx); err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO ledger_accounts (code, name, type, tax_code, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		in.Code, in.Name, in.Type, in.TaxCode, actor, ts(s.now()))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("%w: account code %s already exists", ErrConflict, in.Code)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := s.audit(ctx, tx, actor, "ledger.account.create", in.Code, map[string]any{"id": id, "type": in.Type}); err != nil {
		return nil, err
	}
	return scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountCols+` FROM ledger_accounts WHERE id = ?`, id))
}

// ListAccounts lists the chart ordered by code.
func (s *Store) ListAccounts(ctx context.Context, a *Principal, includeInactive bool) ([]Account, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	q := `SELECT ` + accountCols + ` FROM ledger_accounts`
	if !includeInactive {
		q += ` WHERE (active = 1)`
	}
	rows, err := s.rdb.QueryContext(rctx, q+` ORDER BY code LIMIT 5000`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Account{}
	for rows.Next() {
		ac, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ac)
	}
	return out, rows.Err()
}

// UpdateAccount edits an account. An account that carries a posting keeps its code, type and tax code
// (the database refuses too); an account an automatic posting maps to cannot be deactivated.
func (s *Store) UpdateAccount(ctx context.Context, a *Principal, id int64, ifMatch *int64, in AccountPatch) (*Account, error) {
	var out *Account
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountCols+` FROM ledger_accounts WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != cur.Version {
			return ErrVersionMismatch
		}
		next := AccountInput{Code: cur.Code, Name: cur.Name, Type: cur.Type, TaxCode: cur.TaxCode}
		if in.Code != nil {
			next.Code = *in.Code
		}
		if in.Name != nil {
			next.Name = *in.Name
		}
		if in.Type != nil {
			next.Type = *in.Type
		}
		if in.TaxCode != nil {
			next.TaxCode = *in.TaxCode
		}
		if cur.Used && (next.Code != cur.Code || next.Type != cur.Type || next.TaxCode != cur.TaxCode) {
			return fmt.Errorf("%w: the account has postings, so its code, type and tax code are fixed; add a new account instead", ErrConflict)
		}
		if err := next.check(ctx, tx); err != nil {
			return err
		}
		active := cur.Active
		if in.Active != nil {
			active = *in.Active
		}
		if cur.Active && !active {
			var key string
			err := tx.QueryRowContext(ctx, `SELECT key FROM ledger_mappings WHERE account_id = ? LIMIT 1`, id).Scan(&key)
			if err == nil {
				return fmt.Errorf("%w: the account is mapped for %s postings; map another account first", ErrConflict, key)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			var one int
			err = tx.QueryRowContext(ctx, `SELECT 1 FROM bank_accounts WHERE ledger_account_id = ? AND active = 1 LIMIT 1`, id).Scan(&one)
			if err == nil {
				return fmt.Errorf("%w: an active bank account posts to this account", ErrConflict)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		// Only the columns that change are named: the immutability trigger fires on the columns an UPDATE
		// lists, not on the ones whose value differs.
		set, args := `name=?, active=?, version=version+1`, []any{next.Name, boolInt(active)}
		if next.Code != cur.Code {
			set, args = set+`, code=?`, append(args, next.Code)
		}
		if next.Type != cur.Type {
			set, args = set+`, type=?`, append(args, next.Type)
		}
		if next.TaxCode != cur.TaxCode {
			set, args = set+`, tax_code=?`, append(args, next.TaxCode)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ledger_accounts SET `+set+` WHERE id=?`, append(args, id)...); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("%w: account code %s already exists", ErrConflict, next.Code)
			}
			return err
		}
		if err := s.audit(ctx, tx, a.Subject, "ledger.account.update", cur.Code, map[string]any{"id": id, "active": active}); err != nil {
			return err
		}
		out, err = scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountCols+` FROM ledger_accounts WHERE id = ?`, id))
		return err
	})
	return out, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ChartTemplate is the default chart, shipped as data.
type ChartTemplate struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Accounts    []AccountInput    `json:"accounts"`
	Mappings    map[string]string `json:"mappings"`
}

// DefaultChartTemplate returns the embedded template.
func DefaultChartTemplate() ChartTemplate {
	var t ChartTemplate
	if err := json.Unmarshal(ledgerTemplateJSON, &t); err != nil {
		panic("ledger_template.json: " + err.Error())
	}
	return t
}

// ApplyChartTemplate adds the template's accounts that do not exist yet (by code) and, when withMappings
// is set and no mapping exists yet, maps them. It is safe to run twice.
func (s *Store) ApplyChartTemplate(ctx context.Context, a *Principal, withMappings bool) (created []Account, err error) {
	t := DefaultChartTemplate()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		created = nil
		for _, in := range t.Accounts {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM ledger_accounts WHERE code = ?`, in.Code).Scan(&one); err == nil {
				continue
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			ac, err := s.createAccountTx(ctx, tx, a.Subject, in)
			if err != nil {
				return err
			}
			created = append(created, *ac)
		}
		if withMappings {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger_mappings`).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return s.setMappingsTx(ctx, tx, a, t.Mappings)
			}
		}
		return s.audit(ctx, tx, a.Subject, "ledger.template.apply", t.Name, map[string]any{"created": len(created)})
	})
	return created, err
}

// ---- mappings -----------------------------------------------------------------------------

// Mappings is the account mapping in force: key to account, and whether automatic posting is on.
type Mappings struct {
	Accounts map[string]Account `json:"accounts"`
	Enabled  bool               `json:"enabled"`
	Missing  []string           `json:"missing"`
}

type mapIDs map[string]int64

func (s *Store) loadMappings(ctx context.Context, q dbq) (mapIDs, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, account_id FROM ledger_mappings`)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	m := mapIDs{}
	for rows.Next() {
		var k string
		var id int64
		if err := rows.Scan(&k, &id); err != nil {
			return nil, false, err
		}
		m[k] = id
	}
	return m, len(m) == len(mappingTypes), rows.Err()
}

// GetMappings returns the mapping and what is still missing for automatic posting.
func (s *Store) GetMappings(ctx context.Context, a *Principal) (*Mappings, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	ids, enabled, err := s.loadMappings(rctx, s.rdb)
	if err != nil {
		return nil, err
	}
	out := &Mappings{Accounts: map[string]Account{}, Enabled: enabled, Missing: []string{}}
	for k := range mappingTypes {
		id, ok := ids[k]
		if !ok {
			out.Missing = append(out.Missing, k)
			continue
		}
		ac, err := scanAccount(s.rdb.QueryRowContext(rctx, `SELECT `+accountCols+` FROM ledger_accounts WHERE id = ?`, id))
		if err != nil {
			return nil, err
		}
		out.Accounts[k] = *ac
	}
	sortStrings(out.Missing)
	return out, nil
}

// SetMappings sets the given keys (key to account code). Each account must exist, be active and be of the
// type the key needs: validation happens here, not when the first invoice fails to post.
func (s *Store) SetMappings(ctx context.Context, a *Principal, in map[string]string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error { return s.setMappingsTx(ctx, tx, a, in) })
}

func (s *Store) setMappingsTx(ctx context.Context, tx dbq, a *Principal, in map[string]string) error {
	var p problems
	if len(in) == 0 {
		p.add("mappings", "give at least one key")
	}
	ids := map[string]int64{}
	for k, code := range in {
		want, ok := mappingTypes[k]
		if !ok {
			p.add("mappings."+k, "unknown key; use one of %s", strings.Join(sortedKeys(mappingTypes), ", "))
			continue
		}
		var id int64
		var typ string
		var active int
		err := tx.QueryRowContext(ctx, `SELECT id, type, active FROM ledger_accounts WHERE code = ?`, code).Scan(&id, &typ, &active)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			p.add("mappings."+k, "account %q does not exist", code)
		case err != nil:
			return err
		case active != 1:
			p.add("mappings."+k, "account %q is inactive", code)
		case typ != want:
			p.add("mappings."+k, "account %q is %s; this mapping needs an %s account", code, typ, want)
		default:
			ids[k] = id
		}
	}
	if err := p.err(); err != nil {
		return err
	}
	_, wasOn, err := s.loadMappings(ctx, tx)
	if err != nil {
		return err
	}
	now := ts(s.now())
	for k, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_mappings (key, account_id, updated_by, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET account_id = excluded.account_id, updated_by = excluded.updated_by, updated_at = excluded.updated_at`, k, id, a.Subject, now); err != nil {
			return err
		}
	}
	mapped, on, err := s.loadMappings(ctx, tx)
	if err != nil {
		return err
	}
	if on && !wasOn {
		if err := s.postOpeningTx(ctx, tx, mapped, a.Subject); err != nil {
			return err
		}
	}
	return s.audit(ctx, tx, a.Subject, "ledger.mappings.set", "", in)
}

// ---- period close -------------------------------------------------------------------------

// closedThrough returns the date the ledger is closed through ("" when open).
func (s *Store) closedThrough(ctx context.Context, q dbq) (string, error) {
	var d sql.NullString
	err := q.QueryRowContext(ctx, `SELECT closed_through FROM ledger_closes ORDER BY id DESC LIMIT 1`).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return d.String, err
}

func (s *Store) checkLedgerOpen(ctx context.Context, q dbq, date string) error {
	through, err := s.closedThrough(ctx, q)
	if err != nil {
		return err
	}
	if through != "" && date <= through {
		return fmt.Errorf("%w: the ledger is closed through %s", ErrLocked, through)
	}
	return nil
}

// CloseEvent is one entry of the close history.
type CloseEvent struct {
	ID            int64     `json:"id"`
	Action        string    `json:"action"`
	ClosedThrough string    `json:"closed_through"`
	Reason        string    `json:"reason"`
	By            string    `json:"by"`
	At            time.Time `json:"at"`
}

// LedgerPeriods reports the current close and its history (newest first, at most 50).
func (s *Store) LedgerPeriods(ctx context.Context, a *Principal) (current string, history []CloseEvent, err error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	current, err = s.closedThrough(rctx, s.rdb)
	if err != nil {
		return "", nil, err
	}
	rows, err := s.rdb.QueryContext(rctx, `SELECT id, action, closed_through, reason, by, at FROM ledger_closes ORDER BY id DESC LIMIT 50`)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = rows.Close() }()
	history = []CloseEvent{}
	for rows.Next() {
		var c CloseEvent
		var at int64
		if err := rows.Scan(&c.ID, &c.Action, &c.ClosedThrough, &c.Reason, &c.By, &at); err != nil {
			return "", nil, err
		}
		c.At = fromUnix(at)
		history = append(history, c)
	}
	return current, history, rows.Err()
}

// ClosePeriod closes the ledger through a date: no entry dated on or before it can be posted. The date
// only moves forward (use ReopenPeriod to go back), and a close is refused while a draft journal is
// dated inside the period, so nothing is left behind a closed door.
func (s *Store) ClosePeriod(ctx context.Context, a *Principal, through string) error {
	if !validDate(through) {
		return invalid("closed_through", "use YYYY-MM-DD")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.closedThrough(ctx, tx)
		if err != nil {
			return err
		}
		if through <= cur {
			return fmt.Errorf("%w: the ledger is already closed through %s", ErrConflict, cur)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_entries WHERE status = 'draft' AND entry_date <= ?`, through).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: %d draft journal entries are dated in the period; post or delete them first", ErrConflict, n)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_closes (action, closed_through, by, at) VALUES ('close', ?, ?, ?)`, through, a.Subject, ts(s.now())); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "ledger.period.close", through, map[string]any{"previous": cur})
	})
}

// ReopenPeriod moves the close date back (or to "" to open everything). It needs a reason, and it is
// audited: a closed period is only ever changed through this one door.
func (s *Store) ReopenPeriod(ctx context.Context, a *Principal, through, reason string) error {
	if through != "" && !validDate(through) {
		return invalid("closed_through", "use YYYY-MM-DD, or empty to reopen everything")
	}
	if strings.TrimSpace(reason) == "" || !oneLineText(reason, 500) {
		return invalid("reason", "required, one line, at most 500 characters")
	}
	if !a.Can(PermLedgerAdmin) {
		return ErrForbidden
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := s.closedThrough(ctx, tx)
		if err != nil {
			return err
		}
		if through >= cur {
			return fmt.Errorf("%w: the ledger is closed through %q; give an earlier date", ErrConflict, cur)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_closes (action, closed_through, reason, by, at) VALUES ('reopen', ?, ?, ?, ?)`, through, reason, a.Subject, ts(s.now())); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "ledger.period.reopen", through, map[string]any{"previous": cur, "reason": reason})
	})
}

// ---- journal ------------------------------------------------------------------------------

// JournalLine is one line of an entry as read.
type JournalLine struct {
	Position    int    `json:"position"`
	AccountID   int64  `json:"account_id"`
	AccountCode string `json:"account_code"`
	AccountName string `json:"account_name"`
	DebitMinor  int64  `json:"debit_minor"`
	CreditMinor int64  `json:"credit_minor"`
	Memo        string `json:"memo"`
}

// JournalEntry is a journal entry as read.
type JournalEntry struct {
	ID         int64         `json:"id"`
	Status     string        `json:"status"`
	Source     string        `json:"source"`
	SourceRef  string        `json:"source_ref"`
	Number     *string       `json:"number"`
	EntryDate  string        `json:"entry_date"`
	Currency   string        `json:"currency"`
	Memo       string        `json:"memo"`
	TotalMinor int64         `json:"total_minor"`
	ReversalOf *int64        `json:"reversal_of"`
	ReversedBy *int64        `json:"reversed_by"`
	CreatedBy  string        `json:"created_by"`
	PostedBy   *string       `json:"posted_by"`
	PostedAt   *time.Time    `json:"posted_at"`
	CreatedAt  time.Time     `json:"created_at"`
	Lines      []JournalLine `json:"lines,omitempty"`
}

// JournalLineInput is a line of a manual journal; the account is named by its code.
type JournalLineInput struct {
	AccountCode string `json:"account_code"`
	DebitMinor  int64  `json:"debit_minor"`
	CreditMinor int64  `json:"credit_minor"`
	Memo        string `json:"memo"`
}

// JournalInput is the body of a manual journal.
type JournalInput struct {
	EntryDate string             `json:"entry_date"`
	Currency  string             `json:"currency"`
	Memo      string             `json:"memo"`
	Lines     []JournalLineInput `json:"lines"`
}

type lineSpec struct {
	AccountID int64
	Debit     int64
	Credit    int64
	Memo      string
}

type postSpec struct {
	Source     string
	Ref        string
	Date       string
	Currency   string
	Memo       string
	By         string
	Lines      []lineSpec
	ReversalOf int64
	// AllowInactive lets a reversal post to an account deactivated since the original.
	AllowInactive bool
}

// check validates the shape and the balance: at least two lines, each a debit or a credit within the
// per-line cap, sums equal in minor units.
func (sp *postSpec) check() error {
	var p problems
	if !validDate(sp.Date) {
		p.add("entry_date", "use YYYY-MM-DD")
	}
	if _, ok := CurrencyExponent(sp.Currency); !ok {
		p.add("currency", "unknown currency code")
	}
	if !oneLineText(sp.Memo, 500) {
		p.add("memo", "one line, at most 500 characters")
	}
	if len(sp.Lines) < 2 || len(sp.Lines) > maxJournalLines {
		p.add("lines", "give between 2 and %d lines", maxJournalLines)
		return p.err()
	}
	var debit, credit int64
	for i, l := range sp.Lines {
		f := fmt.Sprintf("lines[%d]", i)
		switch {
		case l.Debit < 0 || l.Credit < 0 || l.Debit > maxMinor || l.Credit > maxMinor:
			p.add(f, "amounts are between 0 and %d minor units", maxMinor)
		case (l.Debit > 0) == (l.Credit > 0):
			p.add(f, "a line is a debit or a credit, not both and not neither")
		}
		if !oneLineText(l.Memo, 200) {
			p.add(f+".memo", "one line, at most 200 characters")
		}
		var err error
		if debit, err = AddMinor(debit, l.Debit); err != nil {
			p.add("lines", "total too large")
			return p.err()
		}
		if credit, err = AddMinor(credit, l.Credit); err != nil {
			p.add("lines", "total too large")
			return p.err()
		}
	}
	if err := p.err(); err != nil {
		return err
	}
	if debit != credit {
		return invalid("lines", "the entry does not balance: debits %s, credits %s", FormatMinor(debit, sp.Currency), FormatMinor(credit, sp.Currency))
	}
	return nil
}

// insertDraftEntry writes an entry and its lines as a draft.
func (s *Store) insertDraftEntry(ctx context.Context, tx dbq, sp postSpec) (int64, error) {
	if err := sp.check(); err != nil {
		return 0, err
	}
	for _, l := range sp.Lines {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT active FROM ledger_accounts WHERE id = ?`, l.AccountID).Scan(&active); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, invalid("lines", "no such account")
			}
			return 0, err
		}
		if active != 1 && !sp.AllowInactive {
			return 0, invalid("lines", "an account is inactive; reactivate it or use another")
		}
	}
	var rev any
	if sp.ReversalOf > 0 {
		rev = sp.ReversalOf
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO journal_entries (status, source, source_ref, entry_date, currency, memo, reversal_of, created_by, created_at) VALUES ('draft', ?, ?, ?, ?, ?, ?, ?, ?)`,
		sp.Source, sp.Ref, sp.Date, sp.Currency, sp.Memo, rev, sp.By, ts(s.now()))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("%w: this event is already posted", ErrConflict)
		}
		return 0, err
	}
	id, _ := res.LastInsertId()
	for i, l := range sp.Lines {
		if _, err := tx.ExecContext(ctx, `INSERT INTO journal_lines (entry_id, position, account_id, debit_minor, credit_minor, memo) VALUES (?, ?, ?, ?, ?, ?)`,
			id, i+1, l.AccountID, l.Debit, l.Credit, l.Memo); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// postDraftEntry numbers a draft (gap-free, series JE, in this transaction) and posts it. The database
// checks the balance and the period once more. It emits journal.posted.v1 in the same transaction.
func (s *Store) postDraftEntry(ctx context.Context, tx dbq, id int64, by string) error {
	var date, cur, source string
	if err := tx.QueryRowContext(ctx, `SELECT entry_date, currency, source FROM journal_entries WHERE id = ? AND status = 'draft'`, id).Scan(&date, &cur, &source); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: the entry is not a draft", ErrConflict)
		}
		return err
	}
	if err := s.checkLedgerOpen(ctx, tx, date); err != nil {
		return err
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO series (code, next_number) VALUES ('JE', 2) ON CONFLICT (code) DO UPDATE SET next_number = next_number + 1 RETURNING next_number - 1`).Scan(&n); err != nil {
		return err
	}
	text := fmt.Sprintf("JE-%05d", n)
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE journal_entries SET status='posted', number=?, number_text=?, posted_by=?, posted_at=? WHERE id=?`, n, text, by, ts(now), id); err != nil {
		if strings.Contains(err.Error(), "period is closed") {
			return fmt.Errorf("%w: the ledger is closed", ErrLocked)
		}
		return err
	}
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(debit_minor), 0) FROM journal_lines WHERE entry_id = ?`, id).Scan(&total); err != nil {
		return err
	}
	return s.emit(ctx, tx, EventJournalPosted, journalPostedData{EntryID: id, Number: text, Source: source, Currency: cur, TotalMinor: total, EntryDate: date, PostedAt: now.UTC()})
}

type journalPostedData struct {
	EntryID    int64     `json:"entry_id"`
	Number     string    `json:"number"`
	Source     string    `json:"source"`
	Currency   string    `json:"currency"`
	TotalMinor int64     `json:"total_minor"`
	EntryDate  string    `json:"entry_date"`
	PostedAt   time.Time `json:"posted_at"`
}

// postEntryTx writes and posts an entry in the caller's transaction.
func (s *Store) postEntryTx(ctx context.Context, tx dbq, sp postSpec) (int64, error) {
	if err := s.checkLedgerOpen(ctx, tx, sp.Date); err != nil {
		return 0, err
	}
	id, err := s.insertDraftEntry(ctx, tx, sp)
	if err != nil {
		return 0, err
	}
	return id, s.postDraftEntry(ctx, tx, id, sp.By)
}

const entryCols = `id, status, source, source_ref, number_text, entry_date, currency, memo, reversal_of, reversed_by, created_by, posted_by, posted_at, created_at,
 (SELECT COALESCE(SUM(debit_minor), 0) FROM journal_lines WHERE entry_id = journal_entries.id)`

func scanEntry(sc scanner) (*JournalEntry, error) {
	var e JournalEntry
	var num, postedBy sql.NullString
	var rev, revBy, postedAt sql.NullInt64
	var created int64
	if err := sc.Scan(&e.ID, &e.Status, &e.Source, &e.SourceRef, &num, &e.EntryDate, &e.Currency, &e.Memo, &rev, &revBy, &e.CreatedBy, &postedBy, &postedAt, &created, &e.TotalMinor); err != nil {
		return nil, err
	}
	e.Number, e.PostedBy, e.PostedAt, e.CreatedAt = nullStr(num), nullStr(postedBy), nullTime(postedAt), fromUnix(created)
	if rev.Valid {
		e.ReversalOf = &rev.Int64
	}
	if revBy.Valid {
		e.ReversedBy = &revBy.Int64
	}
	return &e, nil
}

func (s *Store) loadEntry(ctx context.Context, q dbq, id int64) (*JournalEntry, error) {
	e, err := scanEntry(q.QueryRowContext(ctx, `SELECT `+entryCols+` FROM journal_entries WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT l.position, l.account_id, a.code, a.name, l.debit_minor, l.credit_minor, l.memo
		FROM journal_lines l JOIN ledger_accounts a ON a.id = l.account_id WHERE l.entry_id = ? ORDER BY l.position LIMIT ?`, id, maxJournalLines)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	e.Lines = []JournalLine{}
	for rows.Next() {
		var l JournalLine
		if err := rows.Scan(&l.Position, &l.AccountID, &l.AccountCode, &l.AccountName, &l.DebitMinor, &l.CreditMinor, &l.Memo); err != nil {
			return nil, err
		}
		e.Lines = append(e.Lines, l)
	}
	return e, rows.Err()
}

// GetJournal returns an entry with its lines.
func (s *Store) GetJournal(ctx context.Context, a *Principal, id int64) (*JournalEntry, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	return s.loadEntry(rctx, s.rdb, id)
}

// JournalFilter narrows a journal list.
type JournalFilter struct{ Status, Source, From, To string }

// ListJournals lists entries, newest first, after the cursor id (exclusive).
func (s *Store) ListJournals(ctx context.Context, a *Principal, f JournalFilter, before int64, limit int) ([]JournalEntry, int64, error) {
	limit = clampLimit(limit)
	var w where
	if f.Status != "" {
		if f.Status != "draft" && f.Status != "posted" {
			return nil, 0, invalid("status", "draft or posted")
		}
		w.and("(status = ?)", f.Status)
	}
	if f.Source != "" {
		w.and("(source = ?)", f.Source)
	}
	if f.From != "" {
		if !validDate(f.From) {
			return nil, 0, invalid("from", "use YYYY-MM-DD")
		}
		w.and("(entry_date >= ?)", f.From)
	}
	if f.To != "" {
		if !validDate(f.To) {
			return nil, 0, invalid("to", "use YYYY-MM-DD")
		}
		w.and("(entry_date <= ?)", f.To)
	}
	if before > 0 {
		w.and("(id < ?)", before)
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+entryCols+` FROM journal_entries WHERE `+w.sql()+` ORDER BY id DESC LIMIT ?`, append(w.args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []JournalEntry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *e)
	}
	var next int64
	if len(out) == limit {
		next = out[len(out)-1].ID
	}
	return out, next, rows.Err()
}

func (s *Store) resolveLines(ctx context.Context, q dbq, in []JournalLineInput) ([]lineSpec, error) {
	var p problems
	out := make([]lineSpec, 0, len(in))
	for i, l := range in {
		var id int64
		err := q.QueryRowContext(ctx, `SELECT id FROM ledger_accounts WHERE code = ?`, l.AccountCode).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			p.add(fmt.Sprintf("lines[%d].account_code", i), "no such account")
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, lineSpec{AccountID: id, Debit: l.DebitMinor, Credit: l.CreditMinor, Memo: l.Memo})
	}
	return out, p.err()
}

// CreateJournal drafts a manual journal. It posts only when someone else approves it (PostJournal).
func (s *Store) CreateJournal(ctx context.Context, a *Principal, in JournalInput, idemKey string) (*JournalEntry, bool, error) {
	return s.createJournalAs(ctx, a, in, idemKey, "manual", "")
}

func (s *Store) createJournalAs(ctx context.Context, a *Principal, in JournalInput, idemKey, source, ref string) (*JournalEntry, bool, error) {
	var out *JournalEntry
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, "journal.create", in, func() (*JournalEntry, error) {
			lines, err := s.resolveLines(ctx, tx, in.Lines)
			if err != nil {
				return nil, err
			}
			id, err := s.insertDraftEntry(ctx, tx, postSpec{Source: source, Ref: ref, Date: in.EntryDate, Currency: in.Currency, Memo: in.Memo, By: a.Subject, Lines: lines})
			if err != nil {
				return nil, err
			}
			if err := s.audit(ctx, tx, a.Subject, "journal.draft", fmt.Sprint(id), map[string]any{"source": source, "lines": len(lines)}); err != nil {
				return nil, err
			}
			return s.loadEntry(ctx, tx, id)
		})
		return err
	})
	return out, replayed, err
}

// DeleteJournalDraft removes a draft.
func (s *Store) DeleteJournalDraft(ctx context.Context, a *Principal, id int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM journal_entries WHERE id = ? AND status = 'draft'`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM journal_entries WHERE id = ?`, id).Scan(&one); err != nil {
				return ErrNotFound
			}
			return fmt.Errorf("%w: posted entries are corrected with a reversal, never deleted", ErrConflict)
		}
		return s.audit(ctx, tx, a.Subject, "journal.draft_delete", fmt.Sprint(id), map[string]any{})
	})
}

// PostJournal approves and posts a draft. The person who drafted it cannot (separation of duties,
// authorizeApproval), and the ledger must be open for its date.
func (s *Store) PostJournal(ctx context.Context, a *Principal, id int64) (*JournalEntry, error) {
	var out *JournalEntry
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		e, err := s.loadEntry(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.authorizeApproval(ctx, a, ApprovalRequest{Kind: "journal", Perm: PermLedgerApprove}, e.CreatedBy); err != nil {
			return err
		}
		if e.Status != "draft" {
			return fmt.Errorf("%w: the entry is already posted", ErrConflict)
		}
		if err := s.postDraftEntry(ctx, tx, id, a.Subject); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, a.Subject, "journal.post", fmt.Sprint(id), map[string]any{"drafted_by": e.CreatedBy}); err != nil {
			return err
		}
		out, err = s.loadEntry(ctx, tx, id)
		return err
	})
	return out, err
}

// ReverseJournal corrects a posted entry by posting its mirror image on the given date (which must be in
// an open period), linked to the original. An entry is reversed once; a reversal cannot be reversed
// (post a new entry instead). The reverser is not the person who created the original.
func (s *Store) ReverseJournal(ctx context.Context, a *Principal, id int64, date, reason string) (*JournalEntry, error) {
	if !a.Can(PermLedgerApprove) {
		return nil, ErrForbidden
	}
	return s.reverseEntry(ctx, a, id, date, reason)
}

func (s *Store) reverseEntry(ctx context.Context, a *Principal, id int64, date, reason string) (*JournalEntry, error) {
	if !validDate(date) {
		return nil, invalid("date", "use YYYY-MM-DD")
	}
	if strings.TrimSpace(reason) == "" || !oneLineText(reason, 300) {
		return nil, invalid("reason", "required, one line, at most 300 characters")
	}
	var out *JournalEntry
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		orig, err := s.loadEntry(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := s.authorizeApproval(ctx, a, ApprovalRequest{Kind: "journal", Perm: PermLedgerApprove}, orig.CreatedBy); err != nil {
			return err
		}
		rid, err := s.reverseTx(ctx, tx, orig, date, "Reversal of "+deref(orig.Number)+": "+reason, a.Subject)
		if err != nil {
			return err
		}
		if err := s.audit(ctx, tx, a.Subject, "journal.reverse", fmt.Sprint(id), map[string]any{"reversal_id": rid, "reason": reason}); err != nil {
			return err
		}
		out, err = s.loadEntry(ctx, tx, rid)
		return err
	})
	return out, err
}

// reverseTx posts the mirror image of a posted entry and links the two.
func (s *Store) reverseTx(ctx context.Context, tx dbq, orig *JournalEntry, date, memo, by string) (int64, error) {
	switch {
	case orig.Status != "posted":
		return 0, fmt.Errorf("%w: only a posted entry is reversed; delete a draft", ErrConflict)
	case orig.ReversedBy != nil:
		return 0, fmt.Errorf("%w: the entry was already reversed", ErrConflict)
	case orig.ReversalOf != nil:
		return 0, fmt.Errorf("%w: a reversal cannot be reversed; post a new entry", ErrConflict)
	case date < orig.EntryDate:
		return 0, invalid("date", "a reversal cannot be dated before the entry it reverses")
	}
	lines := make([]lineSpec, len(orig.Lines))
	for i, l := range orig.Lines {
		lines[i] = lineSpec{AccountID: l.AccountID, Debit: l.CreditMinor, Credit: l.DebitMinor, Memo: l.Memo}
	}
	if len(memo) > 500 {
		memo = memo[:500]
	}
	rid, err := s.postEntryTx(ctx, tx, postSpec{Source: "reversal", Date: date, Currency: orig.Currency, Memo: strings.ReplaceAll(memo, "\n", " "), By: by, Lines: lines, ReversalOf: orig.ID, AllowInactive: true})
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE journal_entries SET reversed_by = ? WHERE id = ?`, rid, orig.ID); err != nil {
		return 0, err
	}
	return rid, nil
}
