package finance

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Single sign-on from the admin UI (specs/036-module-ui-sso). The person arrives from the Apps page
// with a one-time launch ticket in the query string. GET only shows a form (a link scanner or a
// browser prefetch must not spend the ticket); the POST redeems it over the module API and starts a
// session of this module's own. The browser receives nothing but that session's cookie, and the
// module keeps no long-lived credential: only an opaque handle that can-core answers "is this
// person still allowed, and with what" for. The session is rechecked at the same intervals as a
// token session (see current).

// ticketData is the interstitial: who the ticket signs in and, when this browser already holds a
// session of somebody else, who that is.
type ticketData struct{ CSRF, Ticket, Name, Account, Subject, Other string }

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

// uiStepUpMaxAge is how recent the person's authentication at the admin UI must be for a
// launch-ticket session to do something irreversible.
const uiStepUpMaxAge = 5 * time.Minute

// stepUpSSO is the step-up of a launch-ticket session, which has no token to type again. can-core is
// asked now (not at the next periodic check) what the person holds, and the route's own rule is
// applied to that answer. can-core must also say that the person authenticated to the admin UI
// (password and TOTP, or the identity provider's auth_time) at most uiStepUpMaxAge ago: a recent
// launch proves nothing, since an admin session left open can launch for hours. Any doubt, an
// unknown authentication time included, refuses.
func (u *UI) stepUpSSO(c *uiCtx) error {
	refused := func() error {
		return invalid("launch", "can-core did not confirm that you may erase; open finance again from the Apps page of the admin UI")
	}
	if u.Sessions == nil {
		return refused()
	}
	v, err := u.Sessions.RevalidateSession(c.r.Context(), c.ss.ssoHandle)
	if err != nil || !v.Active || v.Subject != c.ss.subject {
		return refused()
	}
	p, ok := personalPrincipal(v.Subject, v.Permissions)
	if !ok || !allowedBy(p, PermAdmin, false) {
		return refused()
	}
	// A minute of clock difference between can-core and this module is tolerated, no more.
	if age := u.Now().Sub(v.AuthenticatedAt); v.AuthenticatedAt.IsZero() || age > uiStepUpMaxAge || age < -time.Minute {
		return invalid("launch", "your sign-in is not recent enough to erase: sign out of the admin UI, sign in again (password and code, or at your identity provider), open finance from Apps and erase within %d minutes", int(uiStepUpMaxAge/time.Minute))
	}
	c.p = p
	return nil
}

// launchModule is the module id can-core names the launch nonce cookie after.
const launchModule = "finance"

// liveSession is the subject of the unexpired session this browser holds and the name to show.
func (u *UI) liveSession(r *http.Request) (subject, shown string) {
	ck, err := r.Cookie(u.sessionCookieName())
	if err != nil {
		return "", ""
	}
	ss := u.sessions.get(ck.Value)
	if ss == nil {
		return "", ""
	}
	u.sessions.mu.Lock()
	last, created, name := ss.last, ss.created, ss.name
	u.sessions.mu.Unlock()
	if now := u.Now(); now.Sub(last) > u.IdleTimeout || now.Sub(created) > u.AbsoluteTimeout || ss.pastDeadline(now) {
		return "", ""
	}
	if name == "" {
		name = ss.subject
	}
	return ss.subject, name
}

// peek asks who the ticket would sign in, for this browser only: the nonce cookie can-core set at
// launch must come with it, so a link opened anywhere else names nobody. A page has been written
// when ok is false.
func (u *UI) peek(w http.ResponseWriter, r *http.Request, t string, refuse func()) (modclient.LaunchSession, bool) {
	p, err := u.Sessions.PeekTicket(r.Context(), t, modclient.LaunchBinding(r, launchModule, u.Secure))
	switch {
	case errors.Is(err, modclient.ErrUnavailable):
		u.loginForm(w, r, http.StatusServiceUnavailable, u.msg("login.sso_unavailable"))
	case err != nil || !p.Active || !strings.HasPrefix(p.Subject, "user:"):
		refuse()
	default:
		return p, true
	}
	return modclient.LaunchSession{}, false
}

// showTicket renders the interstitial.
func (u *UI) showTicket(w http.ResponseWriter, r *http.Request, status int, t string, p modclient.LaunchSession) {
	tok := u.loginCSRF(w, r)
	v := ticketData{CSRF: tok, Ticket: t, Name: p.Name, Subject: p.Subject, Account: strings.TrimPrefix(p.Subject, "user:")}
	if other, shown := u.liveSession(r); other != "" && other != p.Subject {
		v.Other = shown
	}
	u.render(w, r, nil, status, "ticket.html", "ticket.title", v)
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
	p, ok := u.peek(w, r, t, func() { u.loginForm(w, r, http.StatusUnauthorized, u.msg("login.ticket_invalid")) })
	if !ok {
		return
	}
	u.showTicket(w, r, http.StatusOK, t, p)
}

// ticketPost redeems the ticket. Failures are counted per client address (as login failures are) and
// all look the same; a valid ticket is never throttled.
func (u *UI) ticketPost(w http.ResponseWriter, r *http.Request) {
	modclient.LimitPublicForm(w, r) // anyone can post here
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
	t := r.PostForm.Get("ticket")
	// A session of somebody else is never replaced without the person saying so.
	if other, _ := u.liveSession(r); other != "" {
		p, ok := u.peek(w, r, t, refuse)
		if !ok {
			return
		}
		if p.Subject != other && r.PostForm.Get("replace") != p.Subject {
			u.showTicket(w, r, http.StatusConflict, t, p)
			return
		}
	}
	ls, err := u.Sessions.RedeemTicket(r.Context(), t, modclient.LaunchBinding(r, launchModule, u.Secure))
	if err != nil {
		if errors.Is(err, modclient.ErrUnavailable) {
			u.loginForm(w, r, http.StatusServiceUnavailable, u.msg("login.sso_unavailable"))
			return
		}
		refuse()
		return
	}
	p, ok := personalPrincipal(ls.Subject, ls.Permissions)
	if !ls.Active || !ok || ls.Session == "" || !ls.ExpiresAt.After(u.Now()) {
		refuse()
		return
	}
	// A session id sent along is never adopted, and the one it names is ended.
	if old, err := r.Cookie(u.sessionCookieName()); err == nil {
		u.sessions.drop(old.Value)
	}
	id, _ := u.sessions.createSSO(*p, ls.Session, ls.Name, ls.ExpiresAt, u.Now())
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
