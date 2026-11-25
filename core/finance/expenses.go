package finance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Expense statuses.
const (
	ExDraft      = "draft"
	ExSubmitted  = "submitted"
	ExApproved   = "approved"
	ExRejected   = "rejected"
	ExReimbursed = "reimbursed"
)

var receiptTypes = []string{"application/pdf", "image/png", "image/jpeg", "image/webp", "image/heic"}

const maxReceiptBytes = 20 << 20

// Receipt is metadata about a receipt file kept elsewhere (the `files` module). This service
// never receives file bytes: it records the name, type, size and SHA-256 so the file can
// be matched and its integrity checked.
type Receipt struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
}

// Expense is a claim for money spent.
type Expense struct {
	ID               int64      `json:"id"`
	Owner            string     `json:"owner"`
	Description      string     `json:"description"`
	AmountMinor      int64      `json:"amount_minor"`
	Currency         string     `json:"currency"`
	Category         string     `json:"category"`
	IncurredOn       string     `json:"incurred_on"`
	Project          string     `json:"project"`
	Receipt          *Receipt   `json:"receipt"`
	Status           string     `json:"status"`
	DecidedBy        *string    `json:"decided_by"`
	DecidedAt        *time.Time `json:"decided_at"`
	RejectReason     string     `json:"reject_reason"`
	ReimbursementRef string     `json:"reimbursement_ref"`
	Version          int64      `json:"version"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// ExpenseInput is the body of an expense create or edit. The owner is always the caller.
type ExpenseInput struct {
	Description string   `json:"description"`
	AmountMinor int64    `json:"amount_minor"`
	Currency    string   `json:"currency"`
	Category    string   `json:"category"`
	IncurredOn  string   `json:"incurred_on"`
	Project     string   `json:"project"`
	Receipt     *Receipt `json:"receipt"`
}

func (in *ExpenseInput) check() error {
	var p problems
	if strings.TrimSpace(in.Description) == "" || !oneLineText(in.Description, 500) {
		p.add("description", "required, one line, at most 500 characters")
	}
	if in.AmountMinor <= 0 || in.AmountMinor > maxMinor {
		p.add("amount_minor", "must be greater than zero")
	}
	if _, ok := CurrencyExponent(in.Currency); !ok {
		p.add("currency", "unknown currency code")
	}
	if !categoryRE.MatchString(in.Category) {
		p.add("category", "lower-case word such as travel or meals")
	}
	if !validDate(in.IncurredOn) {
		p.add("incurred_on", "use YYYY-MM-DD")
	}
	if in.Project != "" && !projectRE.MatchString(in.Project) {
		p.add("project", "not a project key")
	}
	if r := in.Receipt; r != nil {
		if !filenameRE.MatchString(r.Filename) {
			p.add("receipt.filename", "1 to 255 characters, no slashes or control characters")
		}
		if !slices.Contains(receiptTypes, r.ContentType) {
			p.add("receipt.content_type", "one of %s", strings.Join(receiptTypes, ", "))
		}
		if r.SizeBytes < 1 || r.SizeBytes > maxReceiptBytes {
			p.add("receipt.size_bytes", "between 1 and %d", maxReceiptBytes)
		}
		if !sha256RE.MatchString(r.SHA256) {
			p.add("receipt.sha256", "lower-case hex SHA-256")
		}
	}
	return p.err()
}

const exCols = `id, owner, description, amount_minor, currency, category, incurred_on, project, receipt_filename, receipt_content_type, receipt_size, receipt_sha256, status, decided_by, decided_at, reject_reason, reimbursement_ref, version, created_at, updated_at`

func scanExpense(sc scanner) (*Expense, error) {
	var e Expense
	var r Receipt
	var dby sql.NullString
	var dat sql.NullInt64
	var c, u int64
	if err := sc.Scan(&e.ID, &e.Owner, &e.Description, &e.AmountMinor, &e.Currency, &e.Category, &e.IncurredOn, &e.Project, &r.Filename, &r.ContentType, &r.SizeBytes, &r.SHA256, &e.Status, &dby, &dat, &e.RejectReason, &e.ReimbursementRef, &e.Version, &c, &u); err != nil {
		return nil, err
	}
	if r.Filename != "" {
		e.Receipt = &r
	}
	e.DecidedBy, e.DecidedAt = nullStr(dby), nullTime(dat)
	e.CreatedAt, e.UpdatedAt = fromUnix(c), fromUnix(u)
	return &e, nil
}

func (s *Store) exScope(a *Principal, w *where) {
	c, args := ownerClause(a, "owner")
	w.scope(c, args)
	c, args = projectClause(a, "project")
	w.scope(c, args)
	if len(a.Parties) > 0 {
		w.and("(1 = 0)")
	}
}

func (s *Store) loadExpense(ctx context.Context, q dbq, a *Principal, id int64) (*Expense, error) {
	var w where
	w.and("(id = ?)", id)
	s.exScope(a, &w)
	e, err := scanExpense(q.QueryRowContext(ctx, `SELECT `+exCols+` FROM expenses WHERE `+w.sql(), w.args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// GetExpense returns an expense in scope.
func (s *Store) GetExpense(ctx context.Context, a *Principal, id int64) (*Expense, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	return s.loadExpense(rctx, s.rdb, a, id)
}

// ListExpenses lists expenses in scope, newest first.
func (s *Store) ListExpenses(ctx context.Context, a *Principal, status, owner string, before int64, limit int) ([]Expense, int64, error) {
	limit = clampLimit(limit)
	var w where
	s.exScope(a, &w)
	if before > 0 {
		w.and("(id < ?)", before)
	}
	if status != "" {
		w.and("(status = ?)", status)
	}
	if owner != "" {
		w.and("(owner = ?)", owner)
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+exCols+` FROM expenses WHERE `+w.sql()+` ORDER BY id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := []Expense{}
	for rows.Next() {
		e, err := scanExpense(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *e)
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	return out, next, rows.Err()
}

func (s *Store) checkExpenseProject(a *Principal, project string) error {
	if len(a.Projects) > 0 && !a.CanProject(project) {
		return invalid("project", "a project-scoped token must use one of its projects")
	}
	return nil
}

// CreateExpense files a draft expense for the caller. Idempotent on the key.
func (s *Store) CreateExpense(ctx context.Context, a *Principal, in ExpenseInput, idemKey string) (*Expense, bool, error) {
	if err := in.check(); err != nil {
		return nil, false, err
	}
	if err := s.checkExpenseProject(a, in.Project); err != nil {
		return nil, false, err
	}
	if len(a.Parties) > 0 {
		return nil, false, ErrForbidden
	}
	var out *Expense
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, "expense.create", in, func() (*Expense, error) {
			if err := s.checkUnlocked(ctx, tx, in.IncurredOn); err != nil {
				return nil, err
			}
			r := in.Receipt
			if r == nil {
				r = &Receipt{}
			}
			now := ts(s.now())
			res, err := tx.ExecContext(ctx, `INSERT INTO expenses (owner, description, amount_minor, currency, category, incurred_on, project, receipt_filename, receipt_content_type, receipt_size, receipt_sha256, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				a.Subject, in.Description, in.AmountMinor, in.Currency, in.Category, in.IncurredOn, in.Project, r.Filename, r.ContentType, r.SizeBytes, r.SHA256, a.Subject, now, now)
			if err != nil {
				return nil, err
			}
			id, _ := res.LastInsertId()
			return s.loadExpense(ctx, tx, a, id)
		})
		return err
	})
	return out, replayed, err
}

func (s *Store) withExpense(ctx context.Context, a *Principal, id int64, ifMatch *int64, fn func(tx *sql.Tx, e *Expense) error) (*Expense, error) {
	var out *Expense
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		e, err := s.loadExpense(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != e.Version {
			return ErrVersionMismatch
		}
		if err := fn(tx, e); err != nil {
			return err
		}
		out, err = s.loadExpense(ctx, tx, a, id)
		return err
	})
	return out, err
}

// UpdateExpense edits a draft or rejected expense. Only its owner edits it.
func (s *Store) UpdateExpense(ctx context.Context, a *Principal, id int64, ifMatch *int64, in ExpenseInput) (*Expense, error) {
	if err := in.check(); err != nil {
		return nil, err
	}
	if err := s.checkExpenseProject(a, in.Project); err != nil {
		return nil, err
	}
	return s.withExpense(ctx, a, id, ifMatch, func(tx *sql.Tx, e *Expense) error {
		if e.Owner != a.Subject {
			return ErrNotFound
		}
		if e.Status != ExDraft && e.Status != ExRejected {
			return fmt.Errorf("%w: a %s expense cannot be edited", ErrConflict, e.Status)
		}
		if err := s.checkUnlocked(ctx, tx, in.IncurredOn); err != nil {
			return err
		}
		r := in.Receipt
		if r == nil {
			r = &Receipt{}
		}
		_, err := tx.ExecContext(ctx, `UPDATE expenses SET description=?, amount_minor=?, currency=?, category=?, incurred_on=?, project=?, receipt_filename=?, receipt_content_type=?, receipt_size=?, receipt_sha256=?, status='draft', version=version+1, updated_at=? WHERE id=?`,
			in.Description, in.AmountMinor, in.Currency, in.Category, in.IncurredOn, in.Project, r.Filename, r.ContentType, r.SizeBytes, r.SHA256, ts(s.now()), id)
		return err
	})
}

// SubmitExpense sends an expense for approval.
func (s *Store) SubmitExpense(ctx context.Context, a *Principal, id int64, ifMatch *int64) (*Expense, error) {
	return s.withExpense(ctx, a, id, ifMatch, func(tx *sql.Tx, e *Expense) error {
		if e.Owner != a.Subject {
			return ErrNotFound
		}
		if e.Status != ExDraft && e.Status != ExRejected {
			return fmt.Errorf("%w: the expense is %s", ErrConflict, e.Status)
		}
		if err := s.checkUnlocked(ctx, tx, e.IncurredOn); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE expenses SET status='submitted', reject_reason='', version=version+1, updated_at=? WHERE id=?`, ts(s.now()), id)
		return err
	})
}

