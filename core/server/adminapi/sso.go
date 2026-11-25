package adminapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/sso"
)

// Single sign-on administration (epic E11b). Sign-in itself is the browser flow of the admin UI
// (/auth/oidc/login, /auth/oidc/callback); these endpoints show its status and manage identities.
func (s *Server) ssoRoutes() []route {
	return []route{
		{"GET /api/v1/sso", s.authedPerm(rbac.PermManage, s.ssoStatus)},
		{"POST /api/v1/sso/test", s.authedPerm(rbac.PermManage, s.ssoTest)},
		{"GET /api/v1/sso/identities", s.authedPerm(rbac.PermManage, s.ssoIdentities)},
		{"POST /api/v1/sso/identities/{id}/link", s.authedPerm(rbac.PermManage, s.ssoLink)},
		{"POST /api/v1/sso/identities/{id}/unlink", s.authedPerm(rbac.PermManage, s.ssoUnlink)},
	}
}

func (s *Server) ssoStatus(w http.ResponseWriter, r *http.Request, _ principal) {
	if s.SSO == nil {
		s.unavailable(w)
		return
	}
	st, err := s.SSO.Status(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

func (s *Server) ssoTest(w http.ResponseWriter, r *http.Request, p principal) {
	if s.SSO == nil {
		s.unavailable(w)
		return
	}
	res, err := s.SSO.TestDiscovery(r.Context(), p.User.Username)
	switch {
	case errors.Is(err, sso.ErrNotConfigured):
		httpx.Problem(w, http.StatusConflict, "single sign-on is not configured")
	case err != nil:
		s.internal(w, r, err)
	default:
		httpx.WriteJSON(w, http.StatusOK, res)
	}
}

func (s *Server) ssoIdentities(w http.ResponseWriter, r *http.Request, _ principal) {
	if s.SSO == nil {
		s.unavailable(w)
		return
	}
	ids, err := s.SSO.Identities(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"identities": ids})
}

type linkBody struct {
	Username string `json:"username"`
}

func (s *Server) ssoLink(w http.ResponseWriter, r *http.Request, p principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Problem(w, http.StatusNotFound, "no such identity")
		return
	}
	if s.SSO == nil {
		s.unavailable(w)
		return
	}
	var b linkBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	s.ssoDone(w, r, s.SSO.Link(r.Context(), p.User.Username, id, b.Username))
}

func (s *Server) ssoUnlink(w http.ResponseWriter, r *http.Request, p principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpx.Problem(w, http.StatusNotFound, "no such identity")
		return
	}
	if s.SSO == nil {
		s.unavailable(w)
		return
	}
	s.ssoDone(w, r, s.SSO.Unlink(r.Context(), p.User.Username, id))
}

func (s *Server) ssoDone(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, sso.ErrLinkForbidden):
		httpx.Problem(w, http.StatusForbidden, err.Error())
	case errors.Is(err, sso.ErrLinkInvalid), errors.Is(err, rbac.ErrLastOwner):
		httpx.Problem(w, http.StatusConflict, err.Error())
	default:
		s.internal(w, r, err)
	}
}
