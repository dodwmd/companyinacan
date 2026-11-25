package files

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// A share link token is "<share id>.<expiry unix seconds>.<mac>". The mac is an HMAC-SHA-256 over
// the id and the expiry under the module's link secret, so a token cannot be forged, extended or
// moved to another share. The database holds the id, the pinned version and the state (uses,
// revoked): never the token or its mac. Revocation and one-time use are database state; the mac
// only proves the token was issued by this service.
const shareMACLabel = "can-files-share-v1"

func (s *Store) shareMAC(id string, exp int64) string {
	m := hmac.New(sha256.New, s.linkSecret)
	m.Write([]byte(shareMACLabel + "\x00" + id + "\x00" + strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Store) shareToken(id string, exp time.Time) string {
	return id + "." + strconv.FormatInt(exp.Unix(), 10) + "." + s.shareMAC(id, exp.Unix())
}

// verifyShareToken checks the shape and the mac, in constant time. It does not look at the clock
// or the database.
func (s *Store) verifyShareToken(tok string) (id string, exp int64, ok bool) {
	if len(tok) > 200 {
		return "", 0, false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || !validID("shr", parts[0]) {
		return "", 0, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || exp <= 0 {
		return "", 0, false
	}
	if subtle.ConstantTimeCompare([]byte(parts[2]), []byte(s.shareMAC(parts[0], exp))) != 1 {
		return "", 0, false
	}
	return parts[0], exp, true
}

// ShareRequest creates a share link. The zero value is a 24 hour (configurable) link to the
// current version that works until it expires or is revoked.
type ShareRequest struct {
	TTL     time.Duration
	OneTime bool
	Label   string
	Version int // 0 = the current version; the link stays on that version
}

// CreateShare makes a link to one version of a file. Editors and managers. The token is returned
// once and is not recoverable. Both the permission and the role are needed: a link gives an outsider
// the bytes.
func (s *Store) CreateShare(ctx context.Context, p *Principal, fileID string, req ShareRequest) (string, *Share, error) {
	if !p.Has(PermWrite) {
		return "", nil, ErrForbidden
	}
	ttl := req.TTL
	if ttl == 0 {
		ttl = time.Duration(s.cfg.Share.DefaultTTLHours) * time.Hour
	}
	if ttl < time.Minute || ttl > time.Duration(s.cfg.Share.MaxTTLHours)*time.Hour {
		return "", nil, invalid("the lifetime must be between 1 minute and %d hours", s.cfg.Share.MaxTTLHours)
	}
	label, err := cleanLabel(req.Label, 100)
	if err != nil || req.Version < 0 || req.Version > 1_000_000 {
		return "", nil, invalid("bad label or version")
	}
	id := newID("shr")
	sealed, err := s.seal("shares", id, "label_sealed", label)
	if err != nil {
		return "", nil, err
	}
	now := s.now()
	exp := time.Unix(now.Add(ttl).Unix(), 0).UTC()
	var out *Share
	err = s.txFor(ctx, p, "share.create", "file:"+fileID, func(tx *sql.Tx) error {
		f, _, err := s.getFile(ctx, tx, p, fileID, RoleEditor)
		if err != nil {
			return err
		}
		if f.status != "active" {
			return ErrNotFound
		}
		version := f.cur
		if req.Version != 0 {
			version = req.Version
			var purged sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT purged_at FROM versions WHERE (file_id = ?) AND (n = ?)`, fileID, version).Scan(&purged); err != nil || purged.Valid {
				return ErrNotFound
			}
		}
		var live int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM shares WHERE (file_id = ?) AND (revoked_at IS NULL) AND (expires_at > ?)`, fileID, ts(now)).Scan(&live); err != nil {
			return err
		}
		if live >= s.cfg.Share.MaxPerFile {
			return errors.Join(ErrQuota, errors.New("the file has the most live share links it may"))
		}
		one := 0
		if req.OneTime {
			one = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO shares (id, file_id, version, label_sealed, expires_at, one_time, created_by, created_at) VALUES (?,?,?,?,?,?,?,?)`,
			id, fileID, version, nullStr(sealed), ts(exp), one, p.Subject, ts(now)); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, p.Subject, "share.create", "file:"+fileID, []string{"share:" + id, "version:" + strconv.Itoa(version)}, "ok"); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventShared, map[string]any{"file_id": fileID, "share_id": id, "version": version, "expires_at": exp, "one_time": req.OneTime, "actor": p.Subject, "shared_at": now.UTC()}); err != nil {
			return err
		}
		out = &Share{ID: id, FileID: fileID, Version: version, Label: label, ExpiresAt: exp, OneTime: req.OneTime, CreatedBy: p.Subject, CreatedAt: now.UTC()}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	s.wake()
	return s.shareToken(id, exp), out, nil
}

// ListShares lists a file's links: a manager sees all of them, an editor the ones they made.
func (s *Store) ListShares(ctx context.Context, p *Principal, fileID string) ([]Share, error) {
	if !p.Has(PermRead) {
		return nil, ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	_, role, err := s.getFile(ctx, s.db, p, fileID, RoleEditor)
	if err != nil {
		return nil, s.forbid(ctx, p, "share.list", "file:"+fileID, err)
	}
	q := `SELECT id, version, label_sealed, expires_at, one_time, uses, created_by, created_at, revoked_at FROM shares WHERE (file_id = ?)`
	args := []any{fileID}
	if role < RoleManager {
		q += ` AND (created_by = ?)`
		args = append(args, p.Subject)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC, id LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Share{}
	for rows.Next() {
		sh := Share{FileID: fileID}
		var label sql.NullString
		var exp, created int64
		var one int
		var revoked sql.NullInt64
		if err := rows.Scan(&sh.ID, &sh.Version, &label, &exp, &one, &sh.Uses, &sh.CreatedBy, &created, &revoked); err != nil {
			return nil, err
		}
		if sh.Label, err = s.unseal("shares", sh.ID, "label_sealed", label); err != nil {
			return nil, err
		}
		sh.ExpiresAt, sh.OneTime, sh.CreatedAt, sh.RevokedAt = fromUnix(exp), one == 1, fromUnix(created), nullTime(revoked)
		out = append(out, sh)
	}
	return out, rows.Err()
}

// RevokeShare ends a link at once. The person who made it and managers of the space. Idempotent.
func (s *Store) RevokeShare(ctx context.Context, p *Principal, shareID string) error {
	if !p.Has(PermWrite) {
		return ErrForbidden
	}
	if !validID("shr", shareID) {
		return ErrNotFound
	}
	return s.txFor(ctx, p, "share.revoke", "share:"+shareID, func(tx *sql.Tx) error {
		var fileID, creator string
		if err := tx.QueryRowContext(ctx, `SELECT file_id, created_by FROM shares WHERE (id = ?)`, shareID).Scan(&fileID, &creator); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		_, role, err := s.getFile(ctx, tx, p, fileID, RoleEditor)
		if err != nil && !errors.Is(err, ErrForbidden) {
			return err
		}
		if role == RoleNone {
			return ErrNotFound
		}
		if role < RoleManager && (creator != p.Subject || role < RoleEditor) {
			return ErrForbidden
		}
		if _, err := tx.ExecContext(ctx, `UPDATE shares SET revoked_at = ? WHERE (id = ?) AND (revoked_at IS NULL)`, ts(s.now()), shareID); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Subject, "share.revoke", "file:"+fileID, []string{"share:" + shareID}, "ok")
	})
}

var errShareGone = errors.New("share link not usable")

// OpenShare opens the content a valid, live link points at. Every failure is ErrNotFound and
// looks the same (a forged, expired, revoked, used or deleted link are not told apart). Failures,
// and only failures, count against the client address; a request with a valid token is never
// refused because of other requests. The use is counted and audited in one transaction before the
// content is opened, so a one-time link cannot be used twice and an unaudited download is not
// served.
func (s *Store) OpenShare(ctx context.Context, token, client string) (*Content, error) {
	fail := func() error {
		s.shareFails.fail(client)
		if s.shareFails.blocked(client) {
			return ErrRate
		}
		return ErrNotFound
	}
	id, exp, ok := s.verifyShareToken(token)
	if !ok {
		return nil, fail()
	}
	now := s.now()
	if exp <= ts(now) {
		return nil, fail()
	}
	var f *fileRow
	var version int
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var fileID string
		var expires int64
		var revoked sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT file_id, version, expires_at, revoked_at FROM shares WHERE (id = ?)`, id).Scan(&fileID, &version, &expires, &revoked); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errShareGone
			}
			return err
		}
		fr, err := scanFileRow(tx.QueryRowContext(ctx, `SELECT `+fileCols+` `+fileFrom+` WHERE (f.id = ?)`, fileID))
		if err != nil {
			return err
		}
		if fr.status != "active" || revoked.Valid || expires <= ts(now) {
			return errShareGone
		}
		res, err := tx.ExecContext(ctx, `UPDATE shares SET uses = uses + 1 WHERE (id = ?) AND (revoked_at IS NULL) AND (expires_at > ?) AND ((one_time = 0) OR (uses = 0))`, id, ts(now))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errShareGone
		}
		f = fr
		return s.audit(ctx, tx, "share:"+id, "share.download", "file:"+fileID, []string{"version:" + strconv.Itoa(version)}, "ok")
	})
	if errors.Is(err, errShareGone) {
		return nil, fail()
	}
	if err != nil {
		return nil, errors.Join(ErrUnavailable, errors.New("the link could not be served"))
	}
	c, err := s.openVersion(ctx, f, version)
	if err != nil {
		return nil, err
	}
	c.actor = "share:" + id
	return c, nil
}
