package finance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Payment is money received against an invoice, in the invoice's currency.
type Payment struct {
	ID          int64     `json:"id"`
	InvoiceID   int64     `json:"invoice_id"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	Method      string    `json:"method"`
	Reference   string    `json:"reference"`
	ReceivedOn  string    `json:"received_on"`
	Source      string    `json:"source"`
	ExternalID  string    `json:"external_id"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// PaymentInput is the body of a payment record.
type PaymentInput struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Method      string `json:"method"`
	Reference   string `json:"reference"`
	ReceivedOn  string `json:"received_on"`
	ExternalID  string `json:"external_id"`

	// cashAccount is the ledger account of the bank account a confirmed bank transaction arrived in
	// (E15c); not part of the request body.
	cashAccount int64
}

const paymentCols = `id, invoice_id, amount_minor, currency, method, reference, received_on, source, external_id, created_by, created_at`

func scanPayment(sc scanner) (*Payment, error) {
	var p Payment
	var c int64
	if err := sc.Scan(&p.ID, &p.InvoiceID, &p.AmountMinor, &p.Currency, &p.Method, &p.Reference, &p.ReceivedOn, &p.Source, &p.ExternalID, &p.CreatedBy, &c); err != nil {
		return nil, err
	}
	p.CreatedAt = fromUnix(c)
	return &p, nil
}

func (in *PaymentInput) check() error {
	var p problems
	if in.AmountMinor <= 0 || in.AmountMinor > maxMinor {
		p.add("amount_minor", "must be greater than zero")
	}
	if in.Currency != "" {
		if _, ok := CurrencyExponent(in.Currency); !ok {
			p.add("currency", "unknown currency code")
		}
	}
	if !oneLineText(in.Method, 50) {
		p.add("method", "one line, at most 50 characters")
	}
	if !oneLineText(in.Reference, 200) {
		p.add("reference", "one line, at most 200 characters")
	}
	if in.ReceivedOn != "" && !validDate(in.ReceivedOn) {
		p.add("received_on", "use YYYY-MM-DD")
	}
	if !oneLineText(in.ExternalID, 200) {
		p.add("external_id", "one line, at most 200 characters")
	}
	return p.err()
}

// RecordPayment records a manual payment. A retry with the same idempotency key returns the
// original payment. A payment cannot exceed the invoice's balance and must be in its currency.
func (s *Store) RecordPayment(ctx context.Context, a *Principal, invoiceID int64, in PaymentInput, idemKey string) (*Payment, bool, error) {
	if err := in.check(); err != nil {
		return nil, false, err
	}
	var out *Payment
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, fmt.Sprintf("payment.create.%d", invoiceID), in, func() (*Payment, error) {
			p, _, err := s.recordPaymentTx(ctx, tx, a, a.Subject, invoiceID, in, "manual")
			return p, err
		})
		return err
	})
	return out, replayed, err
}

