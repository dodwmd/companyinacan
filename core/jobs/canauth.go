package jobs

import (
	"net/http"
	"regexp"
	"slices"

	"github.com/dodwmd/companyinacan/core/modclient"
)

var canSubjectRe = regexp.MustCompile(`^user:[A-Za-z0-9_.@-]{1,64}$`)

// roleFromPermissions maps the manifest permissions of a personal access token to the one role
// Jobs gives a token, the strongest the permissions fully cover (modules/jobs/module.yaml roles).
// A token whose permissions cover no role has none and is refused.
func roleFromPermissions(perms []string) string {
	has := func(p ...string) bool {
		for _, x := range p {
			if !slices.Contains(perms, x) {
				return false
			}
		}
		return true
	}
	switch {
	case has("jobs.admin"):
		return RoleAdmin
	case has("jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire"):
		return RoleRecruiter
	case has("jobs.read", "ats.read", "ats.write", "ats.interview"):
		return RoleHiringManager
	case has("ats.interview"):
		return RoleInterviewer
	}
	return ""
}

// CanAuth authenticates the personal access tokens can-core issues by asking can-core's token
// introspection who they belong to (specs/016-module-wiring). Any doubt is "unauthenticated".
// A personal token has no job scope: it reaches every job its role allows.
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
	role := roleFromPermissions(id.Permissions)
	if role == "" {
		return nil, ErrUnauthenticated
	}
	return &Principal{Subject: id.Subject, Kind: "personal", Role: role}, nil
}
