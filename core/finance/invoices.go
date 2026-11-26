package finance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Invoice kinds and statuses.
const (
	KindInvoice    = "invoice"
	KindCreditNote = "credit_note"

	StDraft         = "draft"
	StApproved      = "approved"
	StSent          = "sent"
	StPartiallyPaid = "partially_paid"
	StPaid          = "paid"
	StVoid          = "void"
)

// Line is one priced invoice line. Every amount is in the document currency's minor unit.
type Line struct {
	Position       int    `json:"position"`
	Description    string `json:"description"`
	SKU            string `json:"sku"`
	QuantityMilli  int64  `json:"quantity_milli"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	DiscountBP     int64  `json:"discount_bp"`
	TaxCode        string `json:"tax_code"`
	TaxRatePPM     int64  `json:"tax_rate_ppm"`
	GrossMinor     int64  `json:"gross_minor"`
	DiscountMinor  int64  `json:"discount_minor"`
	TaxMinor       int64  `json:"tax_minor"`
	TotalMinor     int64  `json:"total_minor"`
}

// LineInput is a line a client sends. With a SKU, the description, unit price and tax code
// default from the catalogue (the product's currency must match the document).
type LineInput struct {
	Description    string  `json:"description"`
	SKU            string  `json:"sku"`
	QuantityMilli  int64   `json:"quantity_milli"`
	UnitPriceMinor *int64  `json:"unit_price_minor"`
	DiscountBP     int64   `json:"discount_bp"`
	TaxCode        *string `json:"tax_code"`
}

// PartySnapshot is the buyer data frozen onto an invoice when it is approved.
type PartySnapshot struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	TaxID   string `json:"tax_id"`
}

// Invoice is the API view of an invoice or credit note.
type Invoice struct {
	ID                  int64          `json:"id"`
	Kind                string         `json:"kind"`
	Status              string         `json:"status"`
	Series              string         `json:"series"`
	Number              *string        `json:"number"`
	PartyID             int64          `json:"party_id"`
	PartySnapshot       *PartySnapshot `json:"party_snapshot"`
	Project             string         `json:"project"`
	Currency            string         `json:"currency"`
	BaseCurrency        string         `json:"base_currency"`
	FXRate              string         `json:"fx_rate"`
	IssueDate           string         `json:"issue_date"`
	DueDate             string         `json:"due_date"`
	Lines               []Line         `json:"lines"`
	SubtotalMinor       int64          `json:"subtotal_minor"`
	DiscountMinor       int64          `json:"discount_minor"`
	TaxMinor            int64          `json:"tax_minor"`
	TotalMinor          int64          `json:"total_minor"`
	BaseTotalMinor      int64          `json:"base_total_minor"`
	PaidMinor           int64          `json:"paid_minor"`
	CreditedMinor       int64          `json:"credited_minor"`
	BalanceMinor        int64          `json:"balance_minor"`
	OriginalID          *int64         `json:"original_id"`
	Reason              string         `json:"reason"`
	Notes               string         `json:"notes"`
	PaymentInstructions string         `json:"payment_instructions"`
	CreatedBy           string         `json:"created_by"`
	ApprovedBy          *string        `json:"approved_by"`
	ApprovedAt          *time.Time     `json:"approved_at"`
	SentAt              *time.Time     `json:"sent_at"`
	PaidAt              *time.Time     `json:"paid_at"`
	VoidedAt            *time.Time     `json:"voided_at"`
	VoidReason          string         `json:"void_reason"`
	Version             int64          `json:"version"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`

	updatedBy string
	epoch     int64
}

// NewInvoice is the body of an invoice create. Status, number, totals, creator and version
// are not inputs: the server owns them.
type NewInvoice struct {
	PartyID             int64       `json:"party_id"`
	Series              string      `json:"series"`
	Project             string      `json:"project"`
	Currency            string      `json:"currency"`
	FXRate              string      `json:"fx_rate"`
	IssueDate           string      `json:"issue_date"`
	DueDate             string      `json:"due_date"`
	DueDays             *int        `json:"due_days"`
	Notes               string      `json:"notes"`
	PaymentInstructions *string     `json:"payment_instructions"`
	Lines               []LineInput `json:"lines"`
}

// InvoicePatch edits a draft. Lines, when present, replace all lines.
type InvoicePatch struct {
	Project             *string      `json:"project"`
	Currency            *string      `json:"currency"`
	FXRate              *string      `json:"fx_rate"`
	IssueDate           *string      `json:"issue_date"`
	DueDate             *string      `json:"due_date"`
	Notes               *string      `json:"notes"`
	PaymentInstructions *string      `json:"payment_instructions"`
	Lines               *[]LineInput `json:"lines"`
}

const invCols = `id, kind, status, series, number_text, party_id, party_snapshot, project, currency, base_currency, fx_rate_e8, issue_date, due_date,
 subtotal_minor, discount_minor, tax_minor, total_minor, base_total_minor, paid_minor, credited_minor, original_id, reason, notes, payment_instructions,
 created_by, updated_by, approved_by, approved_at, sent_at, paid_at, voided_at, void_reason, version, created_at, updated_at, link_epoch`

func scanInvoice(sc scanner) (*Invoice, error) {
	var (
		v                             Invoice
		num, appBy                    sql.NullString
		snap                          string
		fx                            int64
		orig                          sql.NullInt64
		appAt, sentAt, paidAt, voidAt sql.NullInt64
		created, updated              int64
	)
	if err := sc.Scan(&v.ID, &v.Kind, &v.Status, &v.Series, &num, &v.PartyID, &snap, &v.Project, &v.Currency, &v.BaseCurrency, &fx, &v.IssueDate, &v.DueDate,
		&v.SubtotalMinor, &v.DiscountMinor, &v.TaxMinor, &v.TotalMinor, &v.BaseTotalMinor, &v.PaidMinor, &v.CreditedMinor, &orig, &v.Reason, &v.Notes, &v.PaymentInstructions,
		&v.CreatedBy, &v.updatedBy, &appBy, &appAt, &sentAt, &paidAt, &voidAt, &v.VoidReason, &v.Version, &created, &updated, &v.epoch); err != nil {
		return nil, err
	}
	v.Number, v.ApprovedBy = nullStr(num), nullStr(appBy)
	v.FXRate = FormatFX(fx)
	if orig.Valid {
		v.OriginalID = &orig.Int64
	}
	if snap != "" {
		var ps PartySnapshot
		if json.Unmarshal([]byte(snap), &ps) == nil {
			v.PartySnapshot = &ps
		}
	}
	v.ApprovedAt, v.SentAt, v.PaidAt, v.VoidedAt = nullTime(appAt), nullTime(sentAt), nullTime(paidAt), nullTime(voidAt)
	v.CreatedAt, v.UpdatedAt = fromUnix(created), fromUnix(updated)
	if v.Kind == KindInvoice {
		v.BalanceMinor = v.TotalMinor - v.PaidMinor - v.CreditedMinor
	}
	return &v, nil
}

func (s *Store) loadLines(ctx context.Context, q dbq, id int64) ([]Line, error) {
	rows, err := q.QueryContext(ctx, `SELECT position, description, sku, qty_milli, unit_price_minor, discount_bp, tax_code, tax_rate_ppm, gross_minor, discount_minor, tax_minor, total_minor FROM invoice_lines WHERE invoice_id = ? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Line{}
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.Position, &l.Description, &l.SKU, &l.QuantityMilli, &l.UnitPriceMinor, &l.DiscountBP, &l.TaxCode, &l.TaxRatePPM, &l.GrossMinor, &l.DiscountMinor, &l.TaxMinor, &l.TotalMinor); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// scopeWhere is the scope applied to every invoice query: the party and the project, joined
// with AND to everything else.
func scopeWhere(a *Principal, w *where, party, project string) {
	c, args := partyClause(a, party)
	w.scope(c, args)
	c, args = projectClause(a, project)
	w.scope(c, args)
}

