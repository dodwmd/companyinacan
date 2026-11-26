package forms

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// Permissions defined by the forms module (see modules/forms/module.yaml).
const (
	PermLeadsRead  = "leads.read"
	PermLeadsWrite = "leads.write"
	// PermLeadsErase finds leads by address and erases them for good (erase.go).
	PermLeadsErase = "leads.erase"
)

// ErrUnauthenticated means no valid credentials were presented.
var ErrUnauthenticated = errors.New("unauthenticated")

// Principal is an authenticated caller.
type Principal struct {
	Subject     string
	Permissions []string
}

// Can reports whether the principal holds a permission.
func (p *Principal) Can(perm string) bool { return slices.Contains(p.Permissions, perm) }

// Authenticator turns a request into a Principal. The interface is the seam for can-core
// token introspection (POST /v1/tokens/introspect); until that exists the gateway uses
// StaticTokens.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

type tokenEntry struct {
	hash [sha256.Size]byte
	p    Principal
}

// StaticTokens authenticates fixed bearer tokens in constant time.
type StaticTokens struct{ entries []tokenEntry }

// Add registers a token. Empty tokens are ignored so unset env vars cannot become credentials.
func (s *StaticTokens) Add(token string, p Principal) {
	if token == "" {
		return
	}
	s.entries = append(s.entries, tokenEntry{hash: sha256.Sum256([]byte(token)), p: p})
}

// Authenticate implements Authenticator. It compares against every entry so timing does
// not reveal which token matched or how long a prefix was right.
func (s *StaticTokens) Authenticate(r *http.Request) (*Principal, error) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return nil, ErrUnauthenticated
	}
	h := sha256.Sum256([]byte(strings.TrimSpace(tok)))
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
