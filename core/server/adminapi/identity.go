package adminapi

import (
	"net/http"
	"strings"

	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/rbac"
)

// Revoke now (E13d): an administrator with rbac.manage ends the tokens and sessions of an account
// at once and disables it, for a termination that cannot wait for the people module to report the
// person offboarded (someone still offboarding, flagged urgent). Audited with the actor.

func (s *Server) identityRoutes() []route {
	return []route{
		{"POST /api/v1/identity/revoke-now", s.authedPerm(rbac.PermManage, s.revokeNow)},
		{"POST /api/v1/identity/enable", s.authedPerm(rbac.PermManage, s.enableAccount)},
	}
}

func (s *Server) accountSubject(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.Identity == nil {
		s.unavailable(w)
		return "", false
	}
	var b struct {
		Subject string `json:"subject"`
	}
	if !httpx.DecodeJSONMax(w, r, &b, 4<<10) {
		return "", false
	}
	name, ok := strings.CutPrefix(b.Subject, "user:")
	if !ok || !rbac.ValidSubject(b.Subject) {
		httpx.Problem(w, http.StatusBadRequest, "subject must be user:<name>")
		return "", false
	}
	return name, true
}

func (s *Server) revokeNow(w http.ResponseWriter, r *http.Request, p principal) {
	name, ok := s.accountSubject(w, r)
	if !ok {
		return
	}
	res, err := s.users().Disable(r.Context(), actorOf(p), name)
	if err != nil {
		s.usersErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// enableAccount re-enables a disabled account. Owner only: rbac.manage alone is not enough.
func (s *Server) enableAccount(w http.ResponseWriter, r *http.Request, p principal) {
	name, ok := s.accountSubject(w, r)
	if !ok {
		return
	}
	if err := s.users().Enable(r.Context(), actorOf(p), name); err != nil {
		s.usersErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
