package finance

// Bank accounts, statement import and reconciliation (E15c, specs/035-accounting). The module never talks
// to a bank: a person uploads a statement file. Matching only suggests; a person confirms, and confirming
// records a payment through the same path as any other payment (recordPaymentTx), so the invoice, the
// events and the journal all follow the one rule. Nothing is posted without a confirmation.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const EventBankStatementImported = "bank.statement.imported.v1"

const minBankNumber, maxBankNumber = 5, 40

var bankNumberRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 -]{3,38}[A-Za-z0-9]$`)

// BankAccount is a bank account as the API shows it: the number is never returned, only its mask.
type BankAccount struct {
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	Currency          string    `json:"currency"`
	LedgerAccountID   int64     `json:"ledger_account_id"`
	LedgerAccountCode string    `json:"ledger_account_code"`
	Masked            string    `json:"masked_number"`
	Active            bool      `json:"active"`
	Version           int64     `json:"version"`
	CreatedBy         string    `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
}

// BankAccountInput creates a bank account. Number is sealed and never shown again.
type BankAccountInput struct {
	Name              string `json:"name"`
	Currency          string `json:"currency"`
	Number            string `json:"number"`
	LedgerAccountCode string `json:"ledger_account_code"`
}

const bankAcctCols = `b.id, b.name, b.currency, b.ledger_account_id, a.code, b.last4, b.active, b.version, b.created_by, b.created_at`

const bankAcctFrom = ` FROM bank_accounts b JOIN ledger_accounts a ON a.id = b.ledger_account_id`

func scanBankAccount(sc scanner) (*BankAccount, error) {
	var b BankAccount
	var last4 string
	var act int
	var c int64
	if err := sc.Scan(&b.ID, &b.Name, &b.Currency, &b.LedgerAccountID, &b.LedgerAccountCode, &last4, &act, &b.Version, &b.CreatedBy, &c); err != nil {
		return nil, err
	}
	b.Masked, b.Active, b.CreatedAt = "****"+last4, act == 1, fromUnix(c)
	return &b, nil
}

func normalizeNumber(s string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(s))
}

func sealAAD(id int64) string { return fmt.Sprintf("finance.bank_accounts.sealed_number:%d", id) }

// CreateBankAccount adds a bank account tied to an asset account of the chart.
func (s *Store) CreateBankAccount(ctx context.Context, a *Principal, in BankAccountInput) (*BankAccount, error) {
	var p problems
	if strings.TrimSpace(in.Name) == "" || !oneLineText(in.Name, maxName) {
		p.add("name", "required, one line, at most %d characters", maxName)
	}
	if _, ok := CurrencyExponent(in.Currency); !ok {
		p.add("currency", "unknown currency code")
	}
	// The length is checked after the spaces and dashes are removed: "1   2" is two characters.
	num := normalizeNumber(in.Number)
	if !bankNumberRE.MatchString(in.Number) || len(num) < minBankNumber || len(num) > maxBankNumber {
		p.add("number", "%d to %d letters or digits, optionally separated by spaces or dashes", minBankNumber, maxBankNumber)
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	if s.sealer == nil {
		return nil, fmt.Errorf("%w: CAN_FINANCE_SEAL_KEY is not set, so bank account numbers cannot be sealed", ErrNotConfigured)
	}
	var out *BankAccount
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var lid int64
		var typ string
		var active int
		err := tx.QueryRowContext(ctx, `SELECT id, type, active FROM ledger_accounts WHERE code = ?`, in.LedgerAccountCode).Scan(&lid, &typ, &active)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return invalid("ledger_account_code", "no such account")
		case err != nil:
			return err
		case typ != AcAsset || active != 1:
			return invalid("ledger_account_code", "must be an active asset account")
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO bank_accounts (name, currency, ledger_account_id, sealed_number, last4, created_by, created_at) VALUES (?, ?, ?, 'pending', ?, ?, ?)`,
			in.Name, in.Currency, lid, num[len(num)-4:], a.Subject, ts(s.now()))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		sealed, err := s.sealer.Seal(num, sealAAD(id))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE bank_accounts SET sealed_number = ? WHERE id = ?`, sealed, id); err != nil {
			return err
		}
		// The audit entry names the account and its mask, never the number.
		if err := s.audit(ctx, tx, a.Subject, "bank.account.create", fmt.Sprint(id), map[string]any{"currency": in.Currency, "masked": "****" + num[len(num)-4:]}); err != nil {
			return err
		}
		out, err = scanBankAccount(tx.QueryRowContext(ctx, `SELECT `+bankAcctCols+bankAcctFrom+` WHERE b.id = ?`, id))
		return err
	})
	return out, err
}

