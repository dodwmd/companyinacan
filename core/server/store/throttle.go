package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// RegisterFailure atomically records one failed attempt for key and returns the new state and
// whether this failure established a lock. Failures older than window, or an expired lock, start a
// fresh count. The read-modify-write runs in one transaction.
func (db *DB) RegisterFailure(ctx context.Context, key string, max int, window, lockFor time.Duration, now time.Time) (LoginAttempt, bool, error) {
	var a LoginAttempt
	locked := false
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		a = LoginAttempt{Key: key}
		var f, l int64
		err := tx.QueryRowContext(ctx, `SELECT failures, first_failure, locked_until FROM login_attempts WHERE key=?`, key).Scan(&a.Failures, &f, &l)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		a.FirstFailed, a.LockedUntil = time.Unix(f, 0), time.Unix(l, 0)
		lockExpired := l > 0 && !now.Before(a.LockedUntil)
		if a.Failures == 0 || lockExpired || now.Sub(a.FirstFailed) > window {
			a = LoginAttempt{Key: key, FirstFailed: now}
		}
		a.Failures++
		if a.Failures >= max && !now.Before(a.LockedUntil) {
			a.LockedUntil = now.Add(lockFor)
			locked = true
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO login_attempts(key,failures,first_failure,locked_until) VALUES (?,?,?,?)
			ON CONFLICT(key) DO UPDATE SET failures=excluded.failures, first_failure=excluded.first_failure, locked_until=excluded.locked_until`,
			a.Key, a.Failures, a.FirstFailed.Unix(), a.LockedUntil.Unix())
		return err
	})
	return a, locked, err
}

// PruneLoginAttempts deletes counters whose lock has expired and whose window has passed.
func (db *DB) PruneLoginAttempts(ctx context.Context, now time.Time, window time.Duration) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM login_attempts WHERE locked_until <= ? AND first_failure <= ?`, now.Unix(), now.Add(-window).Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearLoginAttempts removes throttling state: all of it, or only keys for one username
// (`user:<name>|<ip>`) when username is not empty.
func (db *DB) ClearLoginAttempts(ctx context.Context, username string) (int64, error) {
	var res sql.Result
	var err error
	if username == "" {
		res, err = db.ExecContext(ctx, `DELETE FROM login_attempts`)
	} else {
		res, err = db.ExecContext(ctx, `DELETE FROM login_attempts WHERE key LIKE ? ESCAPE '\'`, escapeLike("user:"+strings.ToLower(username)+"|")+"%")
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// SetAdminCredentials replaces the password hash and, when totpSecret is not empty, the TOTP seed
// (resetting the replay guard), and destroys the user's sessions.
func (db *DB) SetAdminCredentials(ctx context.Context, id int64, hash, totpSecret string) error {
	return db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE admin_users SET password_hash=? WHERE id=?`, hash, id); err != nil {
			return err
		}
		if totpSecret != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE admin_users SET totp_secret=?, totp_last_step=0 WHERE id=?`, totpSecret, id); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id)
		return err
	})
}
