package people

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// Sessions of the web UI are those of core/uikit/session. The user presents a personal access token
// once at /login, authenticated by the same Authenticator chain the JSON API uses, or arrives with
// a launch ticket. This file says who may sign in and how often that is asked again. The admin UI's
// session cookie is not a credential here and is never read.

const (
	defaultIdle      = session.DefaultIdle
	defaultAbsolute  = session.DefaultAbsolute
	defaultRecheck   = session.DefaultRecheck
	maxSessions      = 5000
	maxPerSubject    = 5
	maxTokenLen      = session.MaxTokenLen
	sessionCookie    = "can_people_session"
	loginCSRFCookie  = "can_people_login_csrf"
	loginCSRFMaxAge  = 15 * 60
	failedLoginBurst = 10
)

type uiSession = session.Session[Principal]

func (u *UI) newSessions() *session.Manager[Principal] {
	return session.New(session.Config[Principal]{
		Module: "people", SessionCookie: sessionCookie, CSRFCookie: loginCSRFCookie,
		Secure:   func(*http.Request) bool { return u.Secure },
		Now:      func() time.Time { return u.Now() },
		Timeouts: func() (time.Duration, time.Duration) { return u.IdleTimeout, u.AbsoluteTimeout },
		Max:      maxSessions, PerSubject: maxPerSubject, CSRFMaxAge: loginCSRFMaxAge,
		Recheck: func(p Principal, _ bool) time.Duration { return u.recheckFor(p) },
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
			p := principalFromPermissions(subject, perms)
			if p == nil {
				return Principal{}, false
			}
			return *p, true
		},
		ValidSubject: subjectRe.MatchString,
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

// current returns the live session of the request and the caller resolved through the Service:
// every request checks that the person is still active staff. nil means "log in".
func (u *UI) current(w http.ResponseWriter, r *http.Request) (*uiSession, Actor, bool) {
	ss, p, ok := u.sessions.Current(w, r)
	if !ok {
		return nil, Actor{}, false
	}
	a, err := u.Svc.ResolveActor(r.Context(), &p)
	if err != nil || !a.HasPerson || !a.Active() {
		u.sessions.End(w, r)
		return nil, Actor{}, false
	}
	return ss, a, true
}

// recheckFor is how often a session re-authenticates: every minute for a session holding manager,
// hr or admin (a demoted or revoked approver or HR user must not keep approving or revealing other
// people's data for long), RecheckEvery for everyone else.
func (u *UI) recheckFor(p Principal) time.Duration {
	return session.Interval(u.RecheckEvery, slices.ContainsFunc(p.Roles, func(r string) bool { return r == RoleManager || r == RoleHR || r == RoleAdmin }))
}