// ListBankAccounts lists the bank accounts (masked).
func (s *Store) ListBankAccounts(ctx context.Context, a *Principal) ([]BankAccount, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+bankAcctCols+bankAcctFrom+` ORDER BY b.id LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []BankAccount{}
	for rows.Next() {
		b, err := scanBankAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// GetBankAccount returns one bank account (masked).
func (s *Store) GetBankAccount(ctx context.Context, a *Principal, id int64) (*BankAccount, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	b, err := scanBankAccount(s.rdb.QueryRowContext(rctx, `SELECT `+bankAcctCols+bankAcctFrom+` WHERE b.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// ---- statement import ---------------------------------------------------------------------

// StatementResult is what an import did.
type StatementResult struct {
	StatementID    int64  `json:"statement_id"`
	BankAccountID  int64  `json:"bank_account_id"`
	Format         string `json:"format"`
	TransactionCnt int    `json:"transaction_count"`
	NewCount       int    `json:"new_count"`
	DuplicateCount int    `json:"duplicate_count"`
	// AlreadyImported is true when this exact file was imported before: nothing changed.
	AlreadyImported bool `json:"already_imported"`
	// Skipped lists the rows counted as duplicates (the first maxSkippedListed), so nothing disappears silently.
	Skipped []SkippedRow `json:"skipped,omitempty"`
}

// SkippedRow is a statement row that was not imported.
type SkippedRow struct {
	Row         int    `json:"row"` // 1 is the first transaction of the file
	BookedOn    string `json:"booked_on"`
	AmountMinor int64  `json:"amount_minor"`
	// Reason is already_imported, or id_reused when the bank's id matches a transaction with other details.
	Reason string `json:"reason"`
	// Forceable rows have no bank id: they can be imported with force_duplicates=true.
	Forceable bool `json:"forceable"`
}

const maxSkippedListed = 200

// ImportStatement reads a statement file into bank transactions, once. The same file again, and any
// transaction already known by its bank id (or content hash), is counted as a duplicate and skipped.
func (s *Store) ImportStatement(ctx context.Context, a *Principal, accountID int64, format string, body io.Reader, csvCfg CSVConfig, force bool, idemKey string) (*StatementResult, bool, error) {
	data, err := readStatement(body)
	if err != nil {
		var pe *errParse
		if errors.As(err, &pe) {
			return nil, false, invalid("file", "%s", pe.msg)
		}
		return nil, false, err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	var out *StatementResult
	var replayed bool
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, fmt.Sprintf("bank.import.%d", accountID), map[string]any{"format": format, "sha256": hash, "force": force}, func() (*StatementResult, error) {
			return s.importStatementTx(ctx, tx, a, accountID, format, data, hash, csvCfg, force)
		})
		return err
	})
	return out, replayed, err
}

