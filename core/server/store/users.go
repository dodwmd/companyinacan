package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Account is an admin user as the user administration lists it. It carries no credential.
type Account struct {
	ID       int64
	Username string
	Email    string
	// EmailSetBy is the administrator who stored Email.
	EmailSetBy string
	AuthSource string
	CreatedAt  time.Time
	DisabledAt time.Time // zero while enabled
	// Pending: a local account that cannot sign in with a password until its one-time link is used.
	Pending bool
	// Linked: an external (single sign-on) identity is bound to the account.
	Linked bool
}

// SetupLink is the pending one-time link of an account.
type SetupLink struct {
	UserID     int64
	TokenHash  string
	TOTPSecret string
	Purpose    string
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// Purposes of a set-up link.
const (
	LinkInvite = "invite"
	LinkReset  = "reset"
	LinkMFA    = "mfa"
)

// UnusedPassword is the password hash of an account without a password: it never verifies.
const UnusedPassword = "!"

// ErrDeletedName means the name belonged to a deleted account and is never given out again.
var ErrDeletedName = errors.New("that user name belonged to a deleted account")

// usableAccount is true for an admin_users row aliased u that can sign in: enabled, and with local
// credentials or an external identity. Only such an account counts when the last owner is protected.
const usableAccount = `(u.disabled_at=0 AND (u.auth_source='sso' OR (u.password_hash<>'!' AND u.totp_secret<>'')
	OR EXISTS (SELECT 1 FROM sso_identities si WHERE si.user_id=u.id)))`

// OtherUsableOwner reports, inside tx, whether an account other than subject holds the unscoped
// owner role and can sign in.
func OtherUsableOwner(ctx context.Context, tx *sql.Tx, subject string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rbac_assignments a JOIN admin_users u ON a.subject='user:'||u.username
		WHERE a.role='owner' AND a.scope='' AND a.subject<>? AND `+usableAccount, subject).Scan(&n)
	return n > 0, err
}

// IsUsable reports, inside tx, whether the named account can sign in.
func IsUsable(ctx context.Context, tx *sql.Tx, username string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users u WHERE u.username=? AND `+usableAccount, username).Scan(&n)
	return n > 0, err
}

// HoldsOwner reports, inside tx, whether subject holds the unscoped owner role.
func HoldsOwner(ctx context.Context, tx *sql.Tx, subject string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rbac_assignments WHERE subject=? AND role='owner' AND scope=''`, subject).Scan(&n)
	return n > 0, err
}

// EndAccess ends, inside tx, what a subject holds outside the permission checks: personal access
// tokens, module sessions and launch tickets and, unless keepSessions, admin UI sessions.
func EndAccess(ctx context.Context, tx *sql.Tx, subject string, now time.Time, keepSessions bool) error {
	if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at=? WHERE subject=? AND revoked_at=0`, now.Unix(), subject); err != nil {
		return err
	}
	if !keepSessions {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id IN (SELECT id FROM admin_users WHERE username=?)`, strings.TrimPrefix(subject, "user:")); err != nil {
			return err
		}
	}
	return endLaunchAccess(ctx, tx, subject, now)
}

// EndSessions ends, inside tx, a subject's admin sessions and what they launched into modules.
func EndSessions(ctx context.Context, tx *sql.Tx, subject string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id IN (SELECT id FROM admin_users WHERE username=?)`, strings.TrimPrefix(subject, "user:")); err != nil {
		return err
	}
	return endLaunchAccess(ctx, tx, subject, now)
}

// EndPending removes, inside tx, a subject's pending set-up link and its approval delegations (given
// and received).
func EndPending(ctx context.Context, tx *sql.Tx, subject string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_invites WHERE user_id IN (SELECT id FROM admin_users WHERE username=?)`, strings.TrimPrefix(subject, "user:")); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE approval_delegations SET revoked=1 WHERE (delegator=? OR delegate=?) AND revoked=0`, subject, subject)
	return err
}

const accountCols = `u.id, u.username, u.email, u.email_set_by, u.auth_source, u.created_at, u.disabled_at,
	(u.auth_source='local' AND (u.password_hash='!' OR u.totp_secret='')),
	EXISTS (SELECT 1 FROM sso_identities si WHERE si.user_id=u.id)`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var created, disabled int64
	err := row.Scan(&a.ID, &a.Username, &a.Email, &a.EmailSetBy, &a.AuthSource, &created, &disabled, &a.Pending, &a.Linked)
	a.CreatedAt, a.DisabledAt = time.Unix(created, 0), timeOrZero(disabled)
	return a, notFound(err)
}

