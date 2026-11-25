package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// LoginTicket is a launch ticket record (specs/036-module-ui-sso). The ticket itself is never
// stored, only its SHA-256.
type LoginTicket struct {
	ID         string
	TokenHash  string
	Subject    string
	ModuleID   string
	ReturnPath string
	// BindingHash is the SHA-256 of the nonce cookie of the browser that launched.
	BindingHash string
	// NotAfter caps the module session's lifetime (zero = no cap).
	NotAfter time.Time
	// AuthAt is when the launching administrator last authenticated (zero = not known).
	AuthAt    time.Time
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    time.Time // zero while unused
}

// LaunchSession is the module-side session handle of a ticket login. Only its hash is stored.
type LaunchSession struct {
	HandleHash string
	TicketID   string
	Subject    string
	ModuleID   string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  time.Time // zero while valid
	// AuthAt is when the person last authenticated to the admin UI (zero = not known).
	AuthAt time.Time
}

// ErrRevoked means the thing a write depended on was revoked before the write.
var ErrRevoked = errors.New("revoked")

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOrZero(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

// InsertLoginTicket stores a new ticket inside tx.
func (db *DB) InsertLoginTicket(ctx context.Context, tx *sql.Tx, t LoginTicket) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO login_tickets(id,token_hash,subject,module_id,return_path,binding_hash,session_not_after,auth_at,created_at,expires_at,used_at) VALUES (?,?,?,?,?,?,?,?,?,?,0)`,
		t.ID, t.TokenHash, t.Subject, t.ModuleID, t.ReturnPath, t.BindingHash, unixOrZero(t.NotAfter), unixOrZero(t.AuthAt), t.CreatedAt.Unix(), t.ExpiresAt.Unix())
	return err
}

// usable is the condition under which a ticket can still be redeemed by a module from the browser
// that launched it.
const usable = ` token_hash=? AND module_id=? AND binding_hash=? AND used_at=0 AND revoked_at=0 AND expires_at>?`

// RedeemLoginTicket marks the ticket with this hash used, but only when it is unused, unexpired,
// bound to module and presented with the launching browser's nonce: one atomic UPDATE, so of two
// concurrent redeems exactly one wins. ok is false for every other case and the caller cannot tell
// them apart.
func (db *DB) RedeemLoginTicket(ctx context.Context, tx *sql.Tx, hash, module, binding string, now time.Time) (t LoginTicket, ok bool, err error) {
	var created, expires, notAfter, authAt int64
	err = tx.QueryRowContext(ctx, `UPDATE login_tickets SET used_at=? WHERE`+usable+` RETURNING id,subject,module_id,return_path,session_not_after,auth_at,created_at,expires_at`,
		now.Unix(), hash, module, binding, now.Unix()).Scan(&t.ID, &t.Subject, &t.ModuleID, &t.ReturnPath, &notAfter, &authAt, &created, &expires)
	if err == sql.ErrNoRows {
		return LoginTicket{}, false, nil
	}
	if err != nil {
		return LoginTicket{}, false, err
	}
	t.TokenHash, t.CreatedAt, t.ExpiresAt, t.UsedAt = hash, time.Unix(created, 0), time.Unix(expires, 0), now
	t.NotAfter, t.AuthAt = timeOrZero(notAfter), timeOrZero(authAt)
	return t, true, nil
}

// PeekLoginTicket returns the subject of a ticket that RedeemLoginTicket would accept, without
// consuming it.
func (db *DB) PeekLoginTicket(ctx context.Context, hash, module, binding string, now time.Time) (subject string, ok bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT subject FROM login_tickets WHERE`+usable, hash, module, binding, now.Unix()).Scan(&subject)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return subject, err == nil, err
}

