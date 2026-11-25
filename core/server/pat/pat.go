// Package pat issues, revokes and introspects personal access tokens: the credentials people and
// scripts use against a module's API (specs/016-module-wiring). A token is bound to one module and
// to a subset of the permissions that module declares in its manifest. Only the SHA-256 of the
// token is stored. Modules never see the stored record: they ask can-core to introspect a token
// and are told the subject and permissions only when the token belongs to them.
package pat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Token shape and limits.
const (
	TokenPrefix      = "canpat_"
	maxTokenLen      = 128 // anything longer cannot be one of ours; rejected before any lookup
	MaxLivePerModule = 50
	// MaxLivePerIssuer bounds one administrator's live tokens per module, so one person cannot use
	// up the module's quota.
	MaxLivePerIssuer = 10
	// DefaultMaxTTL is the longest lifetime unless Service.MaxTTL says otherwise: a token is a
	// credential that must be reissued now and then, not a permanent key.
	DefaultMaxTTL  = 90 * 24 * time.Hour
	MinTTL         = time.Hour
	MaxPermissions = 20
	maxNameLen     = 80
)

// ErrInvalid is returned for a request that cannot be honoured (the message is safe to show).
var ErrInvalid = errors.New("invalid token request")

// ErrForbidden is returned when the issuer asks for a permission they do not hold themselves.
var ErrForbidden = errors.New("not allowed to grant this")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Service issues and checks tokens.
type Service struct {
	db    *store.DB
	audit *audit.Log
	Now   func() time.Time
	// MaxTTL is the longest token lifetime issued (default DefaultMaxTTL).
	MaxTTL time.Duration
	// FailFree is how many invalid token lookups per module and minute are answered at full speed
	// (default 300); past it each invalid lookup waits FailPenalty (default 200 ms). A valid token
	// is never delayed. Sleep replaces the wait in tests.
	FailFree    int
	FailPenalty time.Duration
	Sleep       func(context.Context, time.Duration)

	failMu sync.Mutex
	fails  map[string]*failWindow
	// Holds, when set, reports whether the issuing subject holds a module permission itself. A
	// token can then never grant more than its issuer has.
	Holds func(ctx context.Context, subject, permission string) (bool, error)
}

// impliedBy lists, per module, the permissions that carry another one inside the module itself:
// finance and work treat <module>.admin as every permission (and write/approve as read), so a
// subject holding only the admin permission holds the rest. Issue and introspection both use this
// one table, so what may be granted and what stays active agree. The jobs and people admin
// permissions imply nothing and are not listed.
var impliedBy = map[string]map[string][]string{
	"finance": {
		"finance.read": {"finance.write", "finance.approve", "finance.admin"}, "finance.write": {"finance.admin"},
		"finance.approve": {"finance.admin"},
	},
	"work": {"work.read": {"work.write", "work.admin"}, "work.write": {"work.admin"}},
}

