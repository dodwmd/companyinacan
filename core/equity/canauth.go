package equity

import (
	"context"
	"net/http"
	"regexp"
	"slices"

	"github.com/dodwmd/companyinacan/core/modclient"
)

var canSubjectRe = regexp.MustCompile(`^user:[A-Za-z0-9_.@-]{1,64}$`)

// canRoles are the manifest roles (modules/equity/module.yaml) a personal access token can carry,
// each with every permission it needs. A role is granted only when the token holds all of them, so a
// token with a subset of somebody's permissions never gains a role.
//
// There is no board entry on purpose. The board's permissions are a subset of the admin role's, so an
// administrator could leave equity.write out of a token and become the board, which approves grants.
// A personal access token is therefore never the board.
var canRoles = []struct {
	role string
	need []string
}{
	{RoleEmployee, []string{"equity.self-read"}},
	{RoleHR, []string{"equity.read", "equity.write", "equity.sensitive-read", "equity.self-read"}},
	{RoleFinance, []string{"equity.read", "equity.write", "equity.approve", "equity.sensitive-read"}},
	{RoleAdmin, []string{"equity.read", "equity.write", "equity.approve", "equity.sensitive-read", "equity.admin"}},
}

// rolesFromPermissions maps the permissions of a personal access token to this module's roles.
func rolesFromPermissions(perms []string) []string {
	var roles []string
	for _, r := range canRoles {
		if !slices.ContainsFunc(r.need, func(p string) bool { return !slices.Contains(perms, p) }) {
			roles = append(roles, r.role)
		}
	}
	return roles
}

// CanAuth authenticates the personal access tokens can-core issues by asking can-core's token
// introspection who they belong to (specs/016-module-wiring). Any doubt is "unauthenticated". The
// token carries no directory person id, so it never reaches the self-service paths and may not perform
// the actions of needsPerson (policy.go).
type CanAuth struct{ In *modclient.Introspector }

// Authenticate implements Authenticator.
func (a *CanAuth) Authenticate(r *http.Request) (*Principal, error) {
	return a.authenticate(r, a.In.Lookup)
}

// AuthenticateFresh is introspection without the cache.
func (a *CanAuth) AuthenticateFresh(r *http.Request) (*Principal, error) {
	return a.authenticate(r, a.In.Fresh)
}

func (a *CanAuth) authenticate(r *http.Request, lookup func(context.Context, string) (modclient.Identity, error)) (*Principal, error) {
	tok, ok := bearer(r)
	if !ok {
		return nil, ErrUnauthenticated
	}
	id, err := lookup(r.Context(), tok)
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
// ticket session of the web UI) to a Principal. Such a principal is never the board (canRoles).
func personalPrincipal(subject string, perms []string) (*Principal, bool) {
	roles := rolesFromPermissions(perms)
	if len(roles) == 0 || !canSubjectRe.MatchString(subject) {
		return nil, false
	}
	return &Principal{Subject: subject, Roles: roles, CanCore: true}, true
}