// loadInvoice loads one invoice with its lines, only if it is in the caller's scope; an
// invoice outside the scope is reported as not found, like one that does not exist.
func (s *Store) loadInvoice(ctx context.Context, q dbq, a *Principal, id int64) (*Invoice, error) {
	var w where
	w.and("(id = ?)", id)
	scopeWhere(a, &w, "party_id", "project")
	v, err := scanInvoice(q.QueryRowContext(ctx, `SELECT `+invCols+` FROM invoices WHERE `+w.sql(), w.args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if v.Lines, err = s.loadLines(ctx, q, id); err != nil {
		return nil, err
	}
	return v, nil
}

// GetInvoice returns an invoice in scope.
func (s *Store) GetInvoice(ctx context.Context, a *Principal, id int64) (*Invoice, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	return s.loadInvoice(rctx, s.rdb, a, id)
}

// InvoiceFilter selects invoices.
type InvoiceFilter struct {
	Kind, Status, Project string
	PartyID               int64
	From, To              string // issue_date range, inclusive
	Overdue               string // as-of date: only unpaid invoices due before it
}

// ListInvoices lists invoices in scope, newest first, with an id cursor.
func (s *Store) ListInvoices(ctx context.Context, a *Principal, f InvoiceFilter, before int64, limit int) ([]Invoice, int64, error) {
	limit = clampLimit(limit)
	var w where
	scopeWhere(a, &w, "party_id", "project")
	if before > 0 {
		w.and("(id < ?)", before)
	}
	if f.Kind != "" {
		w.and("(kind = ?)", f.Kind)
	}
	if f.Status != "" {
		w.and("(status = ?)", f.Status)
	}
	if f.Project != "" {
		w.and("(project = ?)", f.Project)
	}
	if f.PartyID > 0 {
		w.and("(party_id = ?)", f.PartyID)
	}
	if f.From != "" {
		w.and("(issue_date >= ?)", f.From)
	}
	if f.To != "" {
		w.and("(issue_date <= ?)", f.To)
	}
	if f.Overdue != "" {
		w.and("(kind = 'invoice' AND status IN ('approved', 'sent', 'partially_paid') AND due_date < ?)", f.Overdue)
	}
	cancel, rctx := readCtx(ctx)
	defer cancel()
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+invCols+` FROM invoices WHERE `+w.sql()+` ORDER BY id DESC LIMIT ?`, append(w.args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	out := []Invoice{}
	for rows.Next() {
		v, err := scanInvoice(rows)
		if err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		out = append(out, *v)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > limit {
		out = out[:limit]
		next = out[limit-1].ID
	}
	for i := range out {
		if out[i].Lines, err = s.loadLines(rctx, s.rdb, out[i].ID); err != nil {
			return nil, 0, err
		}
	}
	return out, next, nil
}

// ---- pricing ----------------------------------------------------------------------------

type totals struct{ subtotal, discount, tax, total int64 }

// priceLines validates line inputs and prices them against the catalogue and the tax table.
func (s *Store) priceLines(ctx context.Context, q dbq, currency, field string, in []LineInput) ([]Line, totals, error) {
	var t totals
	if len(in) == 0 || len(in) > maxLines {
		return nil, t, invalid(field, "give between 1 and %d lines", maxLines)
	}
	var p problems
	out := make([]Line, 0, len(in))
	for i, li := range in {
		f := fmt.Sprintf("%s[%d]", field, i)
		l := Line{Position: i + 1, Description: li.Description, SKU: li.SKU, QuantityMilli: li.QuantityMilli, DiscountBP: li.DiscountBP}
		unit := li.UnitPriceMinor
		taxCode := ""
		if li.TaxCode != nil {
			taxCode = *li.TaxCode
		}
		if li.SKU != "" {
			var prod Product
			err := q.QueryRowContext(ctx, productSelect+` WHERE sku = ? AND active = 1`, li.SKU).
				Scan(&prod.SKU, &prod.Name, &prod.Description, &prod.UnitPriceMinor, &prod.Currency, &prod.TaxCode, &prod.Active, &prod.Version)
			if err != nil {
				p.add(f+".sku", "unknown or inactive product")
				continue
			}
			if prod.Currency != currency && unit == nil {
				p.add(f+".sku", "product is priced in %s, not %s: give unit_price_minor", prod.Currency, currency)
				continue
			}
			if l.Description == "" {
				l.Description = prod.Name
			}
			if unit == nil {
				u := prod.UnitPriceMinor
				unit = &u
			}
			if li.TaxCode == nil {
				taxCode = prod.TaxCode
			}
		}
		if unit == nil {
			p.add(f+".unit_price_minor", "required")
			continue
		}
		if strings.TrimSpace(l.Description) == "" || !oneLineText(l.Description, 500) {
			p.add(f+".description", "required, one line, at most 500 characters")
			continue
		}
		ppm, err := taxRate(ctx, q, taxCode)
		if err != nil {
			p.add(f+".tax_code", "%v", err)
			continue
		}
		r, err := CalcLine(li.QuantityMilli, *unit, li.DiscountBP, ppm)
		if err != nil {
			p.add(f, "%v", err)
			continue
		}
		l.UnitPriceMinor, l.TaxCode, l.TaxRatePPM = *unit, taxCode, ppm
		l.GrossMinor, l.DiscountMinor, l.TaxMinor, l.TotalMinor = r.Gross, r.Discount, r.Tax, r.Total
		var aerr error
		for _, add := range []struct {
			dst *int64
			v   int64
		}{{&t.subtotal, r.Gross}, {&t.discount, r.Discount}, {&t.tax, r.Tax}, {&t.total, r.Total}} {
			if *add.dst, aerr = AddMinor(*add.dst, add.v); aerr != nil {
				p.add(field, "document total is too large")
				break
			}
		}
		out = append(out, l)
	}
	if err := p.err(); err != nil {
		return nil, t, err
	}
	return out, t, nil
}

func linesToInputs(ls []Line) []LineInput {
	out := make([]LineInput, len(ls))
	for i, l := range ls {
		u, tc := l.UnitPriceMinor, l.TaxCode
		out[i] = LineInput{Description: l.Description, SKU: "", QuantityMilli: l.QuantityMilli, UnitPriceMinor: &u, DiscountBP: l.DiscountBP, TaxCode: &tc}
	}
	return out
}

// resolveFX returns the stored rate for a document currency. The base currency is always 1;
// any other currency needs an explicit rate (base units per one unit of the document
// currency), which is stored on the document and never looked up again.
func (s *Store) resolveFX(currency, fx string) (int64, error) {
	if _, ok := CurrencyExponent(currency); !ok {
		return 0, invalid("currency", "unknown currency code")
	}
	if currency == s.base {
		if fx != "" {
			if v, err := ParseFX(fx); err != nil || v != fxScale {
				return 0, invalid("fx_rate", "must be omitted or 1 in the base currency %s", s.base)
			}
		}
		return fxScale, nil
	}
	if fx == "" {
		return 0, invalid("fx_rate", "required: %s per 1 %s, for example 1.0850", s.base, currency)
	}
	v, err := ParseFX(fx)
	if err != nil {
		return 0, invalid("fx_rate", "%v", err)
	}
	return v, nil
}

func (s *Store) baseTotal(currency string, total, fxE8 int64) int64 {
	from, _ := CurrencyExponent(currency)
	to, _ := CurrencyExponent(s.base)
	if currency == s.base {
		return total
	}
	return ConvertMinor(total, from, to, fxE8)
}

func (s *Store) loadPartyRow(ctx context.Context, q dbq, a *Principal, id int64) (*Party, error) {
	if a != nil && !a.CanParty(id) {
		return nil, ErrNotFound
	}
	p, err := scanParty(q.QueryRowContext(ctx, partySelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invalid("party_id", "no such party")
	}
	return p, err
}

// ---- create / edit / delete drafts ------------------------------------------------------

func (in *NewInvoice) check(now time.Time) error {
	var p problems
	if in.PartyID < 1 {
		p.add("party_id", "required")
	}
	if in.Series != "" {
		if err := validInvoiceSeries(in.Series); err != nil {
			p.add("series", "1 to 6 capital letters, not CN or PO (reserved)")
		}
	}
	if in.Project != "" && !projectRE.MatchString(in.Project) {
		p.add("project", "not a project key")
	}
	if in.IssueDate != "" && !validDate(in.IssueDate) {
		p.add("issue_date", "use YYYY-MM-DD")
	}
	if in.DueDate != "" && !validDate(in.DueDate) {
		p.add("due_date", "use YYYY-MM-DD")
	}
	if in.DueDays != nil && (*in.DueDays < 0 || *in.DueDays > 365) {
		p.add("due_days", "between 0 and 365")
	}
	if in.DueDate != "" && in.DueDays != nil {
		p.add("due_date", "give due_date or due_days, not both")
	}
	if !validText(in.Notes, maxText) {
		p.add("notes", "at most %d characters", maxText)
	}
	if in.PaymentInstructions != nil && !validText(*in.PaymentInstructions, maxText) {
		p.add("payment_instructions", "at most %d characters", maxText)
	}
	return p.err()
}

// CreateInvoice creates a draft invoice. A retry with the same idempotency key returns the
// original draft (replayed = true).
func (s *Store) CreateInvoice(ctx context.Context, a *Principal, in NewInvoice, idemKey string) (*Invoice, bool, error) {
	if err := in.check(s.now()); err != nil {
		return nil, false, err
	}
	var out *Invoice
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, "invoice.create", in, func() (*Invoice, error) {
			if len(a.Projects) > 0 && (in.Project == "" || !a.CanProject(in.Project)) {
				return nil, invalid("project", "a project-scoped token must create invoices in one of its projects")
			}
			id, err := s.insertDraft(ctx, tx, a, a.Subject, KindInvoice, nil, in)
			if err != nil {
				return nil, err
			}
			return s.loadInvoice(ctx, tx, a, id)
		})
		return err
	})
	return out, replayed, err
}

// insertDraft prices and stores a draft. It is shared by manual creates, recurring schedules,
// the timesheet generator and credit notes. a may be nil for system callers (no scope).
func (s *Store) insertDraft(ctx context.Context, tx dbq, a *Principal, createdBy, kind string, orig *Invoice, in NewInvoice) (int64, error) {
	party, err := s.loadPartyRow(ctx, tx, a, in.PartyID)
	if err != nil {
		return 0, err
	}
	if party.Erased {
		return 0, invalid("party_id", "the party has been erased")
	}
	if party.Kind == "vendor" {
		return 0, invalid("party_id", "a vendor cannot be invoiced; make it a customer or both")
	}
	cur := in.Currency
	if cur == "" {
		cur = party.Currency
	}
	if cur == "" {
		cur = s.base
	}
	fx, err := s.resolveFX(cur, in.FXRate)
	if err != nil {
		return 0, err
	}
	lines, t, err := s.priceLines(ctx, tx, cur, "lines", in.Lines)
	if err != nil {
		return 0, err
	}
	now := s.now()
	issue := in.IssueDate
	if issue == "" {
		issue = dateOf(now)
	}
	due := in.DueDate
	if due == "" {
		days := 30
		if in.DueDays != nil {
			days = *in.DueDays
		}
		due = parseDate(issue).AddDate(0, 0, days).Format(time.DateOnly)
	}
	if due < issue {
		return 0, invalid("due_date", "must not be before issue_date")
	}
	series := in.Series
	switch {
	case kind == KindCreditNote:
		series = "CN" // fixed: credit notes have their own counter
	case series == "":
		series = "INV"
	default:
		if err := validInvoiceSeries(series); err != nil {
			return 0, err
		}
	}
	instr := s.payInstr
	if in.PaymentInstructions != nil {
		instr = *in.PaymentInstructions
	}
	var origID, reason any
	if orig != nil {
		origID = orig.ID
	}
	reason = ""
	res, err := tx.ExecContext(ctx, `INSERT INTO invoices (kind, status, series, party_id, project, currency, base_currency, fx_rate_e8, issue_date, due_date,
		subtotal_minor, discount_minor, tax_minor, total_minor, base_total_minor, original_id, reason, notes, payment_instructions, created_by, updated_by, created_at, updated_at)
		VALUES (?, 'draft', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, series, in.PartyID, in.Project, cur, s.base, fx, issue, due, t.subtotal, t.discount, t.tax, t.total, s.baseTotal(cur, t.total, fx),
		origID, reason, in.Notes, instr, createdBy, createdBy, ts(now), ts(now))
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, s.writeLines(ctx, tx, id, lines)
}

func (s *Store) writeLines(ctx context.Context, tx dbq, id int64, lines []Line) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM invoice_lines WHERE invoice_id = ?`, id); err != nil {
		return err
	}
	for _, l := range lines {
		if _, err := tx.ExecContext(ctx, `INSERT INTO invoice_lines (invoice_id, position, description, sku, qty_milli, unit_price_minor, discount_bp, tax_code, tax_rate_ppm, gross_minor, discount_minor, tax_minor, total_minor) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, l.Position, l.Description, l.SKU, l.QuantityMilli, l.UnitPriceMinor, l.DiscountBP, l.TaxCode, l.TaxRatePPM, l.GrossMinor, l.DiscountMinor, l.TaxMinor, l.TotalMinor); err != nil {
			return err
		}
	}
	return nil
}

// withInvoice runs fn on a scoped invoice inside a transaction, checking the optional
// If-Match version first. The returned invoice is reloaded after fn.
func (s *Store) withInvoice(ctx context.Context, a *Principal, id int64, ifMatch *int64, fn func(tx *sql.Tx, v *Invoice) error) (*Invoice, error) {
	var out *Invoice
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		v, err := s.loadInvoice(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != v.Version {
			return ErrVersionMismatch
		}
		if err := fn(tx, v); err != nil {
			return err
		}
		out, err = s.loadInvoice(ctx, tx, a, id)
		return err
	})
	return out, err
}

func notDraft(v *Invoice) error {
	return fmt.Errorf("%w: invoice %s is %s; issued invoices are immutable, void it and issue a credit note", ErrConflict, deref(v.Number), v.Status)
}

func deref(s *string) string {
	if s == nil {
		return "(draft)"
	}
	return *s
}

// bump updates the version and timestamps with an optimistic check; it returns ErrVersionMismatch
// when the row changed underneath (it cannot, inside the single-writer transaction, unless
// the caller skipped the version check, but the guard keeps the invariant local).
func (s *Store) bump(ctx context.Context, tx dbq, v *Invoice, set string, args ...any) error {
	args = append(args, ts(s.now()), v.ID, v.Version)
	res, err := tx.ExecContext(ctx, `UPDATE invoices SET `+set+`, version = version + 1, updated_at = ? WHERE id = ? AND version = ?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrVersionMismatch
	}
	return nil
}

// UpdateInvoice edits a draft.
func (s *Store) UpdateInvoice(ctx context.Context, a *Principal, id int64, ifMatch *int64, in InvoicePatch) (*Invoice, error) {
	return s.withInvoice(ctx, a, id, ifMatch, func(tx *sql.Tx, v *Invoice) error {
		if v.Status != StDraft {
			return notDraft(v)
		}
		var p problems
		str := func(dst *string, src *string, f string, max int, one bool) {
			if src == nil {
				return
			}
			if (one && !oneLineText(*src, max)) || !validText(*src, max) {
				p.add(f, "at most %d characters", max)
				return
			}
			*dst = *src
		}
		str(&v.Notes, in.Notes, "notes", maxText, false)
		str(&v.PaymentInstructions, in.PaymentInstructions, "payment_instructions", maxText, false)
		if in.Project != nil {
			if *in.Project != "" && !projectRE.MatchString(*in.Project) {
				p.add("project", "not a project key")
			} else if len(a.Projects) > 0 && !a.CanProject(*in.Project) {
				p.add("project", "outside your project scope")
			} else {
				v.Project = *in.Project
			}
		}
		for f, ref := range map[string]struct {
			src *string
			dst *string
		}{"issue_date": {in.IssueDate, &v.IssueDate}, "due_date": {in.DueDate, &v.DueDate}} {
			if ref.src == nil {
				continue
			}
			if !validDate(*ref.src) {
				p.add(f, "use YYYY-MM-DD")
			} else {
				*ref.dst = *ref.src
			}
		}
		if v.DueDate < v.IssueDate {
			p.add("due_date", "must not be before issue_date")
		}
		fxStr := v.FXRate
		if in.FXRate != nil {
			fxStr = *in.FXRate
		}
		if in.Currency != nil {
			v.Currency = *in.Currency
			if in.FXRate == nil && v.Currency != s.base {
				fxStr = ""
			}
		}
		if err := p.err(); err != nil {
			return err
		}
		fx, err := s.resolveFX(v.Currency, fxStr)
		if err != nil {
			return err
		}
		inputs := linesToInputs(v.Lines)
		if in.Lines != nil {
			inputs = *in.Lines
		}
		lines, t, err := s.priceLines(ctx, tx, v.Currency, "lines", inputs)
		if err != nil {
			return err
		}
		if err := s.writeLines(ctx, tx, v.ID, lines); err != nil {
			return err
		}
		return s.bump(ctx, tx, v, `project=?, currency=?, fx_rate_e8=?, issue_date=?, due_date=?, notes=?, payment_instructions=?, subtotal_minor=?, discount_minor=?, tax_minor=?, total_minor=?, base_total_minor=?, updated_by=?`,
			v.Project, v.Currency, fx, v.IssueDate, v.DueDate, v.Notes, v.PaymentInstructions, t.subtotal, t.discount, t.tax, t.total, s.baseTotal(v.Currency, t.total, fx), a.Subject)
	})
}

// DeleteDraft removes a draft. Drafts have no number, so deleting one leaves no gap. Time
// entries billed on it become billable again.
func (s *Store) DeleteDraft(ctx context.Context, a *Principal, id int64, ifMatch *int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		v, err := s.loadInvoice(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != v.Version {
			return ErrVersionMismatch
		}
		if v.Status != StDraft {
			return notDraft(v)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE time_entries SET invoice_id = NULL WHERE invoice_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM invoices WHERE id = ? AND status = 'draft'`, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "invoice.delete_draft", fmt.Sprint(id), map[string]any{})
	})
}

// releaseDraftEntries frees time entries tied to the invoices the id query selects.
func (s *Store) releaseDraftEntries(ctx context.Context, tx dbq, idQuery string, args ...any) error {
	_, err := tx.ExecContext(ctx, `UPDATE time_entries SET invoice_id = NULL WHERE invoice_id IN (`+idQuery+`)`, args...)
	return err
}

// ---- approval, sending, voiding ---------------------------------------------------------

// ApprovalRequest describes something awaiting approval, for the Approver.
type ApprovalRequest struct {
	Kind  string // invoice, timesheet, expense, purchase_order
	Owner string // the person the record belongs to (timesheets, expenses), else ""
	// Ref and Version identify the record and its version for approvals through can-core
	// ("invoice:<id>"); empty for a decision that is not tied to a can-core request.
	Ref     string
	Version int64
	// Perm is the permission the approver must hold; empty means finance.approve. Manual journals
	// ask for finance.ledger.approve (E15c) and go through the same separation-of-duties rule.
	Perm string
}

func (r ApprovalRequest) perm() string {
	if r.Perm == "" {
		return PermApprove
	}
	return r.Perm
}

// Approver decides whether a principal may approve a record. The default accepts anyone with
// finance.approve; a deployment with reporting lines (the `directory` capability) replaces
// it so that, for example, only the owner's manager may approve a timesheet.
type Approver interface {
	CanApprove(ctx context.Context, approver *Principal, req ApprovalRequest) (bool, error)
}

// ScopeApprover is the default Approver: the finance.approve permission is enough.
type ScopeApprover struct{}

// CanApprove implements Approver.
func (ScopeApprover) CanApprove(_ context.Context, p *Principal, req ApprovalRequest) (bool, error) {
	return p.Can(req.perm()), nil
}

// authorizeApproval is the one place approval rules live, so no path can skip them: the
// caller must hold finance.approve, the Approver must agree, and the approver must not be
// anyone who created, edited or owns the record (separation of duties).
func (s *Store) authorizeApproval(ctx context.Context, a *Principal, req ApprovalRequest, involved ...string) error {
	if !a.Can(req.perm()) {
		return ErrForbidden
	}
	if slices.Contains(ledgerPerms, req.perm()) && !allowedBy(a, req.perm(), false) {
		return ErrForbidden
	}
	for _, who := range append(involved, req.Owner) {
		if who != "" && sameSubject(who, a.Subject) {
			return ErrSeparation
		}
	}
	ok, err := s.approver.CanApprove(ctx, a, req)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// ApproveInvoice approves a draft: it gets the next gap-free number of its series, the
// buyer's name, address and tax id are frozen onto it, and from then on it cannot be edited.
func (s *Store) ApproveInvoice(ctx context.Context, a *Principal, id int64, ifMatch *int64) (*Invoice, error) {
	return s.withInvoice(ctx, a, id, ifMatch, func(tx *sql.Tx, v *Invoice) error {
		if v.Status != StDraft {
			return fmt.Errorf("%w: invoice is already %s", ErrConflict, v.Status)
		}
		if err := s.authorizeApproval(withTx(ctx, tx), a, ApprovalRequest{Kind: "invoice", Ref: fmt.Sprintf("invoice:%d", v.ID), Version: v.Version}, v.CreatedBy, v.updatedBy); err != nil {
			return err
		}
		if v.Kind == KindInvoice {
			if err := validInvoiceSeries(v.Series); err != nil {
				return err
			}
		}
		party, err := s.loadPartyRow(ctx, tx, nil, v.PartyID)
		if err != nil {
			return err
		}
		if party.Erased {
			return invalid("party_id", "the party has been erased")
		}
		// Re-apply the current tax table: a draft saved before a rate changed gets today's rate.
		lines, t, err := s.priceLines(ctx, tx, v.Currency, "lines", linesToInputs(v.Lines))
		if err != nil {
			return err
		}
		if v.Kind == KindCreditNote {
			var origTotal, origCredited, origPaid int64
			var origStatus string
			if err := tx.QueryRowContext(ctx, `SELECT total_minor, credited_minor, paid_minor, status FROM invoices WHERE id = ?`, v.OriginalID).Scan(&origTotal, &origCredited, &origPaid, &origStatus); err != nil {
				return err
			}
			if origStatus == StVoid || origStatus == StDraft {
				return fmt.Errorf("%w: the original invoice is %s", ErrConflict, origStatus)
			}
			// A credit note reduces what the customer still owes. Money already received is not
			// credited here (refunds are not modelled): the cap is the unpaid balance, so a balance
			// can never go negative.
			if open := origTotal - origPaid - origCredited; t.total > open {
				return invalid("lines", "the credit note (%s) exceeds the unpaid balance of the invoice (%s); money already paid is refunded outside this module", FormatMinor(t.total, v.Currency), FormatMinor(open, v.Currency))
			}
		}
		if err := s.writeLines(ctx, tx, v.ID, lines); err != nil {
			return err
		}
		var n int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO series (code, next_number) VALUES (?, 2) ON CONFLICT (code) DO UPDATE SET next_number = next_number + 1 RETURNING next_number - 1`, v.Series).Scan(&n); err != nil {
			return err
		}
		text := fmt.Sprintf("%s-%05d", v.Series, n)
		snap, _ := json.Marshal(PartySnapshot{Name: party.Name, Address: party.Address, TaxID: party.TaxID})
		now := ts(s.now())
		if err := s.bump(ctx, tx, v, `status='approved', number=?, number_text=?, party_snapshot=?, subtotal_minor=?, discount_minor=?, tax_minor=?, total_minor=?, base_total_minor=?, approved_by=?, approved_at=?, updated_by=?`,
			n, text, string(snap), t.subtotal, t.discount, t.tax, t.total, s.baseTotal(v.Currency, t.total, fxFromString(v.FXRate)), a.Subject, now, a.Subject); err != nil {
			return err
		}
		if v.Kind == KindCreditNote {
			if err := s.applyCredit(ctx, tx, *v.OriginalID, t.total); err != nil {
				return err
			}
		}
		if err := s.postInvoiceTx(ctx, tx, v, text, lines, a.Subject); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "invoice.approve", text, map[string]any{"id": v.ID, "total_minor": t.total, "currency": v.Currency})
	})
}

func fxFromString(s string) int64 {
	v, _ := ParseFX(s)
	return v
}

// applyCredit records a credit note against its invoice and settles it when nothing is left.
func (s *Store) applyCredit(ctx context.Context, tx dbq, origID, amount int64) error {
	now := ts(s.now())
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET credited_minor = credited_minor + ?, version = version + 1, updated_at = ?,
		status = CASE WHEN total_minor - paid_minor - credited_minor - ? <= 0 THEN 'paid' ELSE status END,
		paid_at = CASE WHEN total_minor - paid_minor - credited_minor - ? <= 0 AND paid_at IS NULL THEN ? ELSE paid_at END
		WHERE id = ?`, amount, now, amount, amount, now, origID); err != nil {
		return err
	}
	return nil
}

// SendInvoice marks an approved invoice as sent and emits invoice.sent.v1. It sends nothing: it
// records that the invoice left the building by other means (EmailInvoice mails it and does this).
// Sending a sent invoice again is accepted and changes nothing.
func (s *Store) SendInvoice(ctx context.Context, a *Principal, id int64, ifMatch *int64) (*Invoice, error) {
	return s.withInvoice(ctx, a, id, ifMatch, func(tx *sql.Tx, v *Invoice) error {
		switch v.Status {
		case StSent, StPartiallyPaid, StPaid:
			return nil
		case StApproved:
		default:
			return fmt.Errorf("%w: only an approved invoice can be sent (it is %s)", ErrConflict, v.Status)
		}
		return s.sendTx(ctx, tx, a.Subject, v)
	})
}

// sendTx turns an approved invoice into a sent one.
func (s *Store) sendTx(ctx context.Context, tx dbq, actor string, v *Invoice) error {
	if err := s.bump(ctx, tx, v, `status='sent', sent_at=?, updated_by=?`, ts(s.now()), actor); err != nil {
		return err
	}
	return s.emit(ctx, tx, EventInvoiceSent, invoiceSentData{
		InvoiceID: v.ID, Number: deref(v.Number), Kind: v.Kind, PartyID: v.PartyID, Currency: v.Currency,
		TotalMinor: v.TotalMinor, DueDate: v.DueDate, SentAt: s.now().UTC(),
	})
}

type invoiceSentData struct {
	InvoiceID  int64     `json:"invoice_id"`
	Number     string    `json:"number"`
	Kind       string    `json:"kind"`
	PartyID    int64     `json:"party_id"`
	Currency   string    `json:"currency"`
	TotalMinor int64     `json:"total_minor"`
	DueDate    string    `json:"due_date"`
	SentAt     time.Time `json:"sent_at"`
}

type invoicePaidData struct {
	InvoiceID  int64     `json:"invoice_id"`
	Number     string    `json:"number"`
	PartyID    int64     `json:"party_id"`
	Currency   string    `json:"currency"`
	TotalMinor int64     `json:"total_minor"`
	PaidMinor  int64     `json:"paid_minor"`
	PaidAt     time.Time `json:"paid_at"`
}

// VoidInvoice voids an invoice that has no payments and no credit notes. The number stays
// used (gap-free numbering) and the record stays. Corrections after payment go through
// credit notes. Credit notes cannot be voided: issue an invoice for the difference.
func (s *Store) VoidInvoice(ctx context.Context, a *Principal, id int64, ifMatch *int64, reason string) (*Invoice, error) {
	if strings.TrimSpace(reason) == "" || !oneLineText(reason, 500) {
		return nil, invalid("reason", "required, one line, at most 500 characters")
	}
	return s.withInvoice(ctx, a, id, ifMatch, func(tx *sql.Tx, v *Invoice) error {
		// Voiding undoes an approval, so it follows the same rules: the person who drafted the
		// invoice cannot void it (after approval the "last editor" is whoever approved or sent it,
		// so only the creator is compared).
		if err := s.authorizeApproval(ctx, a, ApprovalRequest{Kind: "invoice"}, v.CreatedBy); err != nil {
			return err
		}
		switch {
		case v.Kind == KindCreditNote:
			return fmt.Errorf("%w: credit notes cannot be voided; issue an invoice for the difference", ErrConflict)
		case v.Status == StDraft:
			return fmt.Errorf("%w: delete a draft instead of voiding it", ErrConflict)
		case v.Status == StVoid:
			return fmt.Errorf("%w: already void", ErrConflict)
		case v.PaidMinor > 0 || v.CreditedMinor > 0:
			return fmt.Errorf("%w: the invoice has payments or credit notes; issue a credit note instead", ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE time_entries SET invoice_id = NULL WHERE invoice_id = ?`, v.ID); err != nil {
			return err
		}
		if err := s.bump(ctx, tx, v, `status='void', voided_at=?, void_reason=?, updated_by=?`, ts(s.now()), reason, a.Subject); err != nil {
			return err
		}
		if err := s.unpostInvoiceTx(ctx, tx, v, a.Subject); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "invoice.void", deref(v.Number), map[string]any{"id": v.ID, "reason": reason})
	})
}

