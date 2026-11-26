package files

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// Single sign-on from the admin UI (specs/036-module-ui-sso): the flow is core/uikit/session's.

// uiStepUpMaxAge is how recent the person's authentication at the admin UI must be for a
// launch-ticket session to do something irreversible.
const uiStepUpMaxAge = session.StepUpMaxAge

// stepUpSSO is the step-up of a launch-ticket session, which has no token to type again: can-core
// is asked now what the person holds, and must say that the person authenticated recently.
func (u *UI) stepUpSSO(c *uiCtx) error {
	p, err := u.sessions.StepUp(c.r, c.ss, "", func(p Principal) bool { return p.Has(PermAdmin) })
	if errors.Is(err, session.ErrStepUpStale) {
		return formError{"erase.stepup_stale"}
	}
	if err != nil {
		return formError{"erase.stepup_unconfirmed"}
	}
	p.Groups = c.p.Groups
	c.p = &p
	return nil
}

// launchPages draws the outcomes of the ticket requests.
func (u *UI) launchPages(w http.ResponseWriter, r *http.Request) session.LaunchPages {
	return session.LaunchPages{
		SignIn: func(status int, key string) { u.loginForm(w, r, status, u.msg(key)) },
		Confirm: func(status int, v session.TicketView) {
			u.render(w, r, nil, status, "ticket.html", "ticket.title", v)
		},
		Failed: func() (string, bool) {
			ip := clientIP(r, u.TrustProxy)
			u.failIP.fail(ip)
			return "60", u.failIP.blocked(ip)
		},
	}
}

// ticketPage is where the admin UI sends the browser. It never consumes the ticket.
func (u *UI) ticketPage(w http.ResponseWriter, r *http.Request) {
	if u.Sessions == nil {
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	u.sessions.TicketPage(w, r, u.launchPages(w, r))
}

// ticketPost redeems the ticket.
func (u *UI) ticketPost(w http.ResponseWriter, r *http.Request) {
	if u.Sessions == nil {
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", "err.not_found")
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderError(w, r, nil, http.StatusBadRequest, "err.invalid_title", "err.bad_form")
		return
	}
	u.sessions.TicketPost(w, r, u.launchPages(w, r), nil, ssoLanding)
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