// recordPaymentTx is the single place a payment is written. a may be nil for the provider
// webhook, which authenticates by signature, not by a token.
func (s *Store) recordPaymentTx(ctx context.Context, tx dbq, a *Principal, actor string, invoiceID int64, in PaymentInput, source string) (*Payment, bool, error) {
	var inv *Invoice
	var err error
	if a != nil {
		inv, err = s.loadInvoice(ctx, tx, a, invoiceID)
	} else {
		inv, err = s.loadInvoice(ctx, tx, &Principal{}, invoiceID)
	}
	if err != nil {
		return nil, false, err
	}
	if in.ExternalID != "" {
		ex, err := scanPayment(tx.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM payments WHERE source = ? AND external_id = ?`, source, in.ExternalID))
		if err == nil {
			if ex.InvoiceID != invoiceID || ex.AmountMinor != in.AmountMinor {
				return nil, false, fmt.Errorf("%w: external_id %s was already used for a different payment", ErrConflict, in.ExternalID)
			}
			return ex, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
	}
	if inv.Kind != KindInvoice {
		return nil, false, fmt.Errorf("%w: payments are recorded against invoices, not credit notes", ErrConflict)
	}
	switch inv.Status {
	case StApproved, StSent, StPartiallyPaid:
	default:
		return nil, false, fmt.Errorf("%w: invoice is %s and cannot take a payment", ErrConflict, inv.Status)
	}
	if in.Currency != "" && in.Currency != inv.Currency {
		return nil, false, invalid("currency", "payments are recorded in the invoice currency %s", inv.Currency)
	}
	if in.AmountMinor > inv.BalanceMinor {
		return nil, false, invalid("amount_minor", "overpayment: the balance is %s %s", FormatMinor(inv.BalanceMinor, inv.Currency), inv.Currency)
	}
	received := in.ReceivedOn
	if received == "" {
		received = dateOf(s.now())
	}
	now := s.now()
	res, err := tx.ExecContext(ctx, `INSERT INTO payments (invoice_id, amount_minor, currency, method, reference, received_on, source, external_id, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		invoiceID, in.AmountMinor, inv.Currency, in.Method, in.Reference, received, source, in.ExternalID, actor, ts(now))
	if err != nil {
		return nil, false, err
	}
	pid, _ := res.LastInsertId()
	if err := s.postPaymentTx(ctx, tx, pid, inv, in.AmountMinor, received, in.cashAccount, actor); err != nil {
		return nil, false, err
	}
	paid := inv.PaidMinor + in.AmountMinor
	status, paidAt := StPartiallyPaid, any(nil)
	if inv.TotalMinor-paid-inv.CreditedMinor <= 0 {
		status, paidAt = StPaid, ts(now)
	}
	if err := s.bump(ctx, tx, inv, `paid_minor=?, status=?, paid_at=COALESCE(?, paid_at)`, paid, status, paidAt); err != nil {
		return nil, false, err
	}
	if status == StPaid {
		if err := s.emit(ctx, tx, EventInvoicePaid, invoicePaidData{InvoiceID: inv.ID, Number: deref(inv.Number), PartyID: inv.PartyID, Currency: inv.Currency,
			TotalMinor: inv.TotalMinor, PaidMinor: paid, PaidAt: now.UTC()}); err != nil {
			return nil, false, err
		}
	}
	p, err := scanPayment(tx.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM payments WHERE id = ?`, pid))
	return p, false, err
}

// ListPayments lists an invoice's payments.
func (s *Store) ListPayments(ctx context.Context, a *Principal, invoiceID int64) ([]Payment, error) {
	cancel, rctx := readCtx(ctx)
	defer cancel()
	if _, err := s.loadInvoice(rctx, s.rdb, a, invoiceID); err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(rctx, `SELECT `+paymentCols+` FROM payments WHERE invoice_id = ? ORDER BY id LIMIT 1000`, invoiceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Payment{}
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ImportPayment is one row of a bank or provider export, matched to an invoice by number.
type ImportPayment struct {
	InvoiceNumber string `json:"invoice_number"`
	PaymentInput
}

// ImportResult reports what happened to one imported row.
type ImportResult struct {
	Index     int    `json:"index"`
	Status    string `json:"status"` // created or duplicate
	PaymentID int64  `json:"payment_id"`
}

const maxImport = 200

// ImportPayments records up to 200 payments from an export, all or none. Every row needs an
// external_id, and a row whose external_id was imported before is reported as a duplicate
// instead of being recorded twice, so re-importing the same file is safe.
func (s *Store) ImportPayments(ctx context.Context, a *Principal, rows []ImportPayment, idemKey string) ([]ImportResult, bool, error) {
	if len(rows) == 0 || len(rows) > maxImport {
		return nil, false, invalid("payments", "give between 1 and %d rows", maxImport)
	}
	var p problems
	for i := range rows {
		if err := rows[i].check(); err != nil {
			var ie *InvalidError
			if errors.As(err, &ie) {
				for _, pf := range ie.Problems {
					p.add(fmt.Sprintf("payments[%d].%s", i, pf.Field), "%s", pf.Message)
				}
			}
		}
		if rows[i].ExternalID == "" {
			p.add(fmt.Sprintf("payments[%d].external_id", i), "required for imports")
		}
		if strings.TrimSpace(rows[i].InvoiceNumber) == "" {
			p.add(fmt.Sprintf("payments[%d].invoice_number", i), "required")
		}
	}
	if err := p.err(); err != nil {
		return nil, false, err
	}
	var out []ImportResult
	var replayed bool
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, replayed, err = idemRun(ctx, s, tx, a.Subject, idemKey, "payment.import", rows, func() ([]ImportResult, error) {
			res := make([]ImportResult, 0, len(rows))
			for i, r := range rows {
				var id int64
				var w where
				w.and("(number_text = ?)", r.InvoiceNumber)
				scopeWhere(a, &w, "party_id", "project")
				if err := tx.QueryRowContext(ctx, `SELECT id FROM invoices WHERE `+w.sql(), w.args...).Scan(&id); err != nil {
					return nil, invalid(fmt.Sprintf("payments[%d].invoice_number", i), "no such invoice")
				}
				pay, dup, err := s.recordPaymentTx(ctx, tx, a, a.Subject, id, r.PaymentInput, "import")
				if err != nil {
					var ie *InvalidError
					if errors.As(err, &ie) {
						return nil, invalid(fmt.Sprintf("payments[%d].%s", i, ie.Problems[0].Field), "%s", ie.Problems[0].Message)
					}
					return nil, err
				}
				st := "created"
				if dup {
					st = "duplicate"
				}
				res = append(res, ImportResult{Index: i, Status: st, PaymentID: pay.ID})
			}
			return res, nil
		})
		return err
	})
	return out, replayed, err
}
