package equity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// Sessions of the web UI are those of core/uikit/session (specs/058). A session begins with a
// launch ticket from the admin UI, or with a token pasted at /login that the Authenticator chain
// of the JSON API accepts. This file says who may sign in, how often that is asked again, and how
// an irreversible action is confirmed. The service account is no person and never signs in.

const (
	uiMaxSessions     = 2000
	uiMaxPerSubject   = 5
	uiSessionCookie   = "can_equity_session"
	uiLoginCSRFCookie = "can_equity_login_csrf"
	uiLoginCSRFMaxAge = 15 * 60
)

type uiSession = session.Session[Principal]

var errNotAccepted = errors.New("token not accepted")

func (u *UI) newSessions() *session.Manager[Principal] {
	return session.New(session.Config[Principal]{
		Module: "equity", SessionCookie: uiSessionCookie, CSRFCookie: uiLoginCSRFCookie,
		Secure:   func(*http.Request) bool { return u.Secure },
		Now:      func() time.Time { return u.Now() },
		Timeouts: func() (time.Duration, time.Duration) { return u.IdleTimeout, u.AbsoluteTimeout },
		Max:      uiMaxSessions, PerSubject: uiMaxPerSubject, CSRFMaxAge: uiLoginCSRFMaxAge,
		Recheck: func(p Principal, _ bool) time.Duration { return u.recheckFor(p) },
		Subject: func(p Principal) string { return p.Subject },
		Reauth:  u.authenticateToken,
		Fresh:   u.authenticateNow,
		SSO:     func() modclient.SessionAPI { return u.Sessions },
		Grant: func(subject string, perms []string) (Principal, bool) {
			p, ok := personalPrincipal(subject, perms)
			if !ok {
				return Principal{}, false
			}
			return *p, true
		},
		ValidSubject: canSubjectRe.MatchString,
	})
}

// authenticateToken runs the shared Authenticator over a pasted token.
func (u *UI) authenticateToken(r *http.Request, token string) (Principal, error) {
	return u.checkToken(r, token, u.Auth.Authenticate)
}

// authenticateNow is authenticateToken without any cached answer, for a step-up.
func (u *UI) authenticateNow(r *http.Request, token string) (Principal, error) {
	if f, ok := u.Auth.(freshAuthenticator); ok {
		return u.checkToken(r, token, f.AuthenticateFresh)
	}
	return u.authenticateToken(r, token)
}

func (u *UI) checkToken(r *http.Request, token string, check func(*http.Request) (*Principal, error)) (Principal, error) {
	if token == "" || len(token) > session.MaxTokenLen || strings.ContainsAny(token, " \r\n\t") {
		return Principal{}, errNotAccepted
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(r.Context())
	req.RemoteAddr = r.RemoteAddr
	req.Header.Set("Authorization", "Bearer "+token)
	p, err := check(req)
	if err != nil || p == nil || p.Service || len(p.Roles) == 0 {
		return Principal{}, errNotAccepted
	}
	return *p, nil
}

// recheckFor is how often a session re-authenticates: every minute for one that sees other
// people's equity, RecheckEvery for a holder.
func (u *UI) recheckFor(p Principal) time.Duration {
	return session.Interval(u.RecheckEvery, slices.ContainsFunc(p.Roles, func(r string) bool { return r != RoleEmployee && r != RoleManager }))
}

// current returns the live session of the request and the caller as the Service resolves them now
// (with a directory, the person the account is linked to at this moment).
func (u *UI) current(w http.ResponseWriter, r *http.Request) (*uiCtx, bool) {
	ss, p, ok := u.sessions.Current(w, r)
	if !ok {
		return nil, false
	}
	a, err := u.Svc.ResolveActor(r.Context(), &p)
	if err != nil {
		u.sessions.End(w, r)
		return nil, false
	}
	return &uiCtx{ss: ss, a: a, caps: u.Svc.Capabilities(a), r: r, w: w}, true
}

// stepUp confirms who is at the keyboard before something that cannot be undone, and returns the
// caller as they are now, not as a cache remembers them: the Service call that follows is made as
// that caller. A token session types its token again; a launch-ticket session needs an admin
// sign-in of the last few minutes, confirmed by can-core. The form must also be ticked.
func (u *UI) stepUp(c *uiCtx) (Actor, error) {
	if c.form("confirm") != "yes" {
		return Actor{}, &uiError{http.StatusBadRequest, "stepup.confirm_required"}
	}
	typed := ""
	if !c.ss.SSO() {
		typed = c.form("token")
	}
	p, err := u.sessions.StepUp(c.r, c.ss, typed, nil)
	switch {
	case errors.Is(err, session.ErrStepUpReenter):
		return Actor{}, &uiError{http.StatusBadRequest, "stepup.reenter"}
	case errors.Is(err, session.ErrStepUpStale):
		return Actor{}, &uiError{http.StatusForbidden, "stepup.stale"}
	case err != nil:
		return Actor{}, &uiError{http.StatusForbidden, "stepup.unconfirmed"}
	}
	return u.Svc.ResolveActor(c.ctx(), &p)
}

// ---- single sign-on from the admin UI (specs/036-module-ui-sso) ------------------------------

func (u *UI) launchPages(w http.ResponseWriter, r *http.Request) session.LaunchPages {
	return session.LaunchPages{
		SignIn: func(status int, key string) { u.loginForm(w, r, status, u.msg(key)) },
		Confirm: func(status int, v session.TicketView) {
			u.render(w, r, nil, status, "ticket.html", "ticket.title", v, "")
		},
		Failed: func() (string, bool) { return "10", !u.failIP.allow(u.clientHost(r)) },
	}
}

// ticketPage is where the admin UI sends the browser. It never consumes the ticket.
func (u *UI) ticketPage(w http.ResponseWriter, r *http.Request) {
	if u.Sessions == nil {
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", u.msg("err.not_found"))
		return
	}
	u.sessions.TicketPage(w, r, u.launchPages(w, r))
}

// ticketPost redeems the ticket.
func (u *UI) ticketPost(w http.ResponseWriter, r *http.Request) {
	modclient.LimitPublicForm(w, r) // anyone can post here
	if u.Sessions == nil {
		u.renderError(w, r, nil, http.StatusNotFound, "err.not_found_title", u.msg("err.not_found"))
		return
	}
	if err := r.ParseForm(); err != nil {
		u.renderError(w, r, nil, http.StatusBadRequest, "err.invalid_title", u.msg("err.bad_form"))
		return
	}
	u.sessions.TicketPost(w, r, u.launchPages(w, r), nil, ssoLanding)
}

// returnRe is the allowlist for the landing place can-core passes on: a slash, then letters,
// digits, slashes, underscores and hyphens. No dots, query, fragment, scheme or host.
var returnRe = regexp.MustCompile(`^/[A-Za-z0-9/_-]{0,100}$`)

func ssoLanding(ret string) string {
	if !returnRe.MatchString(ret) || strings.Contains(ret, "//") {
		return "/"
	}
	return ret
}