// holds reports whether subject holds perm of the module, directly or through a permission that
// implies it inside the module.
func (s *Service) holds(ctx context.Context, subject, module, perm string) (bool, error) {
	for _, p := range append([]string{perm}, impliedBy[module][perm]...) {
		ok, err := s.Holds(ctx, subject, p)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// New returns a Service.
type failWindow struct {
	start time.Time
	n     int
}

func (s *Service) maxTTL() time.Duration {
	if s.MaxTTL > 0 {
		return s.MaxTTL
	}
	return DefaultMaxTTL
}

func New(db *store.DB, al *audit.Log) *Service { return &Service{db: db, audit: al, Now: time.Now} }

// IssueRequest is what an administrator asks for.
type IssueRequest struct {
	Name        string
	Module      string
	Permissions []string
	TTL         time.Duration
}

// Issued is the one-time answer to an issue: the only place the token value ever appears.
type Issued struct {
	ID        string
	Token     string
	ExpiresAt time.Time
}

// View is a token record as listed: never the token or its hash.
type View struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Module      string    `json:"module"`
	Subject     string    `json:"subject"`
	Permissions []string  `json:"permissions"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Revoked     bool      `json:"revoked"`
	Expired     bool      `json:"expired"`
}

// Result is the introspection answer for a valid token.
type Result struct {
	Subject     string
	Permissions []string
	TokenID     string
	ExpiresAt   time.Time
}

func hashOf(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func randomID(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return prefix + hex.EncodeToString(b)
}

func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxNameLen {
		return "", invalid("name is required, at most %d characters", maxNameLen)
	}
	for _, r := range s {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return "", invalid("name may not contain control or invisible characters")
		}
	}
	return s, nil
}

// Issue creates a token for a signed-in administrator (actor is "user:<name>").
func (s *Service) Issue(ctx context.Context, actor string, req IssueRequest) (Issued, error) {
	if !strings.HasPrefix(actor, "user:") || len(actor) < 6 {
		return Issued{}, invalid("only a signed-in administrator can issue tokens")
	}
	name, err := cleanName(req.Name)
	if err != nil {
		return Issued{}, err
	}
	if req.TTL < MinTTL || req.TTL > s.maxTTL() {
		return Issued{}, invalid("expiry must be between %s and %s", MinTTL, s.maxTTL())
	}
	if n := len(req.Permissions); n == 0 || n > MaxPermissions {
		return Issued{}, invalid("between 1 and %d permissions are required", MaxPermissions)
	}
	mod, err := s.db.GetModule(ctx, req.Module)
	if errors.Is(err, store.ErrNotFound) || (err == nil && mod.State != store.StateEnabled) {
		return Issued{}, invalid("module %q is not installed and enabled", req.Module)
	}
	if err != nil {
		return Issued{}, err
	}
	perms := slices.Clone(req.Permissions)
	slices.Sort(perms)
	perms = slices.Compact(perms)
	for _, p := range perms {
		if !slices.Contains(mod.Manifest.Permissions, p) {
			return Issued{}, invalid("module %s does not declare permission %q", mod.ID, p)
		}
		if s.Holds != nil {
			ok, err := s.holds(ctx, actor, mod.ID, p)
			if err != nil {
				return Issued{}, err
			}
			if !ok {
				return Issued{}, fmt.Errorf("%w: you do not hold %q yourself", ErrForbidden, p)
			}
		}
	}
	now := s.Now()
	live, err := s.countLive(ctx, mod.ID, "", now)
	if err != nil {
		return Issued{}, err
	}
	mine, err := s.countLive(ctx, mod.ID, actor, now)
	if err != nil {
		return Issued{}, err
	}
	if mine >= MaxLivePerIssuer {
		return Issued{}, invalid("you already have %d live tokens for module %s; revoke some first", MaxLivePerIssuer, mod.ID)
	}
	if live >= MaxLivePerModule {
		return Issued{}, invalid("module %s already has %d live tokens; revoke some first", mod.ID, MaxLivePerModule)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	rec := store.APIToken{ID: randomID("pat_", 8), Name: name, ModuleID: mod.ID, Subject: actor, Permissions: perms,
		TokenHash: hashOf(token), CreatedBy: actor, ExpiresAt: now.Add(req.TTL)}
	if err := s.db.InsertAPIToken(ctx, rec); err != nil {
		return Issued{}, err
	}
	if _, err := s.audit.Append(ctx, audit.Entry{Actor: actor, Action: "token.issue", Target: rec.ID,
		Detail: map[string]any{"module": mod.ID, "permissions": perms, "expires_at": rec.ExpiresAt.UTC().Format(time.RFC3339)}}); err != nil {
		return Issued{}, err
	}
	return Issued{ID: rec.ID, Token: token, ExpiresAt: rec.ExpiresAt}, nil
}

func (s *Service) countLive(ctx context.Context, module, issuer string, now time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM api_tokens WHERE module_id=? AND revoked_at=0 AND expires_at > ? AND (?='' OR created_by=?)`, module, now.Unix(), issuer, issuer).Scan(&n)
	return n, err
}