func (s *Store) importStatementTx(ctx context.Context, tx dbq, a *Principal, accountID int64, format string, data []byte, hash string, csvCfg CSVConfig, force bool) (*StatementResult, error) {
	var cur, sealed string
	var active int
	err := tx.QueryRowContext(ctx, `SELECT currency, sealed_number, active FROM bank_accounts WHERE id = ?`, accountID).Scan(&cur, &sealed, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if active != 1 {
		return nil, fmt.Errorf("%w: the bank account is inactive", ErrConflict)
	}
	var prev StatementResult
	err = tx.QueryRowContext(ctx, `SELECT id, bank_account_id, format, txn_count, new_count, duplicate_count FROM bank_statements WHERE bank_account_id = ? AND sha256 = ?`, accountID, hash).
		Scan(&prev.StatementID, &prev.BankAccountID, &prev.Format, &prev.TransactionCnt, &prev.NewCount, &prev.DuplicateCount)
	if err == nil {
		prev.AlreadyImported = true
		return &prev, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var ps *ParsedStatement
	switch format {
	case "csv":
		ps, err = ParseCSV(bytes.NewReader(data), csvCfg, cur)
	case "ofx":
		ps, err = ParseOFX(bytes.NewReader(data), cur)
	case "camt053":
		ps, err = ParseCAMT053(bytes.NewReader(data), cur)
	default:
		return nil, invalid("format", "csv, ofx or camt053")
	}
	if err != nil {
		var pe *errParse
		if errors.As(err, &pe) {
			return nil, invalid("file", "%s", pe.msg)
		}
		return nil, err
	}
	// A statement that names its account must name this one. The message says nothing about either number.
	if ps.AccountID != "" {
		if s.sealer == nil {
			return nil, fmt.Errorf("%w: CAN_FINANCE_SEAL_KEY is not set", ErrNotConfigured)
		}
		num, err := s.sealer.Open(sealed, sealAAD(accountID))
		if err != nil {
			return nil, err
		}
		if normalizeNumber(ps.AccountID) != num {
			return nil, invalid("file", "the statement is for a different bank account")
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO bank_statements (bank_account_id, format, sha256, size_bytes, txn_count, new_count, duplicate_count, imported_by, imported_at) VALUES (?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		accountID, format, hash, len(data), len(ps.Txns), a.Subject, ts(s.now()))
	if err != nil {
		return nil, err
	}
	sid, _ := res.LastInsertId()
	out := &StatementResult{StatementID: sid, BankAccountID: accountID, Format: format, TransactionCnt: len(ps.Txns)}
	insert := func(t ParsedTxn, key string) (bool, error) {
		r, err := tx.ExecContext(ctx, `INSERT INTO bank_transactions (bank_account_id, statement_id, txn_key, booked_on, amount_minor, currency, reference, counterparty, description, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (bank_account_id, txn_key) DO NOTHING`,
			accountID, sid, key, t.Date, t.AmountMinor, t.Currency, t.Reference, t.Counterparty, t.Description, ts(s.now()))
		if err != nil {
			return false, err
		}
		n, _ := r.RowsAffected()
		return n == 1, nil
	}
	for i, t := range ps.Txns {
		added, err := insert(t, t.Key)
		if err != nil {
			return nil, err
		}
		// Without a bank id, two files can hold equal rows that are two real payments. They are skipped (and
		// listed) unless the caller says to take them; the key then carries the file's hash.
		derived := strings.HasPrefix(t.Key, derivedKeyPrefix)
		if !added && force && derived {
			if added, err = insert(t, t.Key+":"+hash[:16]); err != nil {
				return nil, err
			}
		}
		if added {
			out.NewCount++
			continue
		}
		out.DuplicateCount++
		if len(out.Skipped) >= maxSkippedListed {
			continue
		}
		sk := SkippedRow{Row: i + 1, BookedOn: t.Date, AmountMinor: t.AmountMinor, Reason: "already_imported", Forceable: derived}
		var same int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM bank_transactions WHERE bank_account_id = ? AND txn_key = ? AND booked_on = ? AND amount_minor = ? AND reference = ? AND counterparty = ? AND description = ?`,
			accountID, t.Key, t.Date, t.AmountMinor, t.Reference, t.Counterparty, t.Description).Scan(&same); err != nil {
			return nil, err
		}
		if same == 0 && !derived {
			sk.Reason = "id_reused"
		}
		out.Skipped = append(out.Skipped, sk)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bank_statements SET new_count = ?, duplicate_count = ? WHERE id = ?`, out.NewCount, out.DuplicateCount, sid); err != nil {
		return nil, err
	}
	if err := s.audit(ctx, tx, a.Subject, "bank.statement.import", fmt.Sprint(sid), map[string]any{"bank_account_id": accountID, "format": format, "transactions": len(ps.Txns), "new": out.NewCount, "duplicates": out.DuplicateCount}); err != nil {
		return nil, err
	}
	err = s.emit(ctx, tx, EventBankStatementImported, bankImportedData{StatementID: sid, BankAccountID: accountID, Format: format,
		TransactionCount: len(ps.Txns), NewCount: out.NewCount, DuplicateCount: out.DuplicateCount, ImportedAt: s.now().UTC()})
	return out, err
}

type bankImportedData struct {
	StatementID      int64     `json:"statement_id"`
	BankAccountID    int64     `json:"bank_account_id"`
	Format           string    `json:"format"`
	TransactionCount int       `json:"transaction_count"`
	NewCount         int       `json:"new_count"`
	DuplicateCount   int       `json:"duplicate_count"`
	ImportedAt       time.Time `json:"imported_at"`
}

// ---- transactions -------------------------------------------------------------------------

// BankTxn is a bank transaction as the API shows it. Counterparty and description are the bank's text:
// personal data, so reading them is audited (and fails closed if the audit cannot be written).
type BankTxn struct {
	ID            int64      `json:"id"`
	BankAccountID int64      `json:"bank_account_id"`
	StatementID   int64      `json:"statement_id"`
	BookedOn      string     `json:"booked_on"`
	AmountMinor   int64      `json:"amount_minor"`
	Currency      string     `json:"currency"`
	Reference     string     `json:"reference"`
	Counterparty  string     `json:"counterparty"`
	Description   string     `json:"description"`
	Status        string     `json:"status"`
	PaymentID     *int64     `json:"matched_payment_id"`
	DecidedBy     *string    `json:"decided_by"`
	DecidedAt     *time.Time `json:"decided_at"`
	IgnoreReason  string     `json:"ignore_reason"`
}

const txnCols = `id, bank_account_id, statement_id, booked_on, amount_minor, currency, reference, counterparty, description, status, matched_payment_id, decided_by, decided_at, ignore_reason`

func scanTxn(sc scanner) (*BankTxn, error) {
	var t BankTxn
	var pid, dat sql.NullInt64
	var dby sql.NullString
	if err := sc.Scan(&t.ID, &t.BankAccountID, &t.StatementID, &t.BookedOn, &t.AmountMinor, &t.Currency, &t.Reference, &t.Counterparty, &t.Description, &t.Status, &pid, &dby, &dat, &t.IgnoreReason); err != nil {
		return nil, err
	}
	if pid.Valid {
		t.PaymentID = &pid.Int64
	}
	t.DecidedBy, t.DecidedAt = nullStr(dby), nullTime(dat)
	return &t, nil
}

// TxnFilter narrows the bank transaction list.
type TxnFilter struct {
	AccountID int64
	Status    string
	From, To  string
}

// ListBankTransactions lists transactions, oldest booking first, after the cursor id. The read is audited
// before anything is returned: if the audit cannot be written, nothing is.
func (s *Store) ListBankTransactions(ctx context.Context, a *Principal, f TxnFilter, after int64, limit int) ([]BankTxn, int64, error) {
	limit = clampLimit(limit)
	var w where
	if f.AccountID > 0 {
		w.and("(bank_account_id = ?)", f.AccountID)
	}
	if f.Status != "" {
		if f.Status != "unmatched" && f.Status != "matched" && f.Status != "ignored" {
			return nil, 0, invalid("status", "unmatched, matched or ignored")
		}
		w.and("(status = ?)", f.Status)
	}
	if f.From != "" {
		if !validDate(f.From) {
			return nil, 0, invalid("from", "use YYYY-MM-DD")
		}
		w.and("(booked_on >= ?)", f.From)
	}
	if f.To != "" {
		if !validDate(f.To) {
			return nil, 0, invalid("to", "use YYYY-MM-DD")
		}
		w.and("(booked_on <= ?)", f.To)
	}
	if after > 0 {
		w.and("(id > ?)", after)
	}
	if err := s.auditRead(ctx, a, "bank.transactions.read", fmt.Sprint(f.AccountID), map[string]any{"status": f.Status, "from": f.From, "to": f.To}); err != nil {
		return nil, 0, err
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+txnCols+` FROM bank_transactions WHERE `+w.sql()+` ORDER BY id LIMIT ?`, append(w.args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []BankTxn{}
	for rows.Next() {
		t, err := scanTxn(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *t)
	}
	var next int64
	if len(out) == limit {
		next = out[len(out)-1].ID
	}
	return out, next, rows.Err()
}

func (s *Store) loadTxn(ctx context.Context, q dbq, id int64) (*BankTxn, error) {
	t, err := scanTxn(q.QueryRowContext(ctx, `SELECT `+txnCols+` FROM bank_transactions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// Suggestion is a possible match. It is advice: nothing is recorded until someone confirms.
type Suggestion struct {
	InvoiceID    int64  `json:"invoice_id"`
	Number       string `json:"number"`
	BalanceMinor int64  `json:"balance_minor"`
	Currency     string `json:"currency"`
	Score        int    `json:"score"`
	Reason       string `json:"reason"` // amount_and_reference, amount or reference
}

// SuggestMatches proposes open invoices for an incoming transaction: the same currency and either the
// exact balance, or the invoice number named in the reference (a partial payment). Suggestions only;
// at most five, best first.
func (s *Store) SuggestMatches(ctx context.Context, a *Principal, txnID int64) ([]Suggestion, error) {
	m, err := s.SuggestMatchesFor(ctx, a, []int64{txnID})
	if err != nil {
		return nil, err
	}
	return m[txnID], nil
}

// maxSuggestBatch bounds one SuggestMatchesFor call.
const maxSuggestBatch = 100

// SuggestMatchesFor suggests for several transactions with one audit entry (a page of the queue is one
// read). A transaction that does not exist is ErrNotFound.
func (s *Store) SuggestMatchesFor(ctx context.Context, a *Principal, txnIDs []int64) (map[int64][]Suggestion, error) {
	if len(txnIDs) == 0 || len(txnIDs) > maxSuggestBatch {
		return nil, invalid("transactions", "give between 1 and %d", maxSuggestBatch)
	}
	if err := s.auditRead(ctx, a, "bank.suggestions.read", fmt.Sprint(txnIDs[0]), map[string]any{"transactions": len(txnIDs)}); err != nil {
		return nil, err
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	out := map[int64][]Suggestion{}
	for _, id := range txnIDs {
		t, err := s.loadTxn(rctx, s.rdb, id)
		if err != nil {
			return nil, err
		}
		if out[id], err = s.suggestFor(rctx, a, t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) suggestFor(ctx context.Context, a *Principal, t *BankTxn) ([]Suggestion, error) {
	out := []Suggestion{}
	if t.Status != "unmatched" || t.AmountMinor <= 0 {
		return out, nil
	}
	var w where
	scopeWhere(a, &w, "party_id", "project")
	w.and("(kind = 'invoice' AND status IN ('approved', 'sent', 'partially_paid') AND currency = ? AND total_minor - paid_minor - credited_minor > 0)", t.Currency)
	w.and("(total_minor - paid_minor - credited_minor = ? OR instr(upper(?), number_text) > 0)", t.AmountMinor, t.Reference)
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, number_text, total_minor - paid_minor - credited_minor FROM invoices WHERE `+w.sql()+` LIMIT 50`, w.args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ref := strings.ToUpper(t.Reference)
	for rows.Next() {
		var sg Suggestion
		if err := rows.Scan(&sg.InvoiceID, &sg.Number, &sg.BalanceMinor); err != nil {
			return nil, err
		}
		sg.Currency = t.Currency
		byAmt := sg.BalanceMinor == t.AmountMinor
		byRef := sg.Number != "" && strings.Contains(ref, sg.Number)
		switch {
		case byAmt && byRef:
			sg.Score, sg.Reason = 100, "amount_and_reference"
		case byRef && t.AmountMinor < sg.BalanceMinor:
			sg.Score, sg.Reason = 60, "reference"
		case byAmt:
			sg.Score, sg.Reason = 50, "amount"
		default:
			continue // names an invoice but pays more than it owes: not a suggestion
		}
		out = append(out, sg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].InvoiceID < out[j].InvoiceID
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out, nil
}

// ConfirmMatch records the payment for a bank transaction and marks it matched, in one transaction. It
// goes through recordPaymentTx, so every payment rule applies (currency, no overpayment, the invoice's
// status) and the payment posts to the ledger of the bank account the money arrived in.
func (s *Store) ConfirmMatch(ctx context.Context, a *Principal, txnID, invoiceID int64, idemKey string) (*BankTxn, bool, error) {
	if !a.Can(PermWrite) {
		return nil, false, ErrForbidden // a match records a payment: it needs finance.write as well
	}
	var out *BankTxn
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, fmt.Sprintf("bank.confirm.%d", txnID), map[string]any{"invoice_id": invoiceID}, func() (*BankTxn, error) {
			t, err := s.loadTxn(ctx, tx, txnID)
			if err != nil {
				return nil, err
			}
			switch {
			case t.Status != "unmatched":
				return nil, fmt.Errorf("%w: the transaction is already %s", ErrConflict, t.Status)
			case t.AmountMinor <= 0:
				return nil, fmt.Errorf("%w: only money received can be matched to an invoice; ignore the transaction instead", ErrConflict)
			}
			var ledgerAcct int64
			if err := tx.QueryRowContext(ctx, `SELECT ledger_account_id FROM bank_accounts WHERE id = ?`, t.BankAccountID).Scan(&ledgerAcct); err != nil {
				return nil, err
			}
			pay, _, err := s.recordPaymentTx(ctx, tx, a, a.Subject, invoiceID, PaymentInput{
				AmountMinor: t.AmountMinor, Currency: t.Currency, Method: "bank", Reference: t.Reference, ReceivedOn: t.BookedOn,
				ExternalID: fmt.Sprintf("bank:%d:%d", t.BankAccountID, t.ID), cashAccount: ledgerAcct,
			}, "import")
			if err != nil {
				return nil, err
			}
			now := ts(s.now())
			if _, err := tx.ExecContext(ctx, `UPDATE bank_transactions SET status = 'matched', matched_payment_id = ?, decided_by = ?, decided_at = ? WHERE id = ?`, pay.ID, a.Subject, now, txnID); err != nil {
				return nil, err
			}
			if err := s.audit(ctx, tx, a.Subject, "bank.match.confirm", fmt.Sprint(txnID), map[string]any{"invoice_id": invoiceID, "payment_id": pay.ID, "amount_minor": t.AmountMinor, "currency": t.Currency}); err != nil {
				return nil, err
			}
			return s.loadTxn(ctx, tx, txnID)
		})
		return err
	})
	return out, replayed, err
}

// IgnoreTransaction marks a transaction as not needing a match (a fee, a transfer), with a reason.
func (s *Store) IgnoreTransaction(ctx context.Context, a *Principal, txnID int64, reason string) (*BankTxn, error) {
	if strings.TrimSpace(reason) == "" || !oneLineText(reason, 200) {
		return nil, invalid("reason", "required, one line, at most 200 characters")
	}
	var out *BankTxn
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := s.loadTxn(ctx, tx, txnID)
		if err != nil {
			return err
		}
		if t.Status != "unmatched" {
			return fmt.Errorf("%w: the transaction is already %s", ErrConflict, t.Status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE bank_transactions SET status = 'ignored', ignore_reason = ?, decided_by = ?, decided_at = ? WHERE id = ?`, reason, a.Subject, ts(s.now()), txnID); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, a.Subject, "bank.match.ignore", fmt.Sprint(txnID), map[string]any{"reason": reason}); err != nil {
			return err
		}
		out, err = s.loadTxn(ctx, tx, txnID)
		return err
	})
	return out, err
}

// ---- reconciliation -----------------------------------------------------------------------

// ReconPeriod is the state of one month of a bank account.
type ReconPeriod struct {
	Period         string `json:"period"`
	Transactions   int    `json:"transactions"`
	Matched        int    `json:"matched"`
	Ignored        int    `json:"ignored"`
	Unmatched      int    `json:"unmatched"`
	Status         string `json:"status"` // open, ready or reconciled
	ReconciledBy   string `json:"reconciled_by,omitempty"`
	ReconciledAtTS int64  `json:"-"`
}

// Reconciliation reports the most recent 36 months: how many transactions are matched, ignored and still
// unmatched, and whether the month was signed off. A month is "ready" when nothing is unmatched.
func (s *Store) Reconciliation(ctx context.Context, a *Principal, accountID int64) ([]ReconPeriod, error) {
	if err := s.auditRead(ctx, a, "bank.reconciliation.read", fmt.Sprint(accountID), map[string]any{}); err != nil {
		return nil, err
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	if _, err := s.GetBankAccount(rctx, a, accountID); err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(rctx, `SELECT substr(booked_on, 1, 7), COUNT(*), COALESCE(SUM(status = 'matched'), 0), COALESCE(SUM(status = 'ignored'), 0), COALESCE(SUM(status = 'unmatched'), 0)
		FROM bank_transactions WHERE bank_account_id = ? GROUP BY 1 ORDER BY 1 DESC LIMIT 36`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ReconPeriod{}
	for rows.Next() {
		var p ReconPeriod
		if err := rows.Scan(&p.Period, &p.Transactions, &p.Matched, &p.Ignored, &p.Unmatched); err != nil {
			return nil, err
		}
		p.Status = "open"
		if p.Unmatched == 0 {
			p.Status = "ready"
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		var by string
		err := s.rdb.QueryRowContext(rctx, `SELECT reconciled_by FROM bank_reconciliations WHERE bank_account_id = ? AND period = ?`, accountID, out[i].Period).Scan(&by)
		if err == nil {
			// A statement imported after the sign-off can bring new unmatched rows: the month is open again.
			if out[i].Unmatched == 0 {
				out[i].Status = "reconciled"
			}
			out[i].ReconciledBy = by
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return out, nil
}

// SignOffReconciliation records that a month is reconciled. It needs nothing unmatched in the month.
func (s *Store) SignOffReconciliation(ctx context.Context, a *Principal, accountID int64, period string) error {
	if _, err := time.Parse("2006-01", period); err != nil || len(period) != 7 {
		return invalid("period", "use YYYY-MM")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var n, matched, ignored, unmatched int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(status = 'matched'), 0), COALESCE(SUM(status = 'ignored'), 0), COALESCE(SUM(status = 'unmatched'), 0)
			FROM bank_transactions WHERE bank_account_id = ? AND substr(booked_on, 1, 7) = ?`, accountID, period).Scan(&n, &matched, &ignored, &unmatched); err != nil {
			return err
		}
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM bank_accounts WHERE id = ?`, accountID).Scan(&one); err != nil {
			return ErrNotFound
		}
		switch {
		case n == 0:
			return fmt.Errorf("%w: the bank account has no transactions in %s", ErrConflict, period)
		case unmatched > 0:
			return fmt.Errorf("%w: %d transactions in %s are still unmatched", ErrConflict, unmatched, period)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO bank_reconciliations (bank_account_id, period, txn_count, matched_count, ignored_count, reconciled_by, reconciled_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			accountID, period, n, matched, ignored, a.Subject, ts(s.now())); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return fmt.Errorf("%w: %s is already reconciled", ErrConflict, period)
			}
			return err
		}
		return s.audit(ctx, tx, a.Subject, "bank.reconciliation.signoff", fmt.Sprint(accountID), map[string]any{"period": period, "transactions": n})
	})
}
