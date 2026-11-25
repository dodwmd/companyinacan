package adminui

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/dodwmd/companyinacan/core/server/pat"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Personal access tokens (specs/016-module-wiring): one page to issue, list and revoke tokens for
// the modules' APIs. The token is shown once, in the response to the issue form.

func (s *Server) tokenRoutes() []route {
	return []route{
		{"GET /tokens", s.authed(s.tokensPage)},
		{"POST /tokens", s.authed(s.tokenIssue)},
		{"POST /tokens/{id}/revoke", s.authed(s.tokenRevoke)},
	}
}

type tokModule struct {
	ID    string
	Perms []string
}

type tokRow struct {
	ID, Name, Module, Permissions, State string
	Expires                              time.Time
}

type newTokenView struct {
	Token, Name, Module string
	Expires             time.Time
}

type tokensData struct {
	Modules []tokModule
	Rows    []tokRow
	New     *newTokenView
	TTLs    []int
}

var tokenTTLChoices = []int{24, 24 * 7, 24 * 30, 24 * 90}

func (s *Server) tokenData(r *http.Request, sc *sessionCtx) (tokensData, error) {
	d := tokensData{TTLs: tokenTTLChoices}
	mods, err := s.DB.ListModules(r.Context())
	if err != nil {
		return d, err
	}
	for _, m := range mods {
		if m.State != store.StateEnabled || len(m.Manifest.Permissions) == 0 {
			continue
		}
		var perms []string
		for _, p := range m.Manifest.Permissions {
			// Offer only what this user could grant: the service refuses the rest anyway.
			ok, err := s.allowed(r, sc, p)
			if err != nil {
				return d, err
			}
			if ok {
				perms = append(perms, p)
			}
		}
		if len(perms) > 0 {
			sort.Strings(perms)
			d.Modules = append(d.Modules, tokModule{m.ID, perms})
		}
	}
	manage, err := s.allowed(r, sc, rbac.PermManage)
	if err != nil {
		return d, err
	}
	views, err := s.PATs.ListFor(r.Context(), subjectOf(sc), manage)
	if err != nil {
		return d, err
	}
	for _, v := range views {
		state := s.msg("tk.state_active")
		switch {
		case v.Revoked:
			state = s.msg("tk.state_revoked")
		case v.Expired:
			state = s.msg("tk.state_expired")
		}
		d.Rows = append(d.Rows, tokRow{ID: v.ID, Name: v.Name, Module: v.Module, Permissions: joinPerms(v.Permissions), State: state, Expires: v.ExpiresAt})
	}
	return d, nil
}

func joinPerms(p []string) string {
	out := ""
	for i, x := range p {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

func (s *Server) tokensPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.PATs == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	d, err := s.tokenData(r, sc)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	s.render(w, r, http.StatusOK, "tokens.html", s.page(r, sc, "tokens", s.msg("tk.title"), d))
}

func (s *Server) tokenIssue(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.PATs == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	hours, err := strconv.Atoi(r.PostFormValue("ttl_hours"))
	if err != nil {
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_value")
		return
	}
	module := r.PostFormValue("module")
	iss, err := s.PATs.Issue(r.Context(), subjectOf(sc), pat.IssueRequest{
		Name: r.PostFormValue("name"), Module: module, Permissions: r.PostForm["permissions"], TTL: time.Duration(hours) * time.Hour,
	})
	switch {
	case errors.Is(err, pat.ErrForbidden):
		s.fail(w, r, sc, http.StatusForbidden, "err.no_permission", "tk.cannot_grant")
		return
	case errors.Is(err, pat.ErrInvalid):
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_value")
		return
	case err != nil:
		s.internal(w, r, sc, err)
		return
	}
	d, err := s.tokenData(r, sc)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	d.New = &newTokenView{Token: iss.Token, Name: r.PostFormValue("name"), Module: module, Expires: iss.ExpiresAt}
	s.render(w, r, http.StatusOK, "tokens.html", s.page(r, sc, "tokens", s.msg("tk.title"), d))
}

func (s *Server) tokenRevoke(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.PATs == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	manage, err := s.allowed(r, sc, rbac.PermManage)
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	switch err := s.PATs.Revoke(r.Context(), subjectOf(sc), r.PathValue("id"), manage); {
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, sc, http.StatusNotFound, "err.not_found_title", "err.not_found")
	case err != nil:
		s.internal(w, r, sc, err)
	default:
		s.redirect(w, r, "/tokens", "flash.token_revoked")
	}
}
