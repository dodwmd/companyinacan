package finance

import (
	"context"
	"database/sql"
	"time"
)

const idempotencyRetention = 7 * 24 * time.Hour

// PurgeReport counts what a purge removed.
type PurgeReport struct {
	Invoices       int64 `json:"invoices"`
	Expenses       int64 `json:"expenses"`
	Timesheets     int64 `json:"timesheets"`
	Contracts      int64 `json:"contracts"`
	PurchaseOrders int64 `json:"purchase_orders"`
	ErasedParties  int64 `json:"erased_parties"`
	AuditEntries   int64 `json:"audit_entries"`
}

// Purge enforces retention. Financial records are kept for retentionYears from their own
// date (invoice issue date, expense date, week start, contract end, PO creation) and then
// deleted, together with their lines, payments and versions; open invoices (not paid or
// void) are never deleted. Parties that were erased and no longer have any record are
// deleted too: that is the end of the legal retention that kept their snapshot. Audit
// entries older than auditDays go (the database refuses sooner than a year). Expired
// idempotency keys and delivered events are dropped.
func (s *Store) Purge(ctx context.Context, retentionYears, auditDays int) (PurgeReport, error) {
	var rep PurgeReport
	now := s.now()
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// The triggers refuse deletes of issued records; open the gate for this transaction
		// only. It is closed again before commit, and a rollback restores it too.
		if _, err := tx.ExecContext(ctx, `UPDATE retention_gate SET open = 1`); err != nil {
			return err
		}
		if retentionYears > 0 {
			cutoff := now.AddDate(-retentionYears, 0, 0).Format(time.DateOnly)
			exec := func(dst *int64, q string, args ...any) error {
				res, err := tx.ExecContext(ctx, q, args...)
				if err != nil {
					return err
				}
				n, _ := res.RowsAffected()
				*dst += n
				return nil
			}
			var ignore int64
			if err := exec(&rep.Timesheets, `DELETE FROM time_entries WHERE timesheet_id IN (SELECT id FROM timesheets WHERE status = 'approved' AND week_start < ?)`, cutoff); err != nil {
				return err
			}
			rep.Timesheets = 0
			if err := exec(&rep.Timesheets, `DELETE FROM timesheets WHERE status = 'approved' AND week_start < ?`, cutoff); err != nil {
				return err
			}
			if err := exec(&rep.Expenses, `DELETE FROM expenses WHERE status IN ('approved', 'rejected', 'reimbursed') AND incurred_on < ?`, cutoff); err != nil {
				return err
			}
			// Credit notes first (they reference their invoice), then payments, then invoices
			// that no remaining credit note points at.
			if err := exec(&rep.Invoices, `DELETE FROM invoices WHERE kind = 'credit_note' AND status <> 'draft' AND issue_date < ?`, cutoff); err != nil {
				return err
			}
			if err := exec(&ignore, `UPDATE time_entries SET invoice_id = NULL WHERE invoice_id IN (SELECT id FROM invoices WHERE kind = 'invoice' AND status IN ('paid', 'void') AND issue_date < ?)`, cutoff); err != nil {
				return err
			}
			if err := exec(&ignore, `DELETE FROM payments WHERE invoice_id IN (SELECT id FROM invoices WHERE kind = 'invoice' AND status IN ('paid', 'void') AND issue_date < ?
				AND NOT EXISTS (SELECT 1 FROM invoices c WHERE c.original_id = invoices.id))`, cutoff); err != nil {
				return err
			}
			if err := exec(&rep.Invoices, `DELETE FROM invoices WHERE kind = 'invoice' AND status IN ('paid', 'void') AND issue_date < ?
				AND NOT EXISTS (SELECT 1 FROM invoices c WHERE c.original_id = invoices.id)`, cutoff); err != nil {
				return err
			}
			if err := exec(&ignore, `DELETE FROM contract_versions WHERE contract_id IN (SELECT id FROM contracts WHERE status IN ('expired', 'terminated') AND COALESCE(end_date, date(terminated_at, 'unixepoch'), date(updated_at, 'unixepoch')) < ?)`, cutoff); err != nil {
				return err
			}
			if err := exec(&rep.Contracts, `DELETE FROM contracts WHERE status IN ('expired', 'terminated') AND COALESCE(end_date, date(terminated_at, 'unixepoch'), date(updated_at, 'unixepoch')) < ?`, cutoff); err != nil {
				return err
			}
			if err := exec(&rep.PurchaseOrders, `DELETE FROM purchase_orders WHERE status IN ('received', 'cancelled') AND date(created_at, 'unixepoch') < ?`, cutoff); err != nil {
				return err
			}
			if err := exec(&rep.ErasedParties, `DELETE FROM parties WHERE erased_at IS NOT NULL
				AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.party_id = parties.id)
				AND NOT EXISTS (SELECT 1 FROM contracts c WHERE c.party_id = parties.id)
				AND NOT EXISTS (SELECT 1 FROM purchase_orders p WHERE p.vendor_id = parties.id)
				AND NOT EXISTS (SELECT 1 FROM recurring r WHERE r.party_id = parties.id)`); err != nil {
				return err
			}
		}
		if auditDays > 0 {
			res, err := tx.ExecContext(ctx, `DELETE FROM audit_log WHERE at < ?`, ts(now.AddDate(0, 0, -auditDays)))
			if err != nil {
				return err
			}
			rep.AuditEntries, _ = res.RowsAffected()
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency WHERE created_at < ?`, ts(now.Add(-idempotencyRetention))); err != nil {
			return err
		}
		if err := s.purgeOutbox(ctx, tx, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE retention_gate SET open = 0`)
		return err
	})
	if err == nil && (rep.Invoices+rep.Expenses+rep.Timesheets+rep.Contracts+rep.PurchaseOrders+rep.ErasedParties+rep.AuditEntries) > 0 {
		err = s.Scrub(ctx)
	}
	return rep, err
}
