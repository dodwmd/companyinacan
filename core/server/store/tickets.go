package store

import (
	"context"
	"database/sql"
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
	CreatedAt  time.Time
	ExpiresAt  time.Time
	UsedAt     time.Time // zero while unused
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
}

// InsertLoginTicket stores a new ticket inside tx.
func (db *DB) InsertLoginTicket(ctx context.Context, tx *sql.Tx, t LoginTicket) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO login_tickets(id,token_hash,subject,module_id,return_path,created_at,expires_at,used_at) VALUES (?,?,?,?,?,?,?,0)`,
		t.ID, t.TokenHash, t.Subject, t.ModuleID, t.ReturnPath, t.CreatedAt.Unix(), t.ExpiresAt.Unix())
	return err
}

// RedeemLoginTicket marks the ticket with this hash used, but only when it is unused, unexpired and
// bound to module: one atomic UPDATE, so of two concurrent redeems exactly one wins. ok is false
// for every other case and the caller cannot tell them apart.
func (db *DB) RedeemLoginTicket(ctx context.Context, tx *sql.Tx, hash, module string, now time.Time) (t LoginTicket, ok bool, err error) {
	var created, expires int64
	err = tx.QueryRowContext(ctx, `UPDATE login_tickets SET used_at=? WHERE token_hash=? AND module_id=? AND used_at=0 AND expires_at>? RETURNING id,subject,module_id,return_path,created_at,expires_at`,
		now.Unix(), hash, module, now.Unix()).Scan(&t.ID, &t.Subject, &t.ModuleID, &t.ReturnPath, &created, &expires)
	if err == sql.ErrNoRows {
		return LoginTicket{}, false, nil
	}
	if err != nil {
		return LoginTicket{}, false, err
	}
	t.TokenHash, t.CreatedAt, t.ExpiresAt, t.UsedAt = hash, time.Unix(created, 0), time.Unix(expires, 0), now
	return t, true, nil
}

// InsertLaunchSession stores a session handle inside tx.
func (db *DB) InsertLaunchSession(ctx context.Context, tx *sql.Tx, s LaunchSession) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ui_launch_sessions(handle_hash,ticket_id,subject,module_id,created_at,expires_at,revoked_at) VALUES (?,?,?,?,?,?,0)`,
		s.HandleHash, s.TicketID, s.Subject, s.ModuleID, s.CreatedAt.Unix(), s.ExpiresAt.Unix())
	return err
}

// LaunchSessionByHash looks a session handle up by its hash.
func (db *DB) LaunchSessionByHash(ctx context.Context, hash string) (LaunchSession, error) {
	var s LaunchSession
	var created, expires, revoked int64
	err := db.QueryRowContext(ctx, `SELECT handle_hash,ticket_id,subject,module_id,created_at,expires_at,revoked_at FROM ui_launch_sessions WHERE handle_hash=?`, hash).
		Scan(&s.HandleHash, &s.TicketID, &s.Subject, &s.ModuleID, &created, &expires, &revoked)
	if err != nil {
		return LaunchSession{}, notFound(err)
	}
	s.CreatedAt, s.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	if revoked != 0 {
		s.RevokedAt = time.Unix(revoked, 0)
	}
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
