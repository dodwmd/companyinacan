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

// Accounting permissions (E15c). They are held by an accounting role, never implied by finance.read or
// finance.write, and only an unscoped principal may use them (see allowedBy): the journal, the reports
// and the bank data span every party and project, so a token limited to some of them gets none of it.
// ledger.write implies ledger.read; ledger.admin (chart, mappings, bank accounts, period close) implies
// both; ledger.approve (post a manual journal, reverse an entry) implies read but not write, so the person
// who drafts a journal is never the person who posts it. finance.admin implies all of them.
const (
	PermLedgerRead    = "finance.ledger.read"
	PermLedgerWrite   = "finance.ledger.write"
	PermLedgerApprove = "finance.ledger.approve"
	PermLedgerAdmin   = "finance.ledger.admin"
)

var ledgerPerms = []string{PermLedgerRead, PermLedgerWrite, PermLedgerApprove, PermLedgerAdmin}

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
	switch perm {
	case PermLedgerRead:
		return slices.ContainsFunc(p.Permissions, func(x string) bool { return slices.Contains(ledgerPerms, x) })
	case PermLedgerWrite:
		return slices.Contains(p.Permissions, PermLedgerWrite) || slices.Contains(p.Permissions, PermLedgerAdmin)
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

// freshAuthenticator is an Authenticator whose answers may be cached and that can also ask now,
// for the step-up before an irreversible action.
type freshAuthenticator interface {
	AuthenticateFresh(r *http.Request) (*Principal, error)
}

// Chain tries each authenticator in order and returns the first success.
type Chain []Authenticator

// AuthenticateFresh implements freshAuthenticator.
func (c Chain) AuthenticateFresh(r *http.Request) (*Principal, error) {
	for _, a := range c {
		check := a.Authenticate
		if f, ok := a.(freshAuthenticator); ok {
			check = f.AuthenticateFresh
		}
		if p, err := check(r); err == nil {
			return p, nil
		}
	}
	return nil, ErrUnauthenticated
}

// Authenticate implements Authenticator.
func (c Chain) Authenticate(r *http.Request) (*Principal, error) {
	for _, a := range c {
		if p, err := a.Authenticate(r); err == nil {
			return p, nil
		}
	}
	return nil, ErrUnauthenticated
}
