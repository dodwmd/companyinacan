package finance

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// Permissions defined by the finance module (see modules/finance/module.yaml). Admin implies
// every other permission; write implies read; approve implies read but NOT write, so an
// approver who cannot create things is a valid, common role.
const (
	PermRead    = "finance.read"
	PermWrite   = "finance.write"
	PermApprove = "finance.approve"
	PermAdmin   = "finance.admin"
)

// ErrUnauthenticated means no valid credentials were presented.
var ErrUnauthenticated = errors.New("unauthenticated")

// Principal is an authenticated caller. Parties and Projects limit which customers/vendors
// and which projects the caller can see or change (empty means all of them). OwnOnly
// confines timesheets and expenses to those the caller owns, for tokens given to employees.
type Principal struct {
	Subject     string
	TokenID     string
	Kind        string // personal, service or static
	Permissions []string
	Parties     []int64
	Projects    []string
	OwnOnly     bool
}

// Can reports whether the principal holds a permission (with the implications above).
func (p *Principal) Can(perm string) bool {
	if slices.Contains(p.Permissions, PermAdmin) {
		return true
	}
	if perm == PermRead && (slices.Contains(p.Permissions, PermWrite) || slices.Contains(p.Permissions, PermApprove)) {
		return true
	}
	return slices.Contains(p.Permissions, perm)
}

// CanParty reports whether the principal's party scope covers a party.
func (p *Principal) CanParty(id int64) bool {
	return len(p.Parties) == 0 || slices.Contains(p.Parties, id)
}

// CanProject reports whether the principal's project scope covers a project. An empty
// project (a record with none) is covered only by an unscoped principal.
func (p *Principal) CanProject(key string) bool {
	return len(p.Projects) == 0 || (key != "" && slices.Contains(p.Projects, key))
}

// Unscoped reports whether the principal can reach every party and project.
func (p *Principal) Unscoped() bool { return len(p.Parties) == 0 && len(p.Projects) == 0 }

// Authenticator turns a request into a Principal. It is the seam for can-core token
// introspection (POST /v1/tokens/introspect, SPEC.md 4.3): until that lands the service
// authenticates its own API tokens (TokenAuth) and one bootstrap token (StaticTokens).
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// BearerToken extracts the bearer credential from a request.
func BearerToken(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return "", false
	}
	return tok, true
}

type staticEntry struct {
	hash [sha256.Size]byte
	p    Principal
}

// StaticTokens authenticates fixed bearer tokens in constant time. The bootstrap admin
// token comes from the environment; day-to-day tokens live in the database.
type StaticTokens struct{ entries []staticEntry }

// Add registers a token. Empty tokens are ignored so unset env vars cannot become credentials.
func (s *StaticTokens) Add(token string, p Principal) {
	if token == "" {
		return
	}
	s.entries = append(s.entries, staticEntry{hash: sha256.Sum256([]byte(token)), p: p})
}

// Authenticate implements Authenticator, comparing against every entry.
func (s *StaticTokens) Authenticate(r *http.Request) (*Principal, error) {
	tok, ok := BearerToken(r)
	if !ok {
		return nil, ErrUnauthenticated
	}
	h := sha256.Sum256([]byte(tok))
	var found *Principal
	for i := range s.entries {
		if subtle.ConstantTimeCompare(h[:], s.entries[i].hash[:]) == 1 {
			found = &s.entries[i].p
		}
	}
	if found == nil {
		return nil, ErrUnauthenticated
	}
	return found, nil
}

// Chain tries each authenticator in order and returns the first success.
type Chain []Authenticator

// Authenticate implements Authenticator.
func (c Chain) Authenticate(r *http.Request) (*Principal, error) {
	for _, a := range c {
		if p, err := a.Authenticate(r); err == nil {
			return p, nil
		}
	}
	return nil, ErrUnauthenticated
}
