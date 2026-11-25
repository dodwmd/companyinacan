package provision

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// SCIM bearer tokens: random 256-bit values shown once, stored as SHA-256 only, scoped to
// scim.read (GET) and scim.write (everything else, implies read), with a mandatory expiry. A
// valid token is never throttled. Invalid lookups past a generous free allowance wait a short,
// bounded time, which makes guessing slower without ever delaying a legitimate IdP.
const (
	scimTokenPrefix  = "scim_"
	maxSCIMTokens    = 20
	maxSCIMTokenTTL  = 365 * 24 * time.Hour
	minSCIMTokenTTL  = time.Hour
	scimFreeFailures = 300
	scimFailPenalty  = 200 * time.Millisecond
)

// Scopes of a SCIM token.
const (
	ScopeSCIMRead  = "scim.read"
	ScopeSCIMWrite = "scim.write"
)

// SCIMToken is a token record as listed: never the token or its hash.
type SCIMToken struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	CreatedBy string   `json:"created_by"`
	CreatedAt string   `json:"created_at"`
	ExpiresAt string   `json:"expires_at"`
	Revoked   bool     `json:"revoked"`
	Expired   bool     `json:"expired"`
}

var failWindows struct {
	sync.Mutex
	start time.Time
	n     int
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// scimAuth resolves a presented token. A nil principal with a nil error means "not valid".
func (s *Store) scimAuth(ctx context.Context, token string) (*scimPrincipal, error) {
	if !strings.HasPrefix(token, scimTokenPrefix) || len(token) < len(scimTokenPrefix)+20 {
		s.penalise(ctx)
		return nil, nil
	}
	ctx, cancel := s.read(ctx)
	defer cancel()
	var id, hash, scopes string
	var exp, rev int64
	err := s.db.QueryRowContext(ctx, `SELECT id, hash, scopes, expires_at, revoked_at FROM scim_tokens WHERE hash=?`, hashToken(token)).Scan(&id, &hash, &scopes, &exp, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		s.penalise(ctx)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(token))) != 1 || rev != 0 || exp <= s.now().Unix() {
		s.penalise(ctx)
		return nil, nil
	}
	return &scimPrincipal{TokenID: id, Write: slices.Contains(strings.Fields(scopes), ScopeSCIMWrite)}, nil
}

func (s *Store) penalise(ctx context.Context) {
	failWindows.Lock()
	now := s.now()
	if now.Sub(failWindows.start) >= time.Minute {
		failWindows.start, failWindows.n = now, 0
	}
	failWindows.n++
	over := failWindows.n > scimFreeFailures
	failWindows.Unlock()
	if over {
		t := time.NewTimer(scimFailPenalty)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
}

// CreateSCIMToken issues a token. The value is returned once and cannot be read again.
func (s *Store) CreateSCIMToken(ctx context.Context, actor, name string, scopes []string, ttl time.Duration) (SCIMToken, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 || strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || !unicode.IsPrint(r) }) >= 0 {
		return SCIMToken{}, "", fmt.Errorf("%w: name is required, at most 80 printable characters", ErrInvalid)
	}
	scopes = slices.Compact(slices.Sorted(slices.Values(scopes)))
	if len(scopes) == 0 {
		return SCIMToken{}, "", fmt.Errorf("%w: scopes must be scim.read and/or scim.write", ErrInvalid)
	}
	for _, sc := range scopes {
		if sc != ScopeSCIMRead && sc != ScopeSCIMWrite {
			return SCIMToken{}, "", fmt.Errorf("%w: unknown scope", ErrInvalid)
		}
	}
	if ttl < minSCIMTokenTTL || ttl > maxSCIMTokenTTL {
		return SCIMToken{}, "", fmt.Errorf("%w: expiry must be between %s and %s", ErrInvalid, minSCIMTokenTTL, maxSCIMTokenTTL)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	token := scimTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	rec := SCIMToken{ID: newID("scimtok_"), Name: name, Scopes: scopes, CreatedBy: actor, CreatedAt: ts(now.Unix()), ExpiresAt: ts(now.Add(ttl).Unix())}
	err := s.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM scim_tokens WHERE revoked_at=0 AND expires_at>?`, now.Unix()).Scan(&n); err != nil {
			return err
		}
		if n >= maxSCIMTokens {
			return fmt.Errorf("%w: %d live tokens already exist; revoke some first", ErrConflict, n)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO scim_tokens(id,name,hash,scopes,created_by,created_at,expires_at) VALUES (?,?,?,?,?,?,?)`,
			rec.ID, name, hashToken(token), strings.Join(scopes, " "), actor, now.Unix(), now.Add(ttl).Unix()); err != nil {
			return err
		}
		return s.audit(ctx, tx, actor, "scim.token.create", rec.ID, map[string]any{"scopes": scopes, "expires_at": rec.ExpiresAt})
	})
	if err != nil {
		return SCIMToken{}, "", err
	}
	return rec, token, nil
}

// ListSCIMTokens lists token records (never values).
func (s *Store) ListSCIMTokens(ctx context.Context) ([]SCIMToken, error) {
	ctx, cancel := s.read(ctx)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,scopes,created_by,created_at,expires_at,revoked_at FROM scim_tokens ORDER BY created_at DESC, id LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []SCIMToken{}
	now := s.now().Unix()
	for rows.Next() {
		var t SCIMToken
		var sc string
		var c, e, r int64
		if err := rows.Scan(&t.ID, &t.Name, &sc, &t.CreatedBy, &c, &e, &r); err != nil {
			return nil, err
		}
		t.Scopes, t.CreatedAt, t.ExpiresAt, t.Revoked, t.Expired = strings.Fields(sc), ts(c), ts(e), r != 0, e <= now
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeSCIMToken ends a token at once (idempotent).
func (s *Store) RevokeSCIMToken(ctx context.Context, actor, id string) error {
	return s.tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE scim_tokens SET revoked_at=CASE WHEN revoked_at=0 THEN ? ELSE revoked_at END WHERE id=?`, s.now().Unix(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, actor, "scim.token.revoke", id, nil)
	})
}
