package finance

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
	tokenPrefix      = "cft_"
	maxPersonalLife  = 365 * 24 * time.Hour
	defaultPersonal  = 90 * 24 * time.Hour
	lastUsedInterval = time.Minute
)

// bootstrapSubject is who the environment-supplied admin token acts as; stored tokens cannot
// claim it, so audit entries and activity never confuse the two.
const bootstrapSubject = "bootstrap-admin"

var allScopes = []string{PermRead, PermWrite, PermApprove, PermAdmin}

// TokenInfo describes a token. The secret is shown once, at creation, and never stored.
type TokenInfo struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Subject    string     `json:"subject"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Parties    []int64    `json:"parties"`
	Projects   []string   `json:"projects"`
	OwnOnly    bool       `json:"own_only"`
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
	Parties   []int64    `json:"parties"`
	Projects  []string   `json:"projects"`
	OwnOnly   bool       `json:"own_only"`
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
		if !projectRE.MatchString(k) {
			p.add("projects", "%q is not a project key", k)
		} else if !a.CanProject(k) {
			p.add("projects", "you cannot grant project %s outside your own scope", k)
		}
	}
	for _, id := range in.Parties {
		if id < 1 {
			p.add("parties", "%d is not a party id", id)
		} else if !a.CanParty(id) {
			p.add("parties", "you cannot grant party %d outside your own scope", id)
		}
	}
	// A scoped creator can only narrow: it must scope the new token to its own limits.
	if len(in.Projects) == 0 && len(a.Projects) > 0 {
		p.add("projects", "a project-scoped token can only create project-scoped tokens")
	}
	if len(in.Parties) == 0 && len(a.Parties) > 0 {
		p.add("parties", "a party-scoped token can only create party-scoped tokens")
	}
	if !in.OwnOnly && a.OwnOnly {
		p.add("own_only", "an own-only token can only create own-only tokens")
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
	scopes, projects, parties := dedupe(in.Scopes), dedupe(in.Projects), dedupe(in.Parties)
	slices.Sort(scopes)
	slices.Sort(projects)
	slices.Sort(parties)
	sj, _ := json.Marshal(scopes)
	pj, _ := json.Marshal(projects)
	aj, _ := json.Marshal(parties)
	var expUnix any
	if exp != nil {
		expUnix = ts(*exp)
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range parties {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM parties WHERE id = ?`, id).Scan(&one); err != nil {
				return invalid("parties", "party %d does not exist", id)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_tokens (id, kind, subject, name, secret_hash, scopes, parties, projects, own_only, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, in.Kind, in.Subject, in.Name, hash[:], string(sj), string(aj), string(pj), in.OwnOnly, a.Subject, ts(now), expUnix); err != nil {
			return err
		}
		return s.audit(ctx, tx, a.Subject, "token.create", id, map[string]any{"kind": in.Kind, "subject": in.Subject, "scopes": scopes, "parties": parties, "projects": projects, "own_only": in.OwnOnly, "expires_at": exp})
	})
	if err != nil {
		return nil, "", err
	}
	info, err := s.GetToken(ctx, id)
	return info, tokenPrefix + id + "_" + secret, err
}

const tokenSelect = `SELECT id, kind, subject, name, scopes, parties, projects, own_only, created_by, created_at, expires_at, revoked_at, last_used_at FROM api_tokens`

func scanToken(sc scanner) (*TokenInfo, error) {
	var t TokenInfo
	var scopes, parties, projects string
	var own bool
	var created int64
	var exp, rev, used sql.NullInt64
	if err := sc.Scan(&t.ID, &t.Kind, &t.Subject, &t.Name, &scopes, &parties, &projects, &own, &t.CreatedBy, &created, &exp, &rev, &used); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(scopes), &t.Scopes); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(projects), &t.Projects); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(parties), &t.Parties); err != nil {
		return nil, err
	}
	t.OwnOnly = own
	if err := json.Unmarshal([]byte(parties), &t.Parties); err != nil {
		return nil, err
	}
	t.OwnOnly = own
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
// cft_<id>_<secret>; only SHA-256(secret) is kept, compared in constant time.
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
		parties                string
		own                    bool
		expires, revoked, used sql.NullInt64
	)
	ctx := r.Context()
	err := t.Store.db.QueryRowContext(ctx, `SELECT secret_hash, kind, subject, scopes, parties, projects, own_only, expires_at, revoked_at, last_used_at FROM api_tokens WHERE id = ?`, id).
		Scan(&hash, &kind, &subject, &scopes, &parties, &projects, &own, &expires, &revoked, &used)
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
	p := &Principal{Subject: subject, TokenID: id, Kind: kind, OwnOnly: own}
	if json.Unmarshal([]byte(scopes), &p.Permissions) != nil || json.Unmarshal([]byte(projects), &p.Projects) != nil || json.Unmarshal([]byte(parties), &p.Parties) != nil {
		return nil, fmt.Errorf("token %s: corrupt row", id)
	}
	if !used.Valid || ts(now)-used.Int64 >= int64(lastUsedInterval/time.Second) {
		_, _ = t.Store.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, ts(now), id)
	}
	return p, nil
}
