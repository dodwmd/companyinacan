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

// Cut-over. The moment automatic posting first turns on, what is already owed is brought into the ledger by
// one opening entry per currency, dated that day, in the same transaction:
//
//	debit  ar              open balances (total - paid - credited) of approved, sent and partly paid invoices
//	credit expense_payable approved expense claims not yet reimbursed
//	credit (or debit) opening_equity the difference
//
// From then on every hook posts for every document, old or new: a payment, credit note, void or reimbursement
// of a document that predates the cut-over relieves the balance the opening entry carried. Cash, tax owed and
// everything else the module cannot know is the operator's own manual journal against opening equity.
func (s *Store) postOpeningTx(ctx context.Context, tx dbq, m mapIDs, actor string) error {
	owed := map[string][2]int64{} // currency: receivable, expense payable
	for i, q := range []string{
		`SELECT currency, SUM(total_minor - paid_minor - credited_minor) FROM invoices WHERE kind = 'invoice' AND status IN ('approved', 'sent', 'partially_paid') GROUP BY currency`,
		`SELECT currency, SUM(amount_minor) FROM expenses WHERE status = 'approved' GROUP BY currency`,
	} {
		rows, err := tx.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		for rows.Next() {
			var cur string
			var n int64
			if err := rows.Scan(&cur, &n); err != nil {
				_ = rows.Close()
				return err
			}
			v := owed[cur]
			v[i] = n
			owed[cur] = v
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
	}
	for _, cur := range sortedKeys(owed) {
		ar, payable := owed[cur][0], owed[cur][1]
		var lines []lineSpec
		if ar > 0 {
			lines = append(lines, lineSpec{AccountID: m["ar"], Debit: ar})
		}
		if payable > 0 {
			lines = append(lines, lineSpec{AccountID: m["expense_payable"], Credit: payable})
		}
		switch net := ar - payable; {
		case net > 0:
			lines = append(lines, lineSpec{AccountID: m["opening_equity"], Credit: net})
		case net < 0:
			lines = append(lines, lineSpec{AccountID: m["opening_equity"], Debit: -net})
		}
		if len(lines) < 2 {
			continue
		}
		if _, err := s.postEntryTx(ctx, tx, postSpec{Source: "opening", Ref: "opening:" + cur, Date: dateOf(s.now()), Currency: cur,
			Memo: "Opening balances at cut-over", By: actor, Lines: lines}); err != nil {
			return err
		}
	}
	return nil
}

// cutOverDate keeps a follow-on posting of a document that predates the cut-over (it has no entry of its own)
// from being dated before the opening entry that carries its balance.
func (s *Store) cutOverDate(ctx context.Context, tx dbq, source, ref, date string) (string, error) {
	if _, ok, err := s.entryExists(ctx, tx, source, ref); err != nil || ok {
		return date, err
	}
	var cut sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT MIN(entry_date) FROM journal_entries WHERE source = 'opening'`).Scan(&cut); err != nil {
		return "", err
	}
	if cut.Valid && date < cut.String {
		return cut.String, nil
	}
	return date, nil
}

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
	memo := "Invoice " + number
	if v.Kind == KindCreditNote {
		memo = "Credit note " + number
	}
	return s.postInvoiceAs(ctx, tx, v, v.Kind, memo, lines, actor)
}

// postInvoiceAs posts an invoice-shaped entry (the direction follows v.Kind) under the given source.
func (s *Store) postInvoiceAs(ctx context.Context, tx dbq, v *Invoice, source, memo string, lines []Line, actor string) error {
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
		sp := postSpec{Source: source, Memo: memo, Ref: fmt.Sprintf("invoice:%d", v.ID), Date: v.IssueDate, Currency: v.Currency, By: actor}
		if v.Kind == KindCreditNote && v.OriginalID != nil {
			var err error
			if sp.Date, err = s.cutOverDate(ctx, tx, KindInvoice, fmt.Sprintf("invoice:%d", *v.OriginalID), sp.Date); err != nil {
				return err
			}
		}
		if v.Kind == KindCreditNote {
			sp.Lines = append(sp.Lines, lineSpec{AccountID: m["ar"], Credit: total})
			for _, id := range order {
				sp.Lines = append(sp.Lines, lineSpec{AccountID: id, Debit: merged[id]})
			}
		} else {
			sp.Lines = append(sp.Lines, lineSpec{AccountID: m["ar"], Debit: total})
			for _, id := range order {
				sp.Lines = append(sp.Lines, lineSpec{AccountID: id, Credit: merged[id]})
			}
		}
		_, err := s.postEntryTx(ctx, tx, sp)
		return err
	})
}

// unpostInvoiceTx takes a voided invoice out of the ledger, dated today: the reversal of its entry, or for an
// invoice that predates the cut-over (its balance came in with the opening entry) the mirror of what its entry
// would have been.
func (s *Store) unpostInvoiceTx(ctx context.Context, tx dbq, v *Invoice, actor string) error {
	return s.autoPost(ctx, tx, func(mapIDs) error {
		id, ok, err := s.entryExists(ctx, tx, KindInvoice, fmt.Sprintf("invoice:%d", v.ID))
		if err != nil {
			return err
		}
		if !ok {
			mirror := *v
			mirror.Kind, mirror.OriginalID, mirror.IssueDate = KindCreditNote, nil, dateOf(s.now())
			return s.postInvoiceAs(ctx, tx, &mirror, "invoice_void", "Voided invoice "+deref(v.Number), v.Lines, actor)
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
	})
}

// postPaymentTx posts money received: bank or cash against receivable. cash is the ledger account of the
// bank account the money arrived in; zero means the mapped default.
func (s *Store) postPaymentTx(ctx context.Context, tx dbq, paymentID int64, inv *Invoice, amount int64, date string, cash int64, actor string) error {
	return s.autoPost(ctx, tx, func(m mapIDs) error {
		if cash == 0 {
			cash = m["cash"]
		}
		date, err := s.cutOverDate(ctx, tx, KindInvoice, fmt.Sprintf("invoice:%d", inv.ID), date)
		if err != nil {
			return err
		}
		_, err = s.postEntryTx(ctx, tx, postSpec{Source: "payment", Ref: fmt.Sprintf("payment:%d", paymentID), Date: date, Currency: inv.Currency,
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

// postReimbursementTx posts paying an expense back. A claim approved before the cut-over has no entry of its
// own: the opening entry carried it, and this relieves it.
func (s *Store) postReimbursementTx(ctx context.Context, tx dbq, e *Expense, actor string) error {
	return s.autoPost(ctx, tx, func(m mapIDs) error {
		date := dateOf(s.now())
		if date < e.IncurredOn {
			date = e.IncurredOn
		}
		date, err := s.cutOverDate(ctx, tx, "expense", fmt.Sprintf("expense:%d", e.ID), date)
		if err != nil {
			return err
		}
		_, err = s.postEntryTx(ctx, tx, postSpec{Source: "expense_reimbursed", Ref: fmt.Sprintf("expense:%d", e.ID), Date: date, Currency: e.Currency,
			Memo: fmt.Sprintf("Expense %d reimbursed", e.ID), By: actor,
			Lines: []lineSpec{{AccountID: m["expense_payable"], Debit: e.AmountMinor}, {AccountID: m["cash"], Credit: e.AmountMinor}}})
		return err
	})
}
