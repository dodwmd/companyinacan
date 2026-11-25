package jobs

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Single sign-on from the admin UI (specs/036-module-ui-sso). The person arrives from the Apps
// page with a one-time launch ticket in the query string. GET only shows a form (a link scanner or
// a browser prefetch must not spend the ticket); the POST redeems it over the module API and starts
// a session of this module's own. The browser only ever receives that session's cookie, and the
// module never receives a long-lived credential: it keeps an opaque handle that can-core answers
// "is this person still allowed, and with what" for.

type ticketView struct{ Ticket, CSRF string }

// displayName is the name a launch-ticket session shows.
func (s *sessionStore) displayName(ss *uiSession) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ss.name
}

// revalidateSSO asks can-core again about a launch-ticket session (at the same interval a token
// session re-authenticates). Any doubt (can-core unreachable, handle unknown, subject changed,
// no role of this module left) returns nil and ends the session. The name is kept when can-core
// sends none.
func (u *UI) revalidateSSO(r *http.Request, ss *uiSession, name string) (*Principal, string) {
	if u.Sessions == nil {
		return nil, name
	}
	v, err := u.Sessions.RevalidateSession(r.Context(), ss.ssoHandle)
	if err != nil || !v.Active || v.Subject != ss.subject {
		return nil, name
	}
	p := personalPrincipal(v.Subject, v.Permissions)
	if p == nil {
		return nil, name
	}
	if v.Name != "" {
		name = v.Name
	}
	return p, name
}

// ticketPage is where the admin UI sends the browser. It never consumes the ticket.
func (u *UI) ticketPage(w http.ResponseWriter, r *http.Request) {
	if u.Sessions == nil {
		u.renderError(w, r, nil, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	t := r.URL.Query().Get("t")
	if !modclient.TicketShaped(t) {
		u.loginForm(w, r, http.StatusBadRequest, u.msg("login.ticket_invalid"))
		return
	}
	u.render(w, r, nil, http.StatusOK, "ticket.html", "ticket.title", ticketView{Ticket: t, CSRF: u.loginCSRF(w, r)})
}

// ticketPost redeems the ticket. Failures are counted per client address (as login failures are)
// and all look the same; a valid ticket is never throttled.
func (u *UI) ticketPost(w http.ResponseWriter, r *http.Request) {
	if u.Sessions == nil {
		u.renderError(w, r, nil, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderError(w, r, nil, nil, http.StatusBadRequest, "err.invalid_title", "err.bad_form")
		return
	}
	ck, err := r.Cookie(u.loginCSRFName())
	if err != nil || !constEq(ck.Value, r.PostForm.Get("csrf")) {
		u.loginForm(w, r, http.StatusForbidden, u.msg("login.csrf"))
		return
	}
	refuse := func() {
		if ok, _ := u.failIP.allow(clientIP(r, u.TrustProxy)); !ok {
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
	var p *Principal
	if ls.Active && ls.Session != "" {
		p = personalPrincipal(ls.Subject, ls.Permissions)
	}
	if p == nil {
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
	// Land on a clean URL: the ticket is not in it, and the place to land is checked again here.
	http.Redirect(w, r, ssoLanding(ls.ReturnPath), http.StatusSeeOther)
}

// returnRe is the allowlist for the landing place can-core passes on: a slash, then letters,
// digits, slashes, underscores and hyphens. No dots, query, fragment, scheme or host.
var returnRe = regexp.MustCompile(`^/[A-Za-z0-9/_-]{0,100}$`)

// ssoLanding turns the return path into a local URL; anything unexpected lands on the home page.
func ssoLanding(ret string) string {
	if !returnRe.MatchString(ret) || strings.Contains(ret, "//") || strings.HasPrefix(ret, "/login") || strings.HasPrefix(ret, "/logout") {
		return "/"
	}
	return ret
}
