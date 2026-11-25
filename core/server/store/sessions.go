package store

import (
	"context"
	"time"
)

// PublicIDLen is the length of the non-secret session handle shown in the admin UI.
const PublicIDLen = 16

// PublicSessionID is a short handle for a session (a prefix of the stored token hash). It is safe
// to render: the hash cannot be turned back into the cookie token.
func PublicSessionID(s Session) string {
	if len(s.IDHash) < PublicIDLen {
		return s.IDHash
	}
	return s.IDHash[:PublicIDLen]
}

// ListSessions returns the user's live (unexpired at now) sessions, oldest first.
func (db *DB) ListSessions(ctx context.Context, userID int64, now time.Time) ([]Session, error) {
	rows, err := db.QueryContext(ctx, `SELECT id_hash,user_id,csrf_token,ip,created_at,expires_at FROM sessions WHERE user_id=? AND expires_at > ? ORDER BY created_at, id_hash`, userID, now.Unix())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Session
	for rows.Next() {
		var s Session
		var c, e int64
		if err := rows.Scan(&s.IDHash, &s.UserID, &s.CSRF, &s.IP, &c, &e); err != nil {
			return nil, err
		}
		s.CreatedAt, s.ExpiresAt = time.Unix(c, 0), time.Unix(e, 0)
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeSession deletes the user's session whose public id is publicID. A session of another user
// never matches, and ids of the wrong length never match anything.
func (db *DB) RevokeSession(ctx context.Context, userID int64, publicID string) (bool, error) {
	if len(publicID) != PublicIDLen {
		return false, nil
	}
	res, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND substr(id_hash,1,?)=?`, userID, PublicIDLen, publicID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
