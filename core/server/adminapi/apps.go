package adminapi

import (
	"errors"
	"net/http"

	"github.com/dodwmd/companyinacan/core/server/httpx"
	"github.com/dodwmd/companyinacan/core/server/ticket"
)

// Apps: the modules with a web UI a signed-in person may open, and single sign-on into them
// (specs/036-module-ui-sso). Launching mints a one-time ticket bound to that module.

func (s *Server) appRoutes() []route {
	return []route{
		{"GET /api/v1/apps", s.authed(s.listApps)},
		{"POST /api/v1/apps/{id}/launch", s.authed(s.launchApp)},
	}
}

type appView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Tickets == nil {
		s.unavailable(w)
		return
	}
	apps, err := s.Tickets.Apps(r.Context(), p.Subject())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	items := make([]appView, 0, len(apps))
	for _, a := range apps {
		items = append(items, appView{ID: a.ID, Name: a.Name})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type launchBody struct {
	ReturnPath string `json:"return_path"`
}

type launchView struct {
	URL string `json:"url"`
}

func (s *Server) launchApp(w http.ResponseWriter, r *http.Request, p principal) {
	if s.Tickets == nil {
		s.unavailable(w)
		return
	}
	var b launchBody
	if r.ContentLength != 0 && !httpx.DecodeJSONMax(w, r, &b, 2<<10) {
		return
	}
	res, err := s.Tickets.Launch(r.Context(), p.Subject(), r.PathValue("id"), b.ReturnPath, p.Session)
	switch {
	case errors.Is(err, ticket.ErrNotAllowed):
		// No permission, an unknown module, one without a UI: the same answer.
		httpx.Problem(w, http.StatusNotFound, "no such app")
	case errors.Is(err, ticket.ErrInvalid):
		httpx.Problem(w, http.StatusBadRequest, "return_path must be a path inside the app: a slash, then letters, digits, / _ -")
	case errors.Is(err, ticket.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		httpx.Problem(w, http.StatusTooManyRequests, "too many launches; retry in a minute")
	case err != nil:
		s.internal(w, r, err)
	default:
		// The URL works only in the client that holds this nonce cookie (a browser that called this
		// endpoint), so it cannot be handed to somebody else.
		ck, err := s.Tickets.BindingCookie(res, r.Host)
		if err != nil {
			httpx.Problem(w, http.StatusConflict, "call the API at the admin address on the app's domain: "+err.Error())
			return
		}
		http.SetCookie(w, ck)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		httpx.WriteJSON(w, http.StatusOK, launchView{URL: res.URL})
	}
}
