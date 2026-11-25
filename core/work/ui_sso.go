package work

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Single sign-on from the admin UI (specs/036-module-ui-sso). The person arrives from the Apps
// page with a one-time launch ticket in the query string. GET only shows a form (a link scanner or
// a browser prefetch must not spend the ticket); the POST redeems it over the module API and starts
// a session of this module's own. The browser never receives anything but that session's cookie,
// and the module never receives a long-lived credential: it keeps an opaque handle that can-core
// answers "is this person still allowed, and with what" for.

// ssoRecheck is how often a launch-ticket session asks can-core again: as often as a personal access
// token is re-introspected here (its cache lifetime), so a removed role or a disabled account ends
// the module session within the same half minute, privileged or not.
const ssoRecheck = 30 * time.Second

type uiTicketView struct{ Ticket string }

// workPerms keeps the permissions this module defines.
func workPerms(in []string) []string {
	var perms []string
	for _, p := range in {
		if (p == PermRead || p == PermWrite || p == PermAdmin) && !slices.Contains(perms, p) {
			perms = append(perms, p)
		}
	}
	return perms
}

// ssoPrincipal returns the principal of a launch-ticket session, asking can-core again when the
// last answer is older than ssoRecheck. Any doubt (can-core unreachable, handle unknown, subject
// changed, nothing left to do here) ends the session.
func (u *webUI) ssoPrincipal(r *http.Request, sess *uiSession) (*Principal, bool) {
	st := u.sessions.ssoSnapshot(sess)
	if now := u.now(); now.Sub(st.checked) >= ssoRecheck {
		if u.a.sso == nil {
			return nil, false
		}
		v, err := u.a.sso.RevalidateSession(r.Context(), st.handle)
		if err != nil || !v.Active || v.Subject != sess.subject {
			return nil, false
		}
		perms := workPerms(v.Permissions)
		if len(perms) == 0 {
			return nil, false
		}
		u.sessions.ssoUpdate(sess, perms, v.Name, now)
		st.perms = perms
	}
	return &Principal{Subject: sess.subject, Kind: "personal", Permissions: st.perms}, true
}

// ticketPage is where the admin UI sends the browser. It never consumes the ticket.
func (u *webUI) ticketPage(c *uiCtx) {
	if u.a.sso == nil {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	t := c.r.URL.Query().Get("t")
	if !modclient.TicketShaped(t) {
		u.renderLogin(c, http.StatusBadRequest, "login.ticket_invalid", "")
		return
	}
	tok := randToken(24)
	u.setCookie(c.w, c.r, u.loginCookieName(c.r), tok, 3600)
	b := c.base(msg("ticket.title"), "")
	b.CSRF = tok
	b.D = &uiTicketView{Ticket: t}
	c.render(http.StatusOK, "ticket", b)
}

// ticketPost redeems the ticket. Failures are counted per client address (as login failures are)
// and all look the same; a valid ticket is never throttled.
func (u *webUI) ticketPost(c *uiCtx) {
	r := c.r
	if u.a.sso == nil {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	ck, err := r.Cookie(u.loginCookieName(r))
	if err != nil || ck.Value == "" || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(r.PostFormValue("csrf"))) != 1 {
		u.renderLogin(c, http.StatusForbidden, "login.csrf", "")
		return
	}
	refuse := func() {
		if ok, wait := u.a.failures.allow(clientIP(r, u.a.trustProxy)); !ok {
			select {
			case <-time.After(u.a.penalty):
			case <-r.Context().Done():
			}
			c.w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			u.renderLogin(c, http.StatusTooManyRequests, "login.locked", "")
			return
		}
		u.renderLogin(c, http.StatusUnauthorized, "login.ticket_invalid", "")
	}
	ls, err := u.a.sso.RedeemTicket(r.Context(), r.PostFormValue("ticket"))
	if err != nil {
		if errors.Is(err, modclient.ErrUnavailable) {
			u.renderLogin(c, http.StatusServiceUnavailable, "login.sso_unavailable", "")
			return
		}
		refuse()
		return
	}
	perms := workPerms(ls.Permissions)
	if !ls.Active || !canSubjectRe.MatchString(ls.Subject) || len(perms) == 0 || ls.Session == "" {
		refuse()
		return
	}
	// A session id sent along is never adopted, and the one it names is ended.
	if old, err := r.Cookie(u.sessionCookieName(r)); err == nil {
		u.sessions.delete(old.Value)
	}
	id, _ := u.sessions.createSSO(ls.Subject, ls.Name, ls.Session, perms)
	u.setCookie(c.w, r, u.sessionCookieName(r), id, int(u.sessions.abs.Seconds()))
	u.setCookie(c.w, r, u.loginCookieName(r), "", -1)
	// Land on a clean URL: the ticket is not in it, and the place to land is checked here again.
	http.Redirect(c.w, r, ssoLanding(ls.ReturnPath), http.StatusSeeOther)
}

// returnRe is the allowlist for the landing place can-core passes on: a slash, then letters,
// digits, slashes, underscores and hyphens. No dots, query, fragment, scheme or host.
var returnRe = regexp.MustCompile(`^/[A-Za-z0-9/_-]{0,100}$`)

// ssoLanding turns the return path (relative to /ui) into a local URL; anything unexpected lands on
// the home page.
func ssoLanding(ret string) string {
	if ret == "" || ret == "/" || !returnRe.MatchString(ret) || strings.Contains(ret, "//") {
		return "/ui/"
	}
	if n, ok := safeNext("/ui" + ret); ok {
		return n
	}
	return "/ui/"
}