type expenseApprovedData struct {
	ExpenseID   int64     `json:"expense_id"`
	Person      string    `json:"person"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	Category    string    `json:"category"`
	ApprovedAt  time.Time `json:"approved_at"`
}

func (s *Store) decideExpense(ctx context.Context, a *Principal, id int64, ifMatch *int64, approve bool, reason string) (*Expense, error) {
	if !approve && (strings.TrimSpace(reason) == "" || !oneLineText(reason, 500)) {
		return nil, invalid("reason", "required, one line, at most 500 characters")
	}
	return s.withExpense(ctx, a, id, ifMatch, func(tx *sql.Tx, e *Expense) error {
		if err := s.authorizeApproval(ctx, a, ApprovalRequest{Kind: "expense", Owner: e.Owner}); err != nil {
			return err
		}
		if e.Status != ExSubmitted {
			return fmt.Errorf("%w: the expense is %s, not submitted", ErrConflict, e.Status)
		}
		if err := s.checkUnlocked(ctx, tx, e.IncurredOn); err != nil {
			return err
		}
		now := s.now()
		status := ExRejected
		if approve {
			status = ExApproved
		}
		if _, err := tx.ExecContext(ctx, `UPDATE expenses SET status=?, decided_by=?, decided_at=?, reject_reason=?, version=version+1, updated_at=? WHERE id=?`, status, a.Subject, ts(now), reason, ts(now), id); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, a.Subject, "expense."+status, fmt.Sprint(id), map[string]any{"owner": e.Owner, "amount_minor": e.AmountMinor, "currency": e.Currency}); err != nil {
			return err
		}
		if approve {
			return s.emit(ctx, tx, EventExpenseApproved, expenseApprovedData{ExpenseID: id, Person: e.Owner, AmountMinor: e.AmountMinor, Currency: e.Currency, Category: e.Category, ApprovedAt: now.UTC()})
		}
		return nil
	})
}

// ApproveExpense approves a submitted expense; nobody approves their own.
func (s *Store) ApproveExpense(ctx context.Context, a *Principal, id int64, ifMatch *int64) (*Expense, error) {
	return s.decideExpense(ctx, a, id, ifMatch, true, "")
}

// RejectExpense sends an expense back with a reason.
func (s *Store) RejectExpense(ctx context.Context, a *Principal, id int64, ifMatch *int64, reason string) (*Expense, error) {
	return s.decideExpense(ctx, a, id, ifMatch, false, reason)
}

// ReimburseExpense records that an approved expense was paid back. The person paying it
// is not its owner (nobody reimburses themselves).
func (s *Store) ReimburseExpense(ctx context.Context, a *Principal, id int64, ifMatch *int64, ref string) (*Expense, error) {
	if !oneLineText(ref, 200) {
		return nil, invalid("reference", "one line, at most 200 characters")
	}
	return s.withExpense(ctx, a, id, ifMatch, func(tx *sql.Tx, e *Expense) error {
		if e.Owner == a.Subject {
			return ErrSeparation
		}
		if e.Status != ExApproved {
			return fmt.Errorf("%w: the expense is %s, not approved", ErrConflict, e.Status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE expenses SET status='reimbursed', reimbursement_ref=?, version=version+1, updated_at=? WHERE id=?`, ref, ts(s.now()), id); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "expense.reimburse", fmt.Sprint(id), map[string]any{"reference": ref})
	})
}
