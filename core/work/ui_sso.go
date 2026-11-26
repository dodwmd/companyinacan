package work

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// Single sign-on from the admin UI (specs/036-module-ui-sso): the flow is core/uikit/session's.

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

// launchPages draws the outcomes of the ticket requests.
func (u *webUI) launchPages(c *uiCtx) session.LaunchPages {
	return session.LaunchPages{
		SignIn: func(status int, key string) { u.renderLogin(c, status, key, "") },
		Confirm: func(status int, v session.TicketView) {
			b := c.base(msg("ticket.title"), "")
			b.CSRF = v.CSRF
			b.D = &v
			c.render(status, "ticket", b)
		},
		// refusals share the API's failure budget of the address, and its penalty
		Failed: func() (string, bool) {
			ok, wait := u.a.failures.allow(clientIP(c.r, u.a.trustProxy))
			if ok {
				return "", false
			}
			select {
			case <-time.After(u.a.penalty):
			case <-c.r.Context().Done():
			}
			return strconv.Itoa(int(wait.Seconds()) + 1), true
		},
	}
}

// ticketPage is where the admin UI sends the browser. It never consumes the ticket.
func (u *webUI) ticketPage(c *uiCtx) {
	if u.a.sso == nil {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	u.sessions.TicketPage(c.w, c.r, u.launchPages(c))
}

// ticketPost redeems the ticket.
func (u *webUI) ticketPost(c *uiCtx) {
	if u.a.sso == nil {
		c.errorPage(http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	u.sessions.TicketPost(c.w, c.r, u.launchPages(c), nil, ssoLanding)
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
