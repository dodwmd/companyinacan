package files

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// Sessions of the web UI (specs/050-files-ui) are those of core/uikit/session. A session begins
// with a launch ticket from the admin UI, or with a token pasted at /login that the Authenticator
// chain of the JSON API accepts. This file says who may sign in and how often that is asked again.

const (
	uiDefaultIdle     = session.DefaultIdle
	uiDefaultAbsolute = session.DefaultAbsolute
	uiRecheckDefault  = session.DefaultRecheck
	uiRecheckPrivil   = session.PrivilegedRecheck
	uiMaxSessions     = 2000
	uiMaxPerSubject   = 5
	uiSessionCookie   = "can_files_session"
	uiLoginCSRFCookie = "can_files_login_csrf"
	uiLoginCSRFMaxAge = 15 * 60
	uiFailedLogins    = 10
)

// uiSession is a signed-in browser. Its principal never carries groups: those are resolved on every
// request.
type uiSession = session.Session[Principal]

// errNotAccepted is a token that signs nobody in here.
var errNotAccepted = errors.New("token not accepted")

func (u *UI) newSessions() *session.Manager[Principal] {
	// reauth turns "not valid" (a nil principal) into an error.
	reauth := func(p *Principal, err error) (Principal, error) {
		if err == nil && p == nil {
			err = errNotAccepted
		}
		if err != nil {
			return Principal{}, err
		}
		return *p, nil
	}
	return session.New(session.Config[Principal]{
		Module: "files", SessionCookie: uiSessionCookie, CSRFCookie: uiLoginCSRFCookie,
		Secure:   func(*http.Request) bool { return u.Secure },
		Now:      func() time.Time { return u.Now() },
		Timeouts: func() (time.Duration, time.Duration) { return u.IdleTimeout, u.AbsoluteTimeout },
		Max:      uiMaxSessions, PerSubject: uiMaxPerSubject, CSRFMaxAge: uiLoginCSRFMaxAge,
		Recheck: func(p Principal, _ bool) time.Duration { return u.recheckFor(&p) },
		Subject: func(p Principal) string { return p.Subject },
		Reauth: func(r *http.Request, token string) (Principal, error) {
			return reauth(u.authenticateToken(r.Context(), token))
		},
		Fresh: func(r *http.Request, token string) (Principal, error) {
			return reauth(u.authenticateNow(r.Context(), token))
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

// authenticateToken runs the API's Authenticator over a pasted token. The restore-only credential
// never signs in: it opens the purge hook and nothing else.
func (u *UI) authenticateToken(ctx context.Context, token string) (*Principal, error) {
	return u.checkToken(ctx, token, u.Auth.Authenticate)
}

// authenticateNow is authenticateToken without any cached answer, for a step-up.
func (u *UI) authenticateNow(ctx context.Context, token string) (*Principal, error) {
	if f, ok := u.Auth.(freshAuthenticator); ok {
		return u.checkToken(ctx, token, f.AuthenticateFresh)
	}
	return u.authenticateToken(ctx, token)
}

func (u *UI) checkToken(ctx context.Context, token string, check func(context.Context, string) (*Principal, error)) (*Principal, error) {
	if token == "" || len(token) > session.MaxTokenLen || strings.ContainsAny(token, " \r\n\t") {
		return nil, nil
	}
	p, err := check(ctx, token)
	if err != nil || p == nil || p.Restore || !p.Has(PermRead) {
		return nil, err
	}
	return p, nil
}

// recheckFor is how often a session re-authenticates: every minute for an administrator,
// RecheckEvery for everyone else.
func (u *UI) recheckFor(p *Principal) time.Duration {
	return session.Interval(u.RecheckEvery, p.Has(PermAdmin))
}
