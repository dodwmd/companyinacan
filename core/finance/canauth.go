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
	var perms []string
	for _, p := range id.Permissions {
		if (p == PermRead || p == PermWrite || p == PermApprove || p == PermAdmin) && !slices.Contains(perms, p) {
			perms = append(perms, p)
		}
	}
	if len(perms) == 0 {
		return nil, ErrUnauthenticated
	}
	return &Principal{Subject: id.Subject, Kind: "personal", Permissions: perms}, nil
}
