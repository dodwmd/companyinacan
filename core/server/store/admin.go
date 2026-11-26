package store

import (
	"context"
	"database/sql"
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
	// AuthSource is SourceLocal (password and TOTP) or SourceSSO (OpenID Connect, epic E11b).
	AuthSource string
}

// SSOUsernamePrefix is reserved for users provisioned by single sign-on: a local account may not take
// a name that a future sign-in could be provisioned under.
const SSOUsernamePrefix = "sso-"

// ErrReservedName is returned when a local account is created with a reserved name.
var ErrReservedName = errors.New("the user name prefix sso- is reserved for single sign-on users")

// CreateAdmin inserts a local admin user.
func (db *DB) CreateAdmin(ctx context.Context, username, passwordHash, totpSecret string) (AdminUser, error) {
	if strings.HasPrefix(strings.ToLower(username), SSOUsernamePrefix) {
		return AdminUser{}, ErrReservedName
	}
	now := db.Now()
	var id int64
	// The first admin of an installation is its owner (break-glass role, specs/015); later admins
	// start without any role and are given roles explicitly.
	err := db.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n); err != nil {
			return err
		}
		if err := nameNotDeleted(ctx, tx, username); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO admin_users(username, password_hash, totp_secret, created_at) VALUES (?,?,?,?)`,
			username, passwordHash, totpSecret, now.Unix())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		if n == 0 {
			_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO rbac_assignments(subject, role, scope, granted_by, granted_at) VALUES (?,?,?,?,?)`,
				"user:"+username, "owner", "", "bootstrap", now.Unix())
		}
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return AdminUser{}, ErrExists
		}
		return AdminUser{}, err
	}
	return AdminUser{ID: id, Username: username, PasswordHash: passwordHash, TOTPSecret: totpSecret, CreatedAt: now, AuthSource: SourceLocal}, nil
}

func scanAdmin(row interface{ Scan(...any) error }) (AdminUser, error) {
	var u AdminUser
	var created int64
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TOTPSecret, &created, &u.AuthSource)
	u.CreatedAt = time.Unix(created, 0)
	return u, notFound(err)
}

// AdminByUsername looks an admin up by name.
func (db *DB) AdminByUsername(ctx context.Context, username string) (AdminUser, error) {
	return scanAdmin(db.QueryRowContext(ctx, `SELECT id, username, password_hash, totp_secret, created_at, auth_source FROM admin_users WHERE username=?`, username))
}

// AdminByID looks an admin up by id.
func (db *DB) AdminByID(ctx context.Context, id int64) (AdminUser, error) {
	return scanAdmin(db.QueryRowContext(ctx, `SELECT id, username, password_hash, totp_secret, created_at, auth_source FROM admin_users WHERE id=?`, id))
}

// AdminDisabled reports whether the account is disabled (the person left). Unknown ids count as
// disabled: fail closed.
func (db *DB) AdminDisabled(ctx context.Context, id int64) bool {
	disabled, err := db.AdminDisabledState(ctx, id)
	return disabled || err != nil
}

// AdminDisabledState is AdminDisabled for callers that must tell "disabled" from "cannot tell".
func (db *DB) AdminDisabledState(ctx context.Context, id int64) (bool, error) {
	var d int64
	if err := db.QueryRowContext(ctx, `SELECT disabled_at FROM admin_users WHERE id=?`, id).Scan(&d); err != nil {
		return false, err
	}
	return d != 0, nil
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

// Authentication sources of an admin user and its sessions.
const (
	SourceLocal = "local"
	SourceSSO   = "sso"
)

// Session is a server-side admin session; the cookie carries a random token whose hash is IDHash.
type Session struct {
	IDHash    string
	UserID    int64
	CSRF      string
	IP        string
	CreatedAt time.Time
	ExpiresAt time.Time
	// Source is SourceLocal or SourceSSO. RevalidateAt (zero = never) is when an SSO session stops
	// being accepted so that sign-in is repeated at the identity provider.
	Source       string
	RevalidateAt time.Time
	// AuthAt is the identity provider's auth_time of an SSO session (zero = it did not say). Read
	// it through AuthenticatedAt.
	AuthAt time.Time
	// ReauthAt is when the person last confirmed their password and code again (zero = never).
	ReauthAt time.Time
}

// ProvedAt is the latest moment the person behind the session proved who they are: signing in, or a
// later step-up. Zero means not known.
func (s Session) ProvedAt() time.Time {
	if at := s.AuthenticatedAt(); at.After(s.ReauthAt) {
		return at
	}
	return s.ReauthAt
}

// AuthenticatedAt is when the person behind the session last proved who they are: password and TOTP
// for a local session (made at that moment), the identity provider's auth_time for an SSO session.
// Zero means not known.
func (s Session) AuthenticatedAt() time.Time {
	if s.Source == SourceSSO {
		return s.AuthAt
	}
	return s.CreatedAt
}

// CreateSession stores a session.
func (db *DB) CreateSession(ctx context.Context, s Session) error {
	if s.Source == "" {
		s.Source = SourceLocal
	}
	var rv int64
	if !s.RevalidateAt.IsZero() {
		rv = s.RevalidateAt.Unix()
	}
	_, err := db.ExecContext(ctx, `INSERT INTO sessions(id_hash,user_id,csrf_token,ip,created_at,expires_at,source,revalidate_at,auth_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		s.IDHash, s.UserID, s.CSRF, s.IP, s.CreatedAt.Unix(), s.ExpiresAt.Unix(), s.Source, rv, unixOrZero(s.AuthAt))
	return err
}

// SessionByHash loads a session.
func (db *DB) SessionByHash(ctx context.Context, idHash string) (Session, error) {
	var s Session
	var c, e, rv, auth, re int64
	err := db.QueryRowContext(ctx, `SELECT id_hash,user_id,csrf_token,ip,created_at,expires_at,source,revalidate_at,auth_at,reauth_at FROM sessions WHERE id_hash=?`, idHash).
		Scan(&s.IDHash, &s.UserID, &s.CSRF, &s.IP, &c, &e, &s.Source, &rv, &auth, &re)
	s.CreatedAt, s.ExpiresAt, s.AuthAt, s.ReauthAt = time.Unix(c, 0), time.Unix(e, 0), timeOrZero(auth), timeOrZero(re)
	if rv > 0 {
		s.RevalidateAt = time.Unix(rv, 0)
	}
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