// CreateCreditNote drafts a credit note against an issued invoice. It copies the invoice's
// party, currency, FX rate and project; the lines are the credited items.
func (s *Store) CreateCreditNote(ctx context.Context, a *Principal, origID int64, reason string, lines []LineInput, idemKey string) (*Invoice, bool, error) {
	if strings.TrimSpace(reason) == "" || !oneLineText(reason, 500) {
		return nil, false, invalid("reason", "required, one line, at most 500 characters")
	}
	var out *Invoice
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, fmt.Sprintf("credit_note.create.%d", origID), map[string]any{"reason": reason, "lines": lines}, func() (*Invoice, error) {
			orig, err := s.loadInvoice(ctx, tx, a, origID)
			if err != nil {
				return nil, err
			}
			if orig.Kind != KindInvoice || orig.Status == StDraft || orig.Status == StVoid {
				return nil, fmt.Errorf("%w: a credit note needs an issued, not void, invoice", ErrConflict)
			}
			in := NewInvoice{PartyID: orig.PartyID, Project: orig.Project, Currency: orig.Currency, FXRate: orig.FXRate, Lines: lines}
			id, err := s.insertDraft(ctx, tx, a, a.Subject, KindCreditNote, orig, in)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE invoices SET reason = ? WHERE id = ?`, reason, id); err != nil {
				return nil, err
			}
			return s.loadInvoice(ctx, tx, a, id)
		})
		return err
	})
	return out, replayed, err
}

// ---- links ------------------------------------------------------------------------------

// RevokeLinks invalidates every signed link issued for an invoice.
func (s *Store) RevokeLinks(ctx context.Context, a *Principal, id int64) (*Invoice, error) {
	return s.withInvoice(ctx, a, id, nil, func(tx *sql.Tx, v *Invoice) error {
		if _, err := tx.ExecContext(ctx, `UPDATE invoices SET link_epoch = link_epoch + 1 WHERE id = ?`, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "invoice.link_revoke", deref(v.Number), map[string]any{"id": id})
	})
}