// Revoke ends a token's validity at once. Unless manage is set the actor may revoke only their
// own tokens; someone else's token answers store.ErrNotFound, as an unknown one does.
func (s *Service) Revoke(ctx context.Context, actor, id string, manage bool) error {
	t, err := s.db.GetAPIToken(ctx, id)
	if err != nil {
		return err
	}
	if !manage && t.CreatedBy != actor {
		return store.ErrNotFound
	}
	if err := s.db.RevokeAPIToken(ctx, id); err != nil {
		return err
	}
	_, err = s.audit.Append(ctx, audit.Entry{Actor: actor, Action: "token.revoke", Target: id})
	return err
}

// ListFor returns the token records an actor may see for the admin views: their own, or every
// one when manage is set.
func (s *Service) ListFor(ctx context.Context, actor string, manage bool) ([]View, error) {
	ts, err := s.db.ListAPITokens(ctx)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	out := make([]View, 0, len(ts))
	for _, t := range ts {
		if !manage && t.CreatedBy != actor {
			continue
		}
		out = append(out, View{ID: t.ID, Name: t.Name, Module: t.ModuleID, Subject: t.Subject, Permissions: t.Permissions,
			CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, Revoked: !t.RevokedAt.IsZero(), Expired: !now.Before(t.ExpiresAt)})
	}
	return out, nil
}

// LooksLikeToken reports whether s has the shape of a personal access token. Callers use it to
// route a token to Introspect without a database round trip for other credential types.
func LooksLikeToken(s string) bool {
	return strings.HasPrefix(s, TokenPrefix) && len(s) > len(TokenPrefix) && len(s) <= maxTokenLen
}

// Introspect resolves a token on behalf of the calling module. ok is false for every kind of
// failure (unknown, malformed, expired, revoked, another module's) and the caller cannot tell them
// apart. Permissions the module's current manifest no longer declares are dropped.
func (s *Service) Introspect(ctx context.Context, callerModule, token string) (Result, bool) {
	res, ok := s.introspect(ctx, callerModule, token)
	if !ok && LooksLikeToken(token) {
		s.penalise(ctx, callerModule)
	}
	return res, ok
}

// penalise slows invalid lookups of one module past a per-minute quota. It never runs for a valid
// token.
func (s *Service) penalise(ctx context.Context, module string) {
	free, pen := s.FailFree, s.FailPenalty
	if free <= 0 {
		free = 300
	}
	if pen <= 0 {
		pen = 200 * time.Millisecond
	}
	now := s.Now()
	s.failMu.Lock()
	if s.fails == nil {
		s.fails = map[string]*failWindow{}
	}
	w := s.fails[module]
	if w == nil || now.Sub(w.start) >= time.Minute {
		if len(s.fails) > 256 {
			s.fails = map[string]*failWindow{}
		}
		w = &failWindow{start: now}
		s.fails[module] = w
	}
	w.n++
	over := w.n > free
	s.failMu.Unlock()
	if !over {
		return
	}
	if s.Sleep != nil {
		s.Sleep(ctx, pen)
		return
	}
	t := time.NewTimer(pen)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (s *Service) introspect(ctx context.Context, callerModule, token string) (Result, bool) {
	if !LooksLikeToken(token) {
		return Result{}, false
	}
	t, err := s.db.APITokenByHash(ctx, hashOf(token))
	if err != nil || t.ModuleID != callerModule || !t.RevokedAt.IsZero() || !s.Now().Before(t.ExpiresAt) {
		return Result{}, false
	}
	mod, err := s.db.GetModule(ctx, t.ModuleID)
	if err != nil || mod.State != store.StateEnabled {
		return Result{}, false
	}
	// The issuer must still exist, and the token carries only what the issuer still holds: a token
	// never outlives the rights of the person who made it.
	if _, err := s.db.AdminByUsername(ctx, strings.TrimPrefix(t.Subject, "user:")); err != nil {
		return Result{}, false
	}
	var perms []string
	for _, p := range t.Permissions {
		if !slices.Contains(mod.Manifest.Permissions, p) {
			continue
		}
		if s.Holds != nil {
			ok, err := s.holds(ctx, t.Subject, mod.ID, p)
			if err != nil || !ok {
				continue
			}
		}
		perms = append(perms, p)
	}
	if len(perms) == 0 {
		return Result{}, false
	}
	return Result{Subject: t.Subject, Permissions: perms, TokenID: t.ID, ExpiresAt: t.ExpiresAt}, true
}
