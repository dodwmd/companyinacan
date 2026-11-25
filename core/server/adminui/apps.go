package adminui

import (
	"errors"
	"net/http"

	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/ticket"
)

// The Apps launcher (E11c, specs/036-module-ui-sso): lists the modules with a web page the
// signed-in person may open and, on a POST (CSRF-protected like every state change), mints a
// one-time launch ticket and moves the browser to the module's sign-in page. The address of the
// module comes from can-core's configuration, never from the request.

func (s *Server) appRoutes() []route {
	return []route{
		{"GET /apps", s.authed(s.appsPage)},
		{"POST /apps/{id}/launch", s.authed(s.appLaunch)},
	}
}

type appsData struct{ Apps []ticket.App }

func (s *Server) appsNav(active string) []navItem {
	if s.Tickets == nil {
		return nil
	}
	return []navItem{{Href: "/apps", Label: s.msg("nav.apps"), Current: active == "apps"}}
}

func (s *Server) appsPage(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Tickets == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	apps, err := s.Tickets.Apps(r.Context(), subjectOf(sc))
	if err != nil {
		s.internal(w, r, sc, err)
		return
	}
	s.render(w, r, http.StatusOK, "apps.html", s.page(r, sc, "apps", s.msg("apps.title"), appsData{Apps: apps}))
}

func (s *Server) appLaunch(w http.ResponseWriter, r *http.Request, sc *sessionCtx) {
	if s.Tickets == nil {
		s.fail(w, r, sc, http.StatusServiceUnavailable, "err.unavailable", "err.unavailable_msg")
		return
	}
	// A session from the identity provider has a revalidation deadline; the module session may not
	// outlive it.
	res, err := s.Tickets.Launch(r.Context(), subjectOf(sc), r.PathValue("id"), r.PostFormValue("return"), sc.Session.RevalidateAt)
	switch {
	case errors.Is(err, ticket.ErrNotAllowed):
		s.fail(w, r, sc, http.StatusNotFound, "apps.refused", "apps.refused_msg")
		return
	case errors.Is(err, ticket.ErrInvalid):
		s.fail(w, r, sc, http.StatusBadRequest, "err.bad_request", "err.bad_value")
		return
	case errors.Is(err, ticket.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		s.fail(w, r, sc, http.StatusTooManyRequests, "apps.busy", "apps.busy_msg")
		return
	case err != nil:
		s.internal(w, r, sc, err)
		return
	}
	// The nonce that binds the ticket to this browser: without it the module refuses the ticket, so
	// a link passed to somebody else signs nobody in.
	ck, err := s.Tickets.BindingCookie(res, r.Host)
	if err != nil {
		s.Log.Warn("apps: cannot give this browser the app's launch cookie", "request_id", httpx.RequestID(r.Context()), "module", res.Module, "error", err)
		s.fail(w, r, sc, http.StatusConflict, "apps.address", "apps.address_msg")
		return
	}
	http.SetCookie(w, ck)
	// Not a redirect: form-action 'self' stops a POST from being redirected to another origin.
	// The page moves the browser by itself (meta refresh) and offers the link; the headers of every
	// page here forbid caching and sending a Referer, so the ticket in the address goes nowhere.
	s.interstitial(w, r, "apps.opening", res.URL)
}
