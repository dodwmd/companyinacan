package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrExists is returned when a unique row already exists.
var ErrExists = errors.New("already exists")

// AdminUser is the operator account.
type AdminUser struct {
	ID           int64
	Username     string
	PasswordHash string
	TOTPSecret   string
	CreatedAt    time.Time
}

// CreateAdmin inserts an admin user.
func (db *DB) CreateAdmin(ctx context.Context, username, passwordHash, totpSecret string) (AdminUser, error) {
	now := db.Now()
	res, err := db.ExecContext(ctx, `INSERT INTO admin_users(username, password_hash, totp_secret, created_at) VALUES (?,?,?,?)`,
		username, passwordHash, totpSecret, now.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return AdminUser{}, ErrExists
		}
		return AdminUser{}, err
	}
	id, _ := res.LastInsertId()
	return AdminUser{ID: id, Username: username, PasswordHash: passwordHash, TOTPSecret: totpSecret, CreatedAt: now}, nil
}

func scanAdmin(row interface{ Scan(...any) error }) (AdminUser, error) {
	var u AdminUser
	var created int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecret, &created)
	u.CreatedAt = time.Unix(created, 0)
	return u, notFound(err)
}

// AdminByUsername looks an admin up by name.
func (db *DB) AdminByUsername(ctx context.Context, username string) (AdminUser, error) {
	return scanAdmin(db.QueryRowContext(ctx, `SELECT id, username, password_hash, totp_secret, created_at FROM admin_users WHERE username=?`, username))
}

// AdminByID looks an admin up by id.
func (db *DB) AdminByID(ctx context.Context, id int64) (AdminUser, error) {
	return scanAdmin(db.QueryRowContext(ctx, `SELECT id, username, password_hash, totp_secret, created_at FROM admin_users WHERE id=?`, id))
}

// SetPasswordHash replaces the stored hash (password change or rehash).
func (db *DB) SetPasswordHash(ctx context.Context, id int64, hash string) error {
	_, err := db.ExecContext(ctx, `UPDATE admin_users SET password_hash=? WHERE id=?`, hash, id)
	return err
}

// AdvanceTOTPStep records a used TOTP time step. It returns false when step is not newer than
// the last accepted one, which is what makes a code single-use (replay guard).
func (db *DB) AdvanceTOTPStep(ctx context.Context, id, step int64) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE admin_users SET totp_last_step=? WHERE id=? AND totp_last_step < ?`, step, id, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Session is a server-side admin session; the cookie carries a random token whose hash is IDHash.
type Session struct {
	IDHash    string
	UserID    int64
	CSRF      string
	IP        string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateSession stores a session.
func (db *DB) CreateSession(ctx context.Context, s Session) error {
	_, err := db.ExecContext(ctx, `INSERT INTO sessions(id_hash,user_id,csrf_token,ip,created_at,expires_at) VALUES (?,?,?,?,?,?)`,
		s.IDHash, s.UserID, s.CSRF, s.IP, s.CreatedAt.Unix(), s.ExpiresAt.Unix())
	return err
}

// SessionByHash loads a session.
func (db *DB) SessionByHash(ctx context.Context, idHash string) (Session, error) {
	var s Session
	var c, e int64
	err := db.QueryRowContext(ctx, `SELECT id_hash,user_id,csrf_token,ip,created_at,expires_at FROM sessions WHERE id_hash=?`, idHash).
		Scan(&s.IDHash, &s.UserID, &s.CSRF, &s.IP, &c, &e)
	s.CreatedAt, s.ExpiresAt = time.Unix(c, 0), time.Unix(e, 0)
	return s, notFound(err)
}

// DeleteSession removes one session.
func (db *DB) DeleteSession(ctx context.Context, idHash string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash=?`, idHash)
	return err
}

// DeleteExpiredSessions removes sessions expired at now and returns how many.
func (db *DB) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LoginAttempt is the failed-login counter for one throttling key.
type LoginAttempt struct {
	Key         string
	Failures    int
	FirstFailed time.Time
	LockedUntil time.Time
}

// GetLoginAttempt returns the counter for key (zero value when none).
func (db *DB) GetLoginAttempt(ctx context.Context, key string) (LoginAttempt, error) {
	a := LoginAttempt{Key: key}
	var f, l int64
	err := db.QueryRowContext(ctx, `SELECT failures, first_failure, locked_until FROM login_attempts WHERE key=?`, key).Scan(&a.Failures, &f, &l)
	if errors.Is(err, errNoRows) {
		return a, nil
	}
	a.FirstFailed, a.LockedUntil = time.Unix(f, 0), time.Unix(l, 0)
	return a, err
}

// PutLoginAttempt upserts the counter.
func (db *DB) PutLoginAttempt(ctx context.Context, a LoginAttempt) error {
	_, err := db.ExecContext(ctx, `INSERT INTO login_attempts(key,failures,first_failure,locked_until) VALUES (?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET failures=excluded.failures, first_failure=excluded.first_failure, locked_until=excluded.locked_until`,
		a.Key, a.Failures, a.FirstFailed.Unix(), a.LockedUntil.Unix())
	return err
}

// ClearLoginAttempt resets a counter after a successful login.
func (db *DB) ClearLoginAttempt(ctx context.Context, key string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM login_attempts WHERE key=?`, key)
	return err
}
