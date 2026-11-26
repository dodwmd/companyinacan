package finance

import (
	"context"
	"database/sql"
	"errors"
)

// Reminder events (specs/060-reminders). They carry identifiers, dates and counts only; can-core
// turns them into notifications according to the rules in modules/finance/module.yaml.
const (
	EventInvoiceOverdue     = "invoice.overdue.v1"
	EventContractDeclined   = "contract.declined.v1"
	EventExpenseSubmitted   = "expense.submitted.v1"
	EventExpenseDecided     = "expense.decided.v1"
	EventTimesheetSubmitted = "timesheet.submitted.v1"
	EventTimesheetDecided   = "timesheet.decided.v1"
)

// summaryAbove is the most reminders one sweep raises one by one. Beyond it (the first sweep after an
// upgrade, or after the module was down for a long time) a single event with a count stands for all of
// them, so that a backlog cannot use up the notification and e-mail allowances.
const summaryAbove = 5

type invoiceOverdueData struct {
	InvoiceID   int64  `json:"invoice_id,omitempty"`
	PartyID     int64  `json:"party_id,omitempty"`
	DueDate     string `json:"due_date,omitempty"`
	DaysOverdue int    `json:"days_overdue,omitempty"`
	Count       int    `json:"count,omitempty"`
}

type contractDeclinedData struct {
	ContractID int64 `json:"contract_id"`
	PartyID    int64 `json:"party_id"`
	Version    int64 `json:"version"`
}

type expenseStepData struct {
	ExpenseID int64  `json:"expense_id"`
	Owner     string `json:"owner"`
}

type timesheetStepData struct {
	TimesheetID int64  `json:"timesheet_id"`
	Owner       string `json:"owner"`
}

// unannounced selects the issued invoices with something still to pay whose due date has passed and
// has not been announced; the argument is the date of the sweep.
const unannounced = `FROM invoices i WHERE i.kind = 'invoice' AND i.status IN ('approved', 'sent', 'partially_paid')
	AND i.total_minor - i.paid_minor - i.credited_minor > 0 AND i.due_date < ?
	AND NOT EXISTS (SELECT 1 FROM overdue_notices n WHERE n.invoice_id = i.id AND n.due_date = i.due_date)`

// SweepOverdue announces the invoices that are overdue as of a date and returns how many it covered.
// The mark and the event commit together, so each invoice is announced once. With events off it does
// nothing, so that nothing is marked as announced that nobody could hear.
func (s *Store) SweepOverdue(ctx context.Context, asOf string) (int, error) {
	if !validDate(asOf) {
		return 0, invalid("as_of", "use YYYY-MM-DD")
	}
	if !s.events {
		return 0, nil
	}
	covered := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		covered = 0
		rows, err := tx.QueryContext(ctx, `SELECT i.id, i.party_id, i.due_date `+unannounced+` ORDER BY i.id LIMIT ?`, asOf, summaryAbove+1)
		if err != nil {
			return err
		}
		var due []invoiceOverdueData
		for rows.Next() {
			var d invoiceOverdueData
			if err := rows.Scan(&d.InvoiceID, &d.PartyID, &d.DueDate); err != nil {
				_ = rows.Close()
				return err
			}
			due = append(due, d)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		const mark = `INSERT INTO overdue_notices (invoice_id, due_date, notified_at) `
		const again = ` ON CONFLICT (invoice_id) DO UPDATE SET due_date = excluded.due_date, notified_at = excluded.notified_at`
		now := ts(s.now())
		if len(due) > summaryAbove {
			res, err := tx.ExecContext(ctx, mark+`SELECT i.id, i.due_date, ? `+unannounced+again, now, asOf)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			covered = int(n)
			return s.emit(ctx, tx, EventInvoiceOverdue, invoiceOverdueData{Count: covered})
		}
		for _, d := range due {
			if _, err := tx.ExecContext(ctx, mark+`VALUES (?, ?, ?)`+again, d.InvoiceID, d.DueDate, now); err != nil {
				return err
			}
			d.DaysOverdue = int(parseDate(asOf).Sub(parseDate(d.DueDate)).Hours() / 24)
			if err := s.emit(ctx, tx, EventInvoiceOverdue, d); err != nil {
				return err
			}
			covered++
		}
		return nil
	})
	return covered, err
}
