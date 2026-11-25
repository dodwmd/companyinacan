package people

import (
	"net/http"
	"slices"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// rolesFromPermissions maps the manifest permissions of a personal access token to the roles this
// module's policy layer knows (modules/people/module.yaml roles). The permission sets of the
// manifest roles overlap, so the mapping uses the permission that is unique to a role:
//
//	people.read                      -> employee (everyone in the directory)
//	performance.write w/o sensitive-write -> manager (the manager set has no sensitive-write)
//	people.write                     -> hr
//	people.admin                     -> admin
//
// Nothing else grants a role: a token with no people.* or performance.* permission has none.
func rolesFromPermissions(perms []string) []string {
	has := func(p string) bool { return slices.Contains(perms, p) }
	var roles []string
	if has("people.read") {
		roles = append(roles, RoleEmployee)
		if has("performance.write") && !has("people.sensitive-write") {
			roles = append(roles, RoleManager)
		}
		if has("people.write") {
			roles = append(roles, RoleHR)
		}
		if has("people.admin") {
			roles = append(roles, RoleAdmin)
		}
	}
	return roles
}

// CanAuth authenticates the personal access tokens can-core issues by asking can-core's token
// introspection who they belong to (specs/016-module-wiring). Any doubt is "unauthenticated".
type CanAuth struct{ In *modclient.Introspector }

// Authenticate implements Authenticator.
func (a *CanAuth) Authenticate(r *http.Request) (*Principal, error) {
	tok, ok := bearer(r)
	if !ok {
		return nil, ErrUnauthenticated
	}
	id, err := a.In.Lookup(r.Context(), tok)
	if err != nil || !id.Active || id.Kind != "user" || !subjectRe.MatchString(id.Subject) {
		return nil, ErrUnauthenticated
	}
	roles := rolesFromPermissions(id.Permissions)
	if len(roles) == 0 {
		return nil, ErrUnauthenticated
	}
	perms := append([]string{}, id.Permissions...) // non-nil: the token is restricted to them
	return &Principal{Subject: id.Subject, Roles: roles, Perms: perms}, nil
}