// endLaunchAccess ends everything single sign-on into module UIs holds for a subject: sessions that
// began with a ticket, tickets not yet redeemed, and tickets being redeemed right now (consumed, with
// no session stored yet: InsertLaunchSession refuses a revoked ticket). It belongs in the transaction of whatever ends
// or resets the account's sign-in.
func endLaunchAccess(ctx context.Context, tx *sql.Tx, subject string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE ui_launch_sessions SET revoked_at=? WHERE subject=? AND revoked_at=0`, now.Unix(), subject); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE login_tickets SET revoked_at=?, used_at=CASE WHEN used_at=0 THEN ? ELSE used_at END WHERE subject=? AND revoked_at=0`,
		now.Unix(), now.Unix(), subject)
	return err
}

// EndLaunchAccess is endLaunchAccess for callers outside this package that hold a transaction.
func EndLaunchAccess(ctx context.Context, tx *sql.Tx, subject string, now time.Time) error {
	return endLaunchAccess(ctx, tx, subject, now)
}

// endLaunchAccessByUserID is endLaunchAccess for the account with this id.
func endLaunchAccessByUserID(ctx context.Context, tx *sql.Tx, userID int64, now time.Time) error {
	var username string
	if err := tx.QueryRowContext(ctx, `SELECT username FROM admin_users WHERE id=?`, userID).Scan(&username); err != nil {
		return notFound(err)
	}
	return endLaunchAccess(ctx, tx, "user:"+username, now)
}

// InsertLaunchSession stores a session handle inside tx, but only while the ticket it comes from has
// not been revoked since it was consumed; otherwise ErrRevoked. The check and the insert are one
// statement, so no revocation can fall between them.
func (db *DB) InsertLaunchSession(ctx context.Context, tx *sql.Tx, s LaunchSession) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO ui_launch_sessions(handle_hash,ticket_id,subject,module_id,created_at,expires_at,auth_at,revoked_at)
		SELECT ?,?,?,?,?,?,?,0 WHERE EXISTS (SELECT 1 FROM login_tickets WHERE id=? AND subject=? AND revoked_at=0)`,
		s.HandleHash, s.TicketID, s.Subject, s.ModuleID, s.CreatedAt.Unix(), s.ExpiresAt.Unix(), unixOrZero(s.AuthAt), s.TicketID, s.Subject)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrRevoked
	}
	return nil
}

// AdminSessionLive reports, inside tx, whether the admin session with this hash is still one of the
// named, enabled account and usable at now.
func (db *DB) AdminSessionLive(ctx context.Context, tx *sql.Tx, idHash, username string, now time.Time) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions s JOIN admin_users u ON u.id=s.user_id
		WHERE s.id_hash=? AND u.username=? AND u.disabled_at=0 AND s.expires_at>? AND (s.revalidate_at=0 OR s.revalidate_at>?)`,
		idHash, username, now.Unix(), now.Unix()).Scan(&n)
	return n == 1, err
}

// LaunchSessionByHash looks a session handle up by its hash.
func (db *DB) LaunchSessionByHash(ctx context.Context, hash string) (LaunchSession, error) {
	var s LaunchSession
	var created, expires, revoked, authAt int64
	err := db.QueryRowContext(ctx, `SELECT handle_hash,ticket_id,subject,module_id,created_at,expires_at,revoked_at,auth_at FROM ui_launch_sessions WHERE handle_hash=?`, hash).
		Scan(&s.HandleHash, &s.TicketID, &s.Subject, &s.ModuleID, &created, &expires, &revoked, &authAt)
	if err != nil {
		return LaunchSession{}, notFound(err)
	}
	s.CreatedAt, s.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	s.RevokedAt, s.AuthAt = timeOrZero(revoked), timeOrZero(authAt)
	return s, nil
}

// RevokeLaunchSessions ends every module session a subject began with a ticket.
func (db *DB) RevokeLaunchSessions(ctx context.Context, subject string, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `UPDATE ui_launch_sessions SET revoked_at=? WHERE subject=? AND revoked_at=0`, now.Unix(), subject)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeLoginTickets deletes tickets and session handles that ended before cutoff.
func (db *DB) PurgeLoginTickets(ctx context.Context, cutoff time.Time) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM login_tickets WHERE expires_at < ?`, cutoff.Unix()); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM ui_launch_sessions WHERE expires_at < ?`, cutoff.Unix())
	return err
}
