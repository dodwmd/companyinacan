package work

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Token kinds. Personal tokens belong to a person and must expire; service tokens belong
// to a bot or integration and may be long-lived (they should still be rotated).
const (
	KindPersonal = "personal"
	KindService  = "service"
)

const (
	tokenPrefix      = "cwt_"
	maxPersonalLife  = 365 * 24 * time.Hour
	defaultPersonal  = 90 * 24 * time.Hour
	lastUsedInterval = time.Minute
)

// bootstrapSubject is who the environment-supplied admin token acts as; stored tokens cannot
// claim it, so audit entries and activity never confuse the two.
const bootstrapSubject = "bootstrap-admin"

var allScopes = []string{PermRead, PermWrite, PermAdmin}

// TokenInfo describes a token. The secret is shown once, at creation, and never stored.
type TokenInfo struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Subject    string     `json:"subject"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Projects   []string   `json:"projects"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// NewToken is the body of a token create.
type NewToken struct {
	Kind      string     `json:"kind"`
	Subject   string     `json:"subject"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	Projects  []string   `json:"projects"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// CreateToken issues a token and returns its secret once. The creator can only hand out
// scopes and projects it holds itself, so a project-scoped admin cannot widen its reach.
func (s *Store) CreateToken(ctx context.Context, a *Principal, in NewToken) (*TokenInfo, string, error) {
	var p problems
	if in.Kind != KindPersonal && in.Kind != KindService {
		p.add("kind", "must be personal or service")
	}
	if !validSubject(in.Subject) {
		p.add("subject", "required, at most %d characters", maxSubject)
	} else if strings.EqualFold(in.Subject, bootstrapSubject) {
		p.add("subject", "%q is reserved for the bootstrap token", bootstrapSubject)
	}
	if !validText(in.Name, 100) || strings.ContainsAny(in.Name, "\r\n") {
		p.add("name", "one line, at most 100 characters")
	}
	if len(in.Scopes) == 0 {
		p.add("scopes", "give at least one of %s", strings.Join(allScopes, ", "))
	}
	for _, sc := range in.Scopes {
		if !slices.Contains(allScopes, sc) {
			p.add("scopes", "unknown scope %q", sc)
		} else if !a.Can(sc) {
			p.add("scopes", "you cannot grant %s without holding it", sc)
		}
	}
	for _, k := range in.Projects {
		if !projectKeyRE.MatchString(k) {
			p.add("projects", "%q is not a project key", k)
		} else if !a.CanProject(k) {
			p.add("projects", "you cannot grant project %s outside your own scope", k)
		}
	}
	if len(in.Projects) == 0 && !a.Unscoped() {
		p.add("projects", "a project-scoped token can only create project-scoped tokens")
	}
	now := s.now().UTC()
	exp := in.ExpiresAt
	if in.Kind == KindPersonal {
		if exp == nil {
			t := now.Add(defaultPersonal)
			exp = &t
		}
		if exp.After(now.Add(maxPersonalLife)) {
			p.add("expires_at", "a personal token lives at most 365 days")
		}
	}
	if exp != nil && !exp.After(now) {
		p.add("expires_at", "must be in the future")
	}
	if err := p.err(); err != nil {
		return nil, "", err
	}
	var idb [8]byte
	var sec [32]byte
	_, _ = rand.Read(idb[:])
	_, _ = rand.Read(sec[:])
	id := hex.EncodeToString(idb[:])
	secret := base64.RawURLEncoding.EncodeToString(sec[:])
	hash := sha256.Sum256([]byte(secret))
	scopes, projects := dedupe(in.Scopes), dedupe(in.Projects)
	sj, _ := json.Marshal(scopes)
	pj, _ := json.Marshal(projects)
	var expUnix any
	if exp != nil {
		expUnix = ts(*exp)
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, k := range projects {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE key = ?`, k).Scan(&one); err != nil {
				return invalid("projects", "project %s does not exist", k)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_tokens (id, kind, subject, name, secret_hash, scopes, projects, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, in.Kind, in.Subject, in.Name, hash[:], string(sj), string(pj), a.Subject, ts(now), expUnix); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "token.create", id, map[string]any{"kind": in.Kind, "subject": in.Subject, "scopes": scopes, "projects": projects, "expires_at": exp})
	})
	if err != nil {
		return nil, "", err
	}
	info, err := s.GetToken(ctx, id)
	return info, tokenPrefix + id + "_" + secret, err
}

const tokenSelect = `SELECT id, kind, subject, name, scopes, projects, created_by, created_at, expires_at, revoked_at, last_used_at FROM api_tokens`

func scanToken(sc scanner) (*TokenInfo, error) {
	var t TokenInfo
	var scopes, projects string
	var created int64
	var exp, rev, used sql.NullInt64
	if err := sc.Scan(&t.ID, &t.Kind, &t.Subject, &t.Name, &scopes, &projects, &t.CreatedBy, &created, &exp, &rev, &used); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(projects), &t.Projects); err != nil {
		return nil, err
	}
	t.CreatedAt, t.ExpiresAt, t.RevokedAt, t.LastUsedAt = fromUnix(created), nullTime(exp), nullTime(rev), nullTime(used)
	return &t, nil
}

// GetToken returns token metadata by id.
func (s *Store) GetToken(ctx context.Context, id string) (*TokenInfo, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, tokenSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// ListTokens lists token metadata (never secrets), newest first.
func (s *Store) ListTokens(ctx context.Context) ([]TokenInfo, error) {
	rs, err := s.db.QueryContext(ctx, tokenSelect+` ORDER BY created_at DESC, id LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	out := []TokenInfo{}
	for rs.Next() {
		t, err := scanToken(rs)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rs.Err()
}

// RevokeToken revokes a token; it stops working immediately. Revoking twice is fine.
func (s *Store) RevokeToken(ctx context.Context, a *Principal, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, ts(s.now()), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a.Subject, "token.revoke", id, map[string]any{})
	})
}

// TokenAuth authenticates the tokens stored in the database. Tokens look like
// cwt_<id>_<secret>; only SHA-256(secret) is kept, compared in constant time.
type TokenAuth struct{ Store *Store }

// Authenticate implements Authenticator.
func (t TokenAuth) Authenticate(r *http.Request) (*Principal, error) {
	tok, ok := BearerToken(r)
	if !ok || !strings.HasPrefix(tok, tokenPrefix) {
		return nil, ErrUnauthenticated
	}
	id, secret, ok := strings.Cut(strings.TrimPrefix(tok, tokenPrefix), "_")
	if !ok || len(id) != 16 || len(secret) < 16 || len(secret) > 200 {
		return nil, ErrUnauthenticated
	}
	var (
		hash                   []byte
		kind, subject          string
		scopes, projects       string
		expires, revoked, used sql.NullInt64
	)
	ctx := r.Context()
	err := t.Store.db.QueryRowContext(ctx, `SELECT secret_hash, kind, subject, scopes, projects, expires_at, revoked_at, last_used_at FROM api_tokens WHERE id = ?`, id).
		Scan(&hash, &kind, &subject, &scopes, &projects, &expires, &revoked, &used)
	// Always hash and compare, so an unknown id costs the same as a wrong secret.
	sum := sha256.Sum256([]byte(secret))
	if err != nil {
		subtle.ConstantTimeCompare(sum[:], make([]byte, sha256.Size))
		return nil, ErrUnauthenticated
	}
	if subtle.ConstantTimeCompare(sum[:], hash) != 1 {
		return nil, ErrUnauthenticated
	}
	now := t.Store.now()
	if revoked.Valid || (expires.Valid && expires.Int64 <= ts(now)) {
		return nil, ErrUnauthenticated
	}
	p := &Principal{Subject: subject, TokenID: id, Kind: kind}
	if json.Unmarshal([]byte(scopes), &p.Permissions) != nil || json.Unmarshal([]byte(projects), &p.Projects) != nil {
		return nil, fmt.Errorf("token %s: corrupt row", id)
	}
	if !used.Valid || ts(now)-used.Int64 >= int64(lastUsedInterval/time.Second) {
		_, _ = t.Store.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, ts(now), id)
	}
	return p, nil
}
