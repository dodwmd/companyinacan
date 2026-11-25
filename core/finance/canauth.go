package finance

import (
	"net/http"
	"regexp"
	"slices"

	"github.com/dodwmd/companyinacan/core/modclient"
)

var canSubjectRe = regexp.MustCompile(`^user:[A-Za-z0-9_.@-]{1,64}$`)

// CanAuth authenticates the personal access tokens can-core issues by asking can-core's token
// introspection who they belong to (specs/016-module-wiring). The permissions are those recorded at
// issue, limited to what this module defines. Any doubt is "unauthenticated": it fails closed.
// A personal token is unscoped by party and project and is not "own only": narrower access is a
// module token (TokenAuth) concern.
type CanAuth struct{ In *modclient.Introspector }

// Authenticate implements Authenticator.
func (a *CanAuth) Authenticate(r *http.Request) (*Principal, error) {
	tok, ok := BearerToken(r)
	if !ok {
		return nil, ErrUnauthenticated
	}
	id, err := a.In.Lookup(r.Context(), tok)
	if err != nil || !id.Active || id.Kind != "user" || !canSubjectRe.MatchString(id.Subject) {
		return nil, ErrUnauthenticated
	}
	p, ok := personalPrincipal(id.Subject, id.Permissions)
	if !ok {
		return nil, ErrUnauthenticated
	}
	return p, nil
}

// personalPrincipal maps what can-core says about a person (a personal access token or a launch
// ticket session) to a Principal: the permissions are limited to those this module defines, and
// nothing left means no principal. Shared so the two sign-in paths cannot drift.
func personalPrincipal(subject string, in []string) (*Principal, bool) {
	if !canSubjectRe.MatchString(subject) {
		return nil, false
	}
	var perms []string
	for _, p := range in {
		if (p == PermRead || p == PermWrite || p == PermApprove || p == PermAdmin || slices.Contains(ledgerPerms, p)) && !slices.Contains(perms, p) {
			perms = append(perms, p)
		}
	}
	if len(perms) == 0 {
		return nil, false
	}
	return &Principal{Subject: subject, Kind: "personal", Permissions: perms}, true
}