// Accounts lists up to limit accounts by name.
func (db *DB) Accounts(ctx context.Context, limit int) ([]Account, error) {
	if limit < 1 || limit > 500 {
		limit = 500
	}
	rows, err := db.QueryContext(ctx, `SELECT `+accountCols+` FROM admin_users u ORDER BY u.username LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AccountByName looks one account up.
func (db *DB) AccountByName(ctx context.Context, username string) (Account, error) {
	return scanAccount(db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM admin_users u WHERE u.username=?`, username))
}

// AccountTx is AccountByName inside tx.
func AccountTx(ctx context.Context, tx *sql.Tx, username string) (Account, error) {
	return scanAccount(tx.QueryRowContext(ctx, `SELECT `+accountCols+` FROM admin_users u WHERE u.username=?`, username))
}

// InsertPendingAccount creates, inside tx, a local account with no credentials and no role. Names are
// unique regardless of case and the name of a deleted account is refused.
func (db *DB) InsertPendingAccount(ctx context.Context, tx *sql.Tx, username string) (int64, error) {
	if strings.HasPrefix(strings.ToLower(username), SSOUsernamePrefix) {
		return 0, ErrReservedName
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users WHERE lower(username)=lower(?)`, username).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, ErrExists
	}
	if err := nameNotDeleted(ctx, tx, username); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO admin_users(username, password_hash, totp_secret, created_at) VALUES (?,?,'',?)`,
		username, UnusedPassword, db.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nameNotDeleted(ctx context.Context, tx *sql.Tx, username string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM deleted_users WHERE username=?`, username).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrDeletedName
	}
	return nil
}

// PutSetupLink replaces, inside tx, the account's pending link.
func PutSetupLink(ctx context.Context, tx *sql.Tx, l SetupLink) error {
	_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO user_invites(user_id, token_hash, totp_secret, purpose, created_by, created_at, expires_at, failures) VALUES (?,?,?,?,?,?,?,0)`,
		l.UserID, l.TokenHash, l.TOTPSecret, l.Purpose, l.CreatedBy, l.CreatedAt.Unix(), l.ExpiresAt.Unix())
	return err
}

// SetupLinkByHash finds an unexpired link with a password attempt left and the account it is for. A
// disabled account has none.
func (db *DB) SetupLinkByHash(ctx context.Context, hash string, maxAttempts int, now time.Time) (SetupLink, AdminUser, error) {
	var l SetupLink
	var created, expires int64
	err := db.QueryRowContext(ctx, `SELECT i.user_id, i.token_hash, i.totp_secret, i.purpose, i.created_by, i.created_at, i.expires_at
		FROM user_invites i JOIN admin_users u ON u.id=i.user_id WHERE i.token_hash=? AND i.expires_at>? AND i.failures<? AND u.disabled_at=0`, hash, now.Unix(), maxAttempts).
		Scan(&l.UserID, &l.TokenHash, &l.TOTPSecret, &l.Purpose, &l.CreatedBy, &created, &expires)
	if err != nil {
		return SetupLink{}, AdminUser{}, notFound(err)
	}
	l.CreatedAt, l.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	u, err := db.AdminByID(ctx, l.UserID)
	return l, u, err
}

// SetupLinkOf returns the account's unexpired link (without its secrets), or ErrNotFound.
func (db *DB) SetupLinkOf(ctx context.Context, userID int64, now time.Time) (SetupLink, error) {
	l := SetupLink{UserID: userID}
	var created, expires int64
	err := db.QueryRowContext(ctx, `SELECT purpose, created_by, created_at, expires_at FROM user_invites WHERE user_id=? AND expires_at>?`, userID, now.Unix()).
		Scan(&l.Purpose, &l.CreatedBy, &created, &expires)
	l.CreatedAt, l.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	return l, notFound(err)
}

// ReserveSetupAttempt takes one of the link's password attempts, before the password is checked: one
// statement, so attempts running side by side cannot get more than max between them. used is how many
// are now taken; ok is false when none was left (or the link is gone).
func (db *DB) ReserveSetupAttempt(ctx context.Context, hash string, max int) (used int, ok bool, err error) {
	err = db.QueryRowContext(ctx, `UPDATE user_invites SET failures=failures+1 WHERE token_hash=? AND failures<? RETURNING failures`, hash, max).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return used, err == nil, err
}

// ReleaseSetupAttempt gives back an attempt whose password was never checked.
func (db *DB) ReleaseSetupAttempt(ctx context.Context, hash string) error {
	_, err := db.ExecContext(ctx, `UPDATE user_invites SET failures=failures-1 WHERE token_hash=? AND failures>0`, hash)
	return err
}

// DeleteSetupLink removes a link that has no attempt left.
func (db *DB) DeleteSetupLink(ctx context.Context, hash string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM user_invites WHERE token_hash=?`, hash)
	return err
}

// ConsumeSetupLink uses a link once, inside tx: it deletes the link and sets the account's
// credentials, ending its sessions. totpStep marks the enrolment code as used. ErrNotFound means the
// link was used, replaced or expired meanwhile.
func ConsumeSetupLink(ctx context.Context, tx *sql.Tx, hash, passwordHash string, totpStep int64, now time.Time) error {
	var userID int64
	var secret string
	err := tx.QueryRowContext(ctx, `DELETE FROM user_invites WHERE token_hash=? AND expires_at>?
		AND user_id IN (SELECT id FROM admin_users WHERE disabled_at=0) RETURNING user_id, totp_secret`, hash, now.Unix()).Scan(&userID, &secret)
	if err != nil {
		return notFound(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE admin_users SET password_hash=?, totp_secret=?, totp_last_step=? WHERE id=?`, passwordHash, secret, totpStep, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	return endLaunchAccessByUserID(ctx, tx, userID, now)
}

// ClearCredentials removes, inside tx, a local account's code and, unless keepPassword, its password.
func ClearCredentials(ctx context.Context, tx *sql.Tx, userID int64, keepPassword bool) error {
	q := `UPDATE admin_users SET totp_secret='', totp_last_step=0, password_hash='!' WHERE id=? AND auth_source='local'`
	if keepPassword {
		q = `UPDATE admin_users SET totp_secret='', totp_last_step=0 WHERE id=? AND auth_source='local'`
	}
	_, err := tx.ExecContext(ctx, q, userID)
	return err
}

// SetEmail stores, inside tx, where the account's links are mailed and who said so.
func SetEmail(ctx context.Context, tx *sql.Tx, userID int64, email, setBy string) error {
	_, err := tx.ExecContext(ctx, `UPDATE admin_users SET email=?, email_set_by=? WHERE id=?`, email, setBy, userID)
	return err
}

// ClearForeignEmail forgets, inside tx, the account's address unless actor stored it.
func ClearForeignEmail(ctx context.Context, tx *sql.Tx, username, actor string) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE admin_users SET email='', email_set_by='' WHERE username=? AND email<>'' AND email_set_by<>?`, username, actor)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteAccount removes, inside tx, an account and everything keyed on its subject, and keeps its
// name from being used again. The audit log, approval history and revoked token records stay.
func (db *DB) DeleteAccount(ctx context.Context, tx *sql.Tx, a Account) error {
	subject, now := "user:"+a.Username, db.Now()
	if err := EndAccess(ctx, tx, subject, now, false); err != nil {
		return err
	}
	if err := EndPending(ctx, tx, subject); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM rbac_assignments WHERE subject=?`,
		`DELETE FROM push_subscriptions WHERE subject=?`,
		`DELETE FROM notifications WHERE subject=?`,
		`DELETE FROM notify_prefs WHERE subject=?`,
		`DELETE FROM notify_settings WHERE subject=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, subject); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM login_attempts WHERE key LIKE ? ESCAPE '\' OR key=?`,
		escapeLike("user:"+strings.ToLower(a.Username)+"|")+"%", "acct:"+a.Username); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM admin_users WHERE id=?`, a.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO deleted_users(username, deleted_at) VALUES (?,?)`, a.Username, now.Unix())
	return err
}

// SetSessionReauth records a step-up on one session.
func (db *DB) SetSessionReauth(ctx context.Context, idHash string, now time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE sessions SET reauth_at=? WHERE id_hash=?`, now.Unix(), idHash)
	return err
}
