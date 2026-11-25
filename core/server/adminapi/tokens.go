package adminapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Personal access tokens for module APIs (specs/016-module-wiring). Any signed-in administrator
// may issue a token for permissions they hold themselves; listing and revoking are limited to
// one's own tokens unless the caller holds rbac.manage.

func (s *Server) tokenRoutes() []route {
	return []route{
		{"GET /api/v1/tokens", s.authed(s.listTokens)},
		{"POST /api/v1/tokens", s.authed(s.issueToken)},
		{"DELETE /api/v1/tokens/{id}", s.authed(s.revokeToken)},
	}
}

type issueTokenBody struct {
	Module      string   `json:"module"`
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
	TTLHours    int      `json:"ttl_hours"`
}

type issuedTokenView struct {
	ID          string   `json:"id"`
	Token       string   `json:"token"`
	Module      string   `json:"module"`
	Permissions []string `json:"permissions"`
	ExpiresAt   string   `json:"expires_at"`
}

func (s *Server) canManage(r *http.Request, p principal) (bool, error) {
	if s.RBAC == nil {
		return true, nil
	}
	return s.RBAC.Check(r.Context(), p.Subject(), rbac.PermManage, "")
}

func (s *Server) issueToken(w http.ResponseWriter, r *http.Request, p principal) {
	if s.PATs == nil {
		s.unavailable(w)
		return
	}
	var b issueTokenBody
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	if b.TTLHours < 1 {
		httpx.Problem(w, http.StatusBadRequest, "ttl_hours must be at least 1 (the maximum is set by can-core, 90 days by default)")
		return
	}
	iss, err := s.PATs.Issue(r.Context(), p.Subject(), pat.IssueRequest{Name: b.Name, Module: b.Module, Permissions: b.Permissions, TTL: time.Duration(b.TTLHours) * time.Hour})
	switch {
	case errors.Is(err, pat.ErrForbidden):
		httpx.Problem(w, http.StatusForbidden, err.Error())
	case errors.Is(err, pat.ErrInvalid):
		httpx.Problem(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.internal(w, r, err)
	default:
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteJSON(w, http.StatusCreated, issuedTokenView{ID: iss.ID, Token: iss.Token, Module: b.Module, Permissions: b.Permissions, ExpiresAt: iss.ExpiresAt.UTC().Format(time.RFC3339)})
	}
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, p principal) {
	if s.PATs == nil {
		s.unavailable(w)
		return
	}
	manage, err := s.canManage(r, p)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	out, err := s.PATs.ListFor(r.Context(), p.Subject(), manage)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, p principal) {
	if s.PATs == nil {
		s.unavailable(w)
		return
	}
	manage, err := s.canManage(r, p)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	switch err := s.PATs.Revoke(r.Context(), p.Subject(), r.PathValue("id"), manage); {
	case errors.Is(err, store.ErrNotFound):
		httpx.Problem(w, http.StatusNotFound, "no such token")
	case err != nil:
		s.internal(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
