package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// APIToken is a personal access token record. The token itself is never stored, only its hash.
type APIToken struct {
	ID          string
	Name        string
	ModuleID    string
	Subject     string
	Permissions []string
	TokenHash   string
	CreatedBy   string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   time.Time // zero while the token is valid
}

const tokenCols = `id,name,module_id,subject,permissions,token_hash,created_by,created_at,expires_at,revoked_at`

func scanToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var perms string
	var created, expires, revoked int64
	if err := row.Scan(&t.ID, &t.Name, &t.ModuleID, &t.Subject, &perms, &t.TokenHash, &t.CreatedBy, &created, &expires, &revoked); err != nil {
		return t, notFound(err)
	}
	if err := json.Unmarshal([]byte(perms), &t.Permissions); err != nil {
		return t, err
	}
	t.CreatedAt, t.ExpiresAt = time.Unix(created, 0), time.Unix(expires, 0)
	if revoked != 0 {
		t.RevokedAt = time.Unix(revoked, 0)
	}
	return t, nil
}

// InsertAPIToken stores a new token record.
func (db *DB) InsertAPIToken(ctx context.Context, t APIToken) error {
	perms, err := json.Marshal(t.Permissions)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO api_tokens(`+tokenCols+`) VALUES (?,?,?,?,?,?,?,?,?,0)`,
		t.ID, t.Name, t.ModuleID, t.Subject, string(perms), t.TokenHash, t.CreatedBy, db.Now().Unix(), t.ExpiresAt.Unix())
	return err
}

// APITokenByHash looks a token up by the SHA-256 of its value (hex).
func (db *DB) APITokenByHash(ctx context.Context, hash string) (APIToken, error) {
	return scanToken(db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE token_hash=?`, hash))
}

// GetAPIToken looks a token record up by its id.
func (db *DB) GetAPIToken(ctx context.Context, id string) (APIToken, error) {
	return scanToken(db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM api_tokens WHERE id=?`, id))
}

// ListAPITokens returns every token record, newest first. Hashes are included in the struct but
// callers must not expose them.
func (db *DB) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+tokenCols+` FROM api_tokens ORDER BY created_at DESC, id LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeAPIToken marks a token revoked (idempotent). An unknown id is ErrNotFound.
func (db *DB) RevokeAPIToken(ctx context.Context, id string) error {
	res, err := db.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = CASE WHEN revoked_at = 0 THEN ? ELSE revoked_at END WHERE id=?`, db.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PruneAPITokens deletes tokens that expired or were revoked before the cutoff.
func (db *DB) PruneAPITokens(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM api_tokens WHERE expires_at < ? OR (revoked_at != 0 AND revoked_at < ?)`, before.Unix(), before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// InsertEventOnce is InsertEvent for publishers that retry: sourceID is the publisher's own id of
// the event. A second call with the same (source, sourceID) stores nothing and returns the id of
// the first event with dup=true. An empty sourceID is a plain InsertEvent.
func (db *DB) InsertEventOnce(ctx context.Context, e Event, sourceID string, targets []DeliveryTarget) (id string, dup bool, err error) {
	if sourceID == "" {
		return e.ID, false, db.InsertEvent(ctx, e, targets)
	}
	err = db.InTx(ctx, func(tx *sql.Tx) error {
		var prev string
		switch err := tx.QueryRowContext(ctx, `SELECT event_id FROM event_dedupe WHERE source=? AND source_id=?`, e.Source, sourceID).Scan(&prev); {
		case err == nil:
			id, dup = prev, true
			return nil
		case err != sql.ErrNoRows:
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_dedupe(source,source_id,event_id,created_at) VALUES (?,?,?,?)`, e.Source, sourceID, e.ID, db.Now().Unix()); err != nil {
			return err
		}
		if err := insertEventTx(ctx, tx, e, targets); err != nil {
			return err
		}
		id = e.ID
		return nil
	})
	return id, dup, err
}

// PruneEventDedupe forgets de-duplication keys older than before (a retry window, not forever).
func (db *DB) PruneEventDedupe(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM event_dedupe WHERE created_at < ?`, before.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequestRotation records that a module secret key must be regenerated at the next reconcile.
func (db *DB) RequestRotation(ctx context.Context, module, key, by string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO secret_rotations(module_id,key,requested_by,requested_at) VALUES (?,?,?,?)
		ON CONFLICT(module_id,key) DO NOTHING`, module, key, by, db.Now().Unix())
	return err
}

// PendingRotations lists the keys of a module awaiting rotation.
func (db *DB) PendingRotations(ctx context.Context, module string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key FROM secret_rotations WHERE module_id=? ORDER BY key`, module)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ClearRotations removes rotation requests once they were applied.
func (db *DB) ClearRotations(ctx context.Context, module string, keys []string) error {
	for _, k := range keys {
		if _, err := db.ExecContext(ctx, `DELETE FROM secret_rotations WHERE module_id=? AND key=?`, module, k); err != nil {
			return err
		}
	}
	return nil
}
