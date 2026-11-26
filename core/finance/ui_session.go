package finance

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// Sessions of the web UI (specs/031-finance-ui) are those of core/uikit/session. The user presents
// a personal access token once at /login, authenticated by the same Authenticator chain the JSON
// API uses, or arrives with a launch ticket. This file says who may sign in and how often that is
// asked again.

const (
	uiDefaultIdle     = session.DefaultIdle
	uiDefaultAbsolute = session.DefaultAbsolute
	uiRecheckDefault  = session.DefaultRecheck
	uiRecheckPrivil   = session.PrivilegedRecheck
	uiMaxSessions     = 5000
	uiMaxPerSubject   = 5
	uiSessionCookie   = "can_finance_session"
	uiLoginCSRFCookie = "can_finance_login_csrf"
	uiLoginCSRFMaxAge = 15 * 60
	uiFailedLogins    = 10
)

type uiSession = session.Session[Principal]

func (u *UI) newSessions() *session.Manager[Principal] {
	value := func(p *Principal, err error) (Principal, error) {
		if err != nil {
			return Principal{}, err
		}
		return *p, nil
	}
	return session.New(session.Config[Principal]{
		Module: "finance", SessionCookie: uiSessionCookie, CSRFCookie: uiLoginCSRFCookie,
		Secure:   func(*http.Request) bool { return u.Secure },
		Now:      func() time.Time { return u.Now() },
		Timeouts: func() (time.Duration, time.Duration) { return u.IdleTimeout, u.AbsoluteTimeout },
		Max:      uiMaxSessions, PerSubject: uiMaxPerSubject, CSRFMaxAge: uiLoginCSRFMaxAge,
		Recheck: func(p Principal, _ bool) time.Duration { return u.recheckFor(&p) },
		Subject: func(p Principal) string { return p.Subject },
		Reauth: func(r *http.Request, token string) (Principal, error) {
			return value(u.authenticateToken(r, token))
		},
		Fresh: func(r *http.Request, token string) (Principal, error) {
			return value(u.authenticateNow(r, token))
		},
		SSO: func() modclient.SessionAPI { return u.Sessions },
		Grant: func(subject string, perms []string) (Principal, bool) {
			p, ok := personalPrincipal(subject, perms)
			if !ok {
				return Principal{}, false
			}
			return *p, true
		},
	})
}

// authenticateToken runs the shared Authenticator over a bearer token, as if it came in an
// Authorization header of the API.
func (u *UI) authenticateToken(r *http.Request, token string) (*Principal, error) {
	return u.checkToken(r, token, u.Auth.Authenticate)
}

// authenticateNow is authenticateToken without any cached answer, for a step-up.
func (u *UI) authenticateNow(r *http.Request, token string) (*Principal, error) {
	if f, ok := u.Auth.(freshAuthenticator); ok {
		return u.checkToken(r, token, f.AuthenticateFresh)
	}
	return u.authenticateToken(r, token)
}

func (u *UI) checkToken(r *http.Request, token string, check func(*http.Request) (*Principal, error)) (*Principal, error) {
	if token == "" || len(token) > session.MaxTokenLen || strings.ContainsAny(token, " \r\n\t") {
		return nil, ErrUnauthenticated
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(r.Context())
	req.RemoteAddr = r.RemoteAddr
	req.Header.Set("Authorization", "Bearer "+token)
	p, err := check(req)
	if err != nil || p == nil {
		return nil, ErrUnauthenticated
	}
	return p, nil
}

// recheckFor is how often a session re-authenticates: every minute for a session that can approve
// or administer, RecheckEvery for everyone else.
func (u *UI) recheckFor(p *Principal) time.Duration {
	return session.Interval(u.RecheckEvery, p.Can(PermApprove) || p.Can(PermAdmin) || p.Can(PermLedgerApprove) || p.Can(PermLedgerAdmin))
}
