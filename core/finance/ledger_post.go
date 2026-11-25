package finance

// Automatic posting: finance events become journal entries inside the same transaction as the event, so
// a document and its entry commit or roll back together. Posting is on only when every account mapping is
// set (GetMappings); until then nothing is posted and finance behaves exactly as before E15c. When it is
// on and a posting cannot be made (the ledger is closed for the date, a mapped account was deactivated)
// the whole operation fails with that reason rather than leave a document without its entry.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// autoPoster runs fn with the mapping when automatic posting is on.
func (s *Store) autoPost(ctx context.Context, tx dbq, fn func(m mapIDs) error) error {
	m, enabled, err := s.loadMappings(ctx, tx)
	if err != nil || !enabled {
		return err
	}
	return fn(m)
}

func (s *Store) entryExists(ctx context.Context, tx dbq, source, ref string) (int64, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM journal_entries WHERE source = ? AND source_ref = ?`, source, ref).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// postInvoiceTx posts an approved invoice or credit note: receivable against income and tax payable. The
// income and tax lines come from the document's own rounded lines, so the entry always equals the document.
func (s *Store) postInvoiceTx(ctx context.Context, tx dbq, v *Invoice, number string, lines []Line, actor string) error {
	return s.autoPost(ctx, tx, func(m mapIDs) error {
		var income int64
		tax := map[string]int64{}
		var total int64
		for _, l := range lines {
			income += l.TotalMinor - l.TaxMinor
			total += l.TotalMinor
			if l.TaxMinor > 0 {
				tax[l.TaxCode] += l.TaxMinor
			}
		}
		if total <= 0 {
			return nil
		}
		type amt struct {
			acct int64
			v    int64
		}
		var credits []amt // the side opposite the receivable
		if income > 0 {
			credits = append(credits, amt{m["income"], income})
		}
		for _, code := range sortedKeys(tax) {
			acct := m["tax_payable"]
			if code != "" {
				err := tx.QueryRowContext(ctx, `SELECT id FROM ledger_accounts WHERE tax_code = ? AND type = 'liability' AND active = 1 ORDER BY id LIMIT 1`, code).Scan(&acct)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if errors.Is(err, sql.ErrNoRows) {
					acct = m["tax_payable"]
				}
			}
			credits = append(credits, amt{acct, tax[code]})
		}
		// Merge lines that hit one account so the entry stays short.
		merged := map[int64]int64{}
		var order []int64
		for _, c := range credits {
			if _, ok := merged[c.acct]; !ok {
				order = append(order, c.acct)
			}
			merged[c.acct] += c.v
		}
		sp := postSpec{Source: v.Kind, Ref: fmt.Sprintf("invoice:%d", v.ID), Date: v.IssueDate, Currency: v.Currency, By: actor}
		if v.Kind == KindCreditNote {
			sp.Memo = "Credit note " + number
			sp.Lines = append(sp.Lines, lineSpec{AccountID: m["ar"], Credit: total})
			for _, id := range order {
				sp.Lines = append(sp.Lines, lineSpec{AccountID: id, Debit: merged[id]})
			}
		} else {
			sp.Memo = "Invoice " + number
			sp.Lines = append(sp.Lines, lineSpec{AccountID: m["ar"], Debit: total})
			for _, id := range order {
				sp.Lines = append(sp.Lines, lineSpec{AccountID: id, Credit: merged[id]})
			}
		}
		_, err := s.postEntryTx(ctx, tx, sp)
		return err
	})
}

// unpostInvoiceTx reverses the entry of an invoice that is voided, dated today.
func (s *Store) unpostInvoiceTx(ctx context.Context, tx dbq, v *Invoice, actor string) error {
	id, ok, err := s.entryExists(ctx, tx, KindInvoice, fmt.Sprintf("invoice:%d", v.ID))
	if err != nil || !ok {
		return err
	}
	orig, err := s.loadEntry(ctx, tx, id)
	if err != nil {
		return err
	}
	date := dateOf(s.now())
	if date < orig.EntryDate {
		date = orig.EntryDate
	}
	_, err = s.reverseTx(ctx, tx, orig, date, "Invoice "+deref(v.Number)+" voided", actor)
	return err
}

// postPaymentTx posts money received: bank or cash against receivable. cash is the ledger account of the
// bank account the money arrived in; zero means the mapped default.
func (s *Store) postPaymentTx(ctx context.Context, tx dbq, paymentID int64, inv *Invoice, amount int64, date string, cash int64, actor string) error {
	return s.autoPost(ctx, tx, func(m mapIDs) error {
		if cash == 0 {
			cash = m["cash"]
		}
		_, err := s.postEntryTx(ctx, tx, postSpec{Source: "payment", Ref: fmt.Sprintf("payment:%d", paymentID), Date: date, Currency: inv.Currency,
			Memo: "Payment for " + deref(inv.Number), By: actor,
			Lines: []lineSpec{{AccountID: cash, Debit: amount}, {AccountID: m["ar"], Credit: amount}}})
		return err
	})
}

// postExpenseTx posts an approved expense: the expense against what is owed to the claimant.
func (s *Store) postExpenseTx(ctx context.Context, tx dbq, e *Expense, actor string) error {
	return s.autoPost(ctx, tx, func(m mapIDs) error {
		_, err := s.postEntryTx(ctx, tx, postSpec{Source: "expense", Ref: fmt.Sprintf("expense:%d", e.ID), Date: e.IncurredOn, Currency: e.Currency,
			Memo: fmt.Sprintf("Expense %d (%s)", e.ID, e.Category), By: actor,
			Lines: []lineSpec{{AccountID: m["expense"], Debit: e.AmountMinor}, {AccountID: m["expense_payable"], Credit: e.AmountMinor}}})
		return err
	})
}

// postReimbursementTx posts paying an expense back, only when the approval itself was posted.
func (s *Store) postReimbursementTx(ctx context.Context, tx dbq, e *Expense, actor string) error {
	return s.autoPost(ctx, tx, func(m mapIDs) error {
		if _, ok, err := s.entryExists(ctx, tx, "expense", fmt.Sprintf("expense:%d", e.ID)); err != nil || !ok {
			return err
		}
		date := dateOf(s.now())
		if date < e.IncurredOn {
			date = e.IncurredOn
		}
		_, err := s.postEntryTx(ctx, tx, postSpec{Source: "expense_reimbursed", Ref: fmt.Sprintf("expense:%d", e.ID), Date: date, Currency: e.Currency,
			Memo: fmt.Sprintf("Expense %d reimbursed", e.ID), By: actor,
			Lines: []lineSpec{{AccountID: m["expense_payable"], Debit: e.AmountMinor}, {AccountID: m["cash"], Credit: e.AmountMinor}}})
		return err
	})
}
