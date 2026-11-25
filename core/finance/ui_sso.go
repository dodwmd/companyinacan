package finance

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Single sign-on from the admin UI (specs/036-module-ui-sso). The person arrives from the Apps page
// with a one-time launch ticket in the query string. GET only shows a form (a link scanner or a
// browser prefetch must not spend the ticket); the POST redeems it over the module API and starts a
// session of this module's own. The browser receives nothing but that session's cookie, and the
// module keeps no long-lived credential: only an opaque handle that can-core answers "is this
// person still allowed, and with what" for. The session is rechecked at the same intervals as a
// token session (see current).

type ticketData struct{ CSRF, Ticket string }

// revalidateSSO asks can-core again about a launch-ticket session. Any doubt (can-core unreachable,
// handle unknown, subject changed, nothing left to do here) is false and ends the session.
func (u *UI) revalidateSSO(r *http.Request, handle, subject string) (*Principal, string, bool) {
	if u.Sessions == nil {
		return nil, "", false
	}
	v, err := u.Sessions.RevalidateSession(r.Context(), handle)
	if err != nil || !v.Active || v.Subject != subject {
		return nil, "", false
	}
	p, ok := personalPrincipal(v.Subject, v.Permissions)
	if !ok {
		return nil, "", false
	}
	return p, v.Name, true
}

// ticketPage is where the admin UI sends the browser. It never consumes the ticket.
func (u *UI) ticketPage(w http.ResponseWriter, r *http.Request) {
	if u.Sessions == nil {
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	t := r.URL.Query().Get("t")
	if !modclient.TicketShaped(t) {
		u.loginForm(w, r, http.StatusBadRequest, u.msg("login.ticket_invalid"))
		return
	}
	u.render(w, r, nil, http.StatusOK, "ticket.html", "ticket.title", ticketData{CSRF: u.loginCSRF(w, r), Ticket: t})
}

// ticketPost redeems the ticket. Failures are counted per client address (as login failures are) and
// all look the same; a valid ticket is never throttled.
func (u *UI) ticketPost(w http.ResponseWriter, r *http.Request) {
	if u.Sessions == nil {
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderError(w, r, nil, http.StatusBadRequest, "err.invalid_title", "err.bad_form")
		return
	}
	ck, err := r.Cookie(u.loginCSRFName())
	if err != nil || !constEq(ck.Value, r.PostForm.Get("csrf")) {
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.csrf"))
		return
	}
	refuse := func() {
		if ok, _ := u.failIP.allow(u.clientHost(r)); !ok {
			w.Header().Set("Retry-After", "10")
			u.loginForm(w, r, http.StatusTooManyRequests, u.msg("login.locked"))
			return
		}
		u.loginForm(w, r, http.StatusUnauthorized, u.msg("login.ticket_invalid"))
	}
	ls, err := u.Sessions.RedeemTicket(r.Context(), r.PostForm.Get("ticket"))
	if err != nil {
		if errors.Is(err, modclient.ErrUnavailable) {
			u.loginForm(w, r, http.StatusServiceUnavailable, u.msg("login.sso_unavailable"))
			return
		}
		refuse()
		return
	}
	p, ok := personalPrincipal(ls.Subject, ls.Permissions)
	if !ls.Active || !ok || ls.Session == "" {
		refuse()
		return
	}
	// A session id sent along is never adopted, and the one it names is ended.
	if old, err := r.Cookie(u.sessionCookieName()); err == nil {
		u.sessions.drop(old.Value)
	}
	id, _ := u.sessions.createSSO(*p, ls.Session, ls.Name, u.Now())
	u.setCookie(w, u.sessionCookieName(), id, 0)
	u.setCookie(w, u.loginCSRFName(), "", -1)
	// Land on a clean URL: the ticket is not in it, and the place to land is checked here again.
	http.Redirect(w, r, ssoLanding(ls.ReturnPath), http.StatusSeeOther)
}

// returnRe is the allowlist for the landing place can-core passes on: a slash, then letters, digits,
// slashes, underscores and hyphens. No dots, query, fragment, scheme or host.
var returnRe = regexp.MustCompile(`^/[A-Za-z0-9/_-]{0,100}$`)

func ssoLanding(ret string) string {
	if !returnRe.MatchString(ret) || strings.Contains(ret, "//") {
		return "/"
	}
	return ret
}
