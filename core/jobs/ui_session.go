package jobs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// Sessions of the web UI are those of core/uikit/session. The user presents a personal access token
// once at /login, authenticated by the same Authenticator chain the API uses, or arrives with a
// launch ticket. This file says who may sign in and how often that is asked again.

const (
	defaultIdle      = session.DefaultIdle
	defaultAbsolute  = session.DefaultAbsolute
	defaultRecheck   = session.DefaultRecheck
	maxSessions      = 5000
	maxPerSubject    = 5
	maxTokenLen      = session.MaxTokenLen
	sessionCookie    = "can_jobs_session"
	loginCSRFCookie  = "can_jobs_login_csrf"
	loginCSRFMaxAge  = 15 * 60
	failedLoginBurst = 10
)

type uiSession = session.Session[Principal]

func (u *UI) newSessions() *session.Manager[Principal] {
	return session.New(session.Config[Principal]{
		Module: "jobs", SessionCookie: sessionCookie, CSRFCookie: loginCSRFCookie,
		Secure:   func(*http.Request) bool { return u.Secure },
		Now:      func() time.Time { return u.Now() },
		Timeouts: func() (time.Duration, time.Duration) { return u.IdleTimeout, u.AbsoluteTimeout },
		Max:      maxSessions, PerSubject: maxPerSubject, CSRFMaxAge: loginCSRFMaxAge,
		Recheck: func(p Principal, _ bool) time.Duration { return u.recheckFor(&p) },
		Subject: func(p Principal) string { return p.Subject },
		Reauth: func(r *http.Request, token string) (Principal, error) {
			p, err := u.authenticateToken(r, token)
			if err != nil {
				return Principal{}, err
			}
			return *p, nil
		},
		SSO: func() modclient.SessionAPI { return u.Sessions },
		Grant: func(subject string, perms []string) (Principal, bool) {
			p := personalPrincipal(subject, perms)
			if p == nil {
				return Principal{}, false
			}
			return *p, true
		},
	})
}

// authenticateToken runs the shared Authenticator over a bearer token, as if it came in an
// Authorization header of the API.
func (u *UI) authenticateToken(r *http.Request, token string) (*Principal, error) {
	if token == "" || len(token) > maxTokenLen || strings.ContainsAny(token, " \r\n\t") {
		return nil, ErrUnauthenticated
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(r.Context())
	req.RemoteAddr = r.RemoteAddr
	req.Header.Set("Authorization", "Bearer "+token)
	p, err := u.Auth.Authenticate(req)
	if err != nil || p == nil {
		return nil, ErrUnauthenticated
	}
	return p, nil
}

// recheckFor is how often a session re-authenticates: every minute for a session that can write
// requisitions, move candidates or read diversity reports (a demoted or revoked recruiter must not
// keep reading candidate data for long), RecheckEvery for the rest.
func (u *UI) recheckFor(p *Principal) time.Duration {
	c := p.Caps()
	return session.Interval(u.RecheckEvery, c.WriteJobs || c.Reports || c.EditCandidate || c.HireApps || c.ReadCandidate)
}
