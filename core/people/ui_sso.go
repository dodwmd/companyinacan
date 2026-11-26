package people

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// Single sign-on from the admin UI (specs/036-module-ui-sso): the flow is core/uikit/session's.

// launchPages draws the outcomes of the ticket requests.
func (u *UI) launchPages(w http.ResponseWriter, r *http.Request) session.LaunchPages {
	return session.LaunchPages{
		SignIn: func(status int, key string) { u.loginForm(w, r, status, u.msg(key)) },
		Confirm: func(status int, v session.TicketView) {
			u.render(w, r, nil, status, "ticket.html", "ticket.title", v)
		},
		Failed: func() (string, bool) { return "10", !u.failIP.allow(u.clientHost(r)) },
	}
}

// ticketPage is where the admin UI sends the browser. It never consumes the ticket.
func (u *UI) ticketPage(w http.ResponseWriter, r *http.Request) {
	if u.Sessions == nil {
		u.renderError(w, r, nil, Actor{}, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	u.sessions.TicketPage(w, r, u.launchPages(w, r))
}

// ticketPost redeems the ticket.
func (u *UI) ticketPost(w http.ResponseWriter, r *http.Request) {
	modclient.LimitPublicForm(w, r) // anyone can post here
	if u.Sessions == nil {
		u.renderError(w, r, nil, Actor{}, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderError(w, r, nil, Actor{}, http.StatusBadRequest, "err.invalid_title", "err.bad_form")
		return
	}
	// As at the token login: a person must be linked in the directory (sso_subject) and active.
	active := func(p Principal) bool {
		a, err := u.Svc.ResolveActor(r.Context(), &p)
		return err == nil && a.HasPerson && a.Active()
	}
	u.sessions.TicketPost(w, r, u.launchPages(w, r), active, ssoLanding)
}

// returnRe is the allowlist for the landing place can-core passes on: a slash, then letters,
// digits, slashes, underscores and hyphens. No dots, query, fragment, scheme or host.
var returnRe = regexp.MustCompile(`^/[A-Za-z0-9/_-]{0,100}$`)

// ssoLanding turns the return path into a local URL; anything unexpected lands on the home page.
func ssoLanding(ret string) string {
	if !returnRe.MatchString(ret) || strings.Contains(ret, "//") {
		return "/"
	}
	return ret
}
