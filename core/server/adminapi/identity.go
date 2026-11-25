package adminapi

import (
	"errors"
	"net/http"

	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/identity"
	"github.com/dodwmd/companyinacan/core/server/rbac"
)

// Revoke now (E13d): an administrator with rbac.manage ends the tokens and sessions of an account
// at once and disables it, for a termination that cannot wait for the people module to report the
// person offboarded (someone still offboarding, flagged urgent). Audited with the actor.

func (s *Server) identityRoutes() []route {
	return []route{{"POST /api/v1/identity/revoke-now", s.authedPerm(rbac.PermManage, s.revokeNow)}}
}

func (s *Server) revokeNow(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Identity == nil {
		s.unavailable(w)
		return
	}
	var b struct {
		Subject string `json:"subject"`
	}
	if !httpx.DecodeJSONMax(w, r, &b, 4<<10) {
		return
	}
	if b.Subject == p.Subject() {
		httpx.Problem(w, http.StatusBadRequest, "you cannot revoke your own account")
		return
	}
	res, err := s.Identity.RevokeNow(r.Context(), p.Subject(), b.Subject)
	switch {
	case errors.Is(err, identity.ErrInvalid):
		httpx.Problem(w, http.StatusBadRequest, "subject must be user:<name>")
	case err != nil:
		s.internal(w, r, err)
	default:
		httpx.WriteJSON(w, http.StatusOK, res)
	}
}
