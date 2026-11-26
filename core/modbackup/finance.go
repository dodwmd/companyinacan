package modbackup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// holdFinanceMail stops can-finance from mailing customers on its own after a restore
// (specs/062-invoice-email). The restored database does not know what happened since the dump: who
// has paid, who disputed, which reminders already went. Left alone, the service would send as soon
// as it starts, also on a second server used to try a restore out. So reminders are switched off
// until an administrator turns them on again, and messages that were still waiting are cancelled.
// A database from before those tables existed has nothing to hold.
func holdFinanceMail(ctx context.Context, path string, now time.Time) (note string, err error) {
	db, err := open(path, "_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
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
