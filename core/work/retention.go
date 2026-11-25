package work

import (
	"context"
	"database/sql"
	"time"
)

const idempotencyRetention = 7 * 24 * time.Hour

// Purge enforces retention: issues resolved more than retentionDays ago are deleted with
// their comments, activity, links and attachments (0 keeps everything), audit entries older
// than auditDays go too, and expired idempotency keys and delivered events are dropped.
// It returns the number of issues deleted.
func (s *Store) Purge(ctx context.Context, retentionDays, auditDays int) (int64, error) {
	now := s.now()
	var deleted int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if retentionDays > 0 {
			res, err := tx.ExecContext(ctx, `DELETE FROM issues WHERE resolved_at IS NOT NULL AND resolved_at < ?`, ts(now.AddDate(0, 0, -retentionDays)))
			if err != nil {
				return err
			}
			deleted, _ = res.RowsAffected()
		}
		if auditDays > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM audit_log WHERE at < ?`, ts(now.AddDate(0, 0, -auditDays))); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency WHERE created_at < ?`, ts(now.Add(-idempotencyRetention))); err != nil {
			return err
		}
		return s.purgeOutbox(ctx, tx, now)
	})
	return deleted, err
}
