package adminapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/users"
)

// People's access over the API (E11e, specs/044-users-roles). The same service as the admin UI makes
// every decision; sensitive calls need a sign-in or POST /api/v1/reauth within the last 10 minutes.
// Using a set-up link is a page, not an API call.
func (s *Server) userRoutes() []route {
	return []route{
		{"POST /api/v1/reauth", s.authed(s.reauth)},
		{"POST /api/v1/users", s.authed(s.createUser)},
		{"GET /api/v1/users/{username}", s.authed(s.getUser)},
		{"DELETE /api/v1/users/{username}", s.userCall(func(r *http.Request, a users.Actor, name string) error {
			return s.users().Delete(r.Context(), a, name)
		})},
		{"POST /api/v1/users/{username}/link", s.authed(s.userLink)},
		{"POST /api/v1/users/{username}/disable", s.userCall(func(r *http.Request, a users.Actor, name string) error {
			_, err := s.users().Disable(r.Context(), a, name)
			return err
		})},
		{"POST /api/v1/users/{username}/enable", s.userCall(func(r *http.Request, a users.Actor, name string) error {
			return s.users().Enable(r.Context(), a, name)
		})},
		{"DELETE /api/v1/users/{username}/sessions", s.userCall(func(r *http.Request, a users.Actor, name string) error {
			return s.users().RevokeSessions(r.Context(), a, name)
		})},
		{"DELETE /api/v1/users/{username}/sessions/{sid}", s.userCall(func(r *http.Request, a users.Actor, name string) error {
			return s.users().RevokeSession(r.Context(), a, name, r.PathValue("sid"))
		})},
		{"DELETE /api/v1/users/{username}/tokens/{id}", s.userCall(func(r *http.Request, a users.Actor, name string) error {
			return s.users().RevokeToken(r.Context(), a, name, r.PathValue("id"))
		})},
	}
}

func (s *Server) users() *users.Service {
	if s.Users != nil {
		return s.Users
	}
	return &users.Service{DB: s.DB, Audit: s.Audit, Auth: s.Auth, RBAC: s.RBAC, PATs: s.PATs, SSO: s.SSO, Identity: s.Identity, Now: s.Now, Log: s.Log}
}

func actorOf(p principal) users.Actor { return users.Actor{User: p.User, Session: p.Session} }

// usersErr maps the users service's refusals to statuses.
func (s *Server) usersErr(w http.ResponseWriter, r *http.Request, err error) {
	status := 0
	switch {
	case errors.Is(err, users.ErrStepUp), errors.Is(err, users.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, users.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, users.ErrLastOwner), errors.Is(err, users.ErrExists):
		status = http.StatusConflict
	case errors.Is(err, users.ErrInvalid), errors.Is(err, users.ErrSelf):
		status = http.StatusBadRequest
	case errors.Is(err, users.ErrUnavailable):
		status = http.StatusServiceUnavailable
	default:
		s.internal(w, r, err)
		return
	}
	httpx.Problem(w, status, err.Error())
}

// userCall is an unsafe call about one user that answers 204.
func (s *Server) userCall(act func(*http.Request, users.Actor, string) error) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request, p principal) {
		if err := act(r, actorOf(p), r.PathValue("username")); err != nil {
			s.usersErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (s *Server) reauth(w http.ResponseWriter, r *http.Request, p principal) {
	var b struct {
		Password string `json:"password"`
		TOTP     string `json:"totp"`
	}
	if !httpx.DecodeJSONMax(w, r, &b, httpx.MaxLoginBodyBytes) {
		return
	}
	err := s.Auth.Reauthenticate(r.Context(), p.Session, p.User, b.Password, b.TOTP, httpx.ClientIP(r, s.TrustProxy))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrInvalidCredentials):
		httpx.Problem(w, http.StatusForbidden, "invalid password or authentication code")
	case errors.Is(err, auth.ErrUnauthenticated):
		http.SetCookie(w, auth.ClearCookie(s.Secure))
		httpx.Problem(w, http.StatusUnauthorized, "login required")
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", strconv.Itoa(5))
		httpx.Problem(w, http.StatusServiceUnavailable, "busy; try again shortly")
	default:
		s.internal(w, r, err)
	}
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, p principal) {
	var b struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Send     bool   `json:"send_email"`
	}
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	l, err := s.users().Create(r.Context(), actorOf(p), b.Username, b.Email, b.Send)
	if err != nil {
		s.usersErr(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, l)
}

func (s *Server) userLink(w http.ResponseWriter, r *http.Request, p principal) {
	var b struct {
		Purpose string `json:"purpose"`
		Email   string `json:"email"`
		Send    bool   `json:"send_email"`
	}
	if !httpx.DecodeJSON(w, r, &b) {
		return
	}
	l, err := s.users().SendLink(r.Context(), actorOf(p), r.PathValue("username"), b.Purpose, b.Email, b.Send)
	if err != nil {
		s.usersErr(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, l)
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request, p principal) {
	d, err := s.users().Detail(r.Context(), actorOf(p), r.PathValue("username"))
	if err != nil {
		s.usersErr(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}
