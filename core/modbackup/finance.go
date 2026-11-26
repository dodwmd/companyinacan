package modbackup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// holdFinanceMail stops can-finance from mailing customers on its own after a restore
// (specs/062-invoice-email). The restored database does not know what happened since the dump: who
// has paid, who disputed, which reminders already went. Left alone, the service would send as soon
// as it starts, also on a second server used to try a restore out. So reminders are switched off
// until an administrator turns them on again, and messages that were still waiting are cancelled.
// A database from before those tables existed has nothing to hold.
//
// The staff reminders (specs/060-reminders) are held too, see holdFinanceReminders.
func holdFinanceMail(ctx context.Context, path string, now time.Time) (note string, err error) {
	db, err := open(path, "_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	held, err := holdFinanceReminders(ctx, db, now)
	if err != nil {
		return "", err
	}
	defer func() {
		if err == nil && held != "" {
			note = strings.TrimSpace(note + " " + held)
		}
	}()
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('dunning_policy', 'document_mails')`).Scan(&tables); err != nil {
		return "", err
	}
	if tables != 2 {
		return "", nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	count := func(res sql.Result, err error) (int64, error) {
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	off, err := count(tx.ExecContext(ctx, `UPDATE dunning_policy SET enabled = 0, restored_at = ?, updated_by = 'can-restore', updated_at = ? WHERE enabled = 1`, now.Unix(), now.Unix()))
	if err != nil {
		return "", err
	}
	cancelled, err := count(tx.ExecContext(ctx, `UPDATE document_mails SET status = 'cancelled', updated_at = ? WHERE status IN ('pending', 'retry')`, now.Unix()))
	if err != nil {
		return "", err
	}
	if off+cancelled == 0 {
		return "", nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (actor, action, target, detail, at) VALUES ('can-restore', 'dunning.restore', '', ?, ?)`,
		fmt.Sprintf(`{"reminders_switched_off":%t,"messages_cancelled":%d}`, off > 0, cancelled), now.Unix()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if off > 0 {
		note = "NOTE: payment reminders to customers are now OFF. Record the payments and disputes made since the dump, then turn them on again (finance: Settings, Payment reminders). "
	}
	return note + fmt.Sprintf("%d waiting customer message(s) cancelled.", cancelled), nil
}

// holdFinanceReminders marks what is already overdue or due for renewal as announced, so that a
// restored server does not tell the staff about all of it again (or, from a second server used to try
// a restore out, at all). What falls due afterwards is announced as usual. A dump from before these
// reminders existed is left alone: its first sweep summarises, as after an upgrade.
func holdFinanceReminders(ctx context.Context, db *sql.DB, now time.Time) (string, error) {
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'overdue_notices'`).Scan(&tables); err != nil || tables == 0 {
		return "", err
	}
	today, at := now.UTC().Format("2006-01-02"), now.UTC().Format(time.RFC3339Nano)
	// The same invoices as can-finance's overdue sweep selects.
	res, err := db.ExecContext(ctx, `INSERT INTO overdue_notices (invoice_id, due_date, notified_at)
		SELECT i.id, i.due_date, ? FROM invoices i WHERE i.kind = 'invoice' AND i.status IN ('approved', 'sent', 'partially_paid')
			AND i.total_minor - i.paid_minor - i.credited_minor > 0 AND i.due_date < ?
		ON CONFLICT (invoice_id) DO UPDATE SET due_date = excluded.due_date, notified_at = excluded.notified_at
			WHERE overdue_notices.due_date <> excluded.due_date`, at, today)
	if err != nil {
		return "", err
	}
	invoices, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	res, err = db.ExecContext(ctx, `UPDATE contracts SET reminded_for = renewal_date WHERE status IN ('signed', 'active') AND renewal_date IS NOT NULL
		AND reminded_for <> renewal_date AND date(renewal_date, '-' || reminder_days || ' days') <= ?`, today)
	if err != nil {
		return "", err
	}
	contracts, err := res.RowsAffected()
	if err != nil || invoices+contracts == 0 {
		return "", err
	}
	return fmt.Sprintf("Staff reminders for %d overdue invoice(s) and %d contract renewal(s) already due are not repeated: check them in finance.", invoices, contracts), nil
}
