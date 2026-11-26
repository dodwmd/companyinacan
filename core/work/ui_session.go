package work

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/uikit/session"
)

// Sessions of the web UI are those of core/uikit/session. A token session is authenticated again on
// every request (through the introspection cache), so revoking the token, an expiry or a permission
// change takes effect within that cache's lifetime instead of when the session ends; a
// launch-ticket session asks can-core again every ssoRecheck. This file says who may sign in.

const (
	uiIdle          = 8 * time.Hour
	uiAbsolute      = 24 * time.Hour
	uiMaxSessions   = 5000
	uiMaxPerSubject = 20
	uiLoginCSRFAge  = 3600
)

type uiSession = session.Session[Principal]

// ssoRecheck is how often a launch-ticket session asks can-core again: as often as a personal access
// token is re-introspected here (its cache lifetime), so a removed role or a disabled account ends
// the module session within the same half minute, privileged or not.
const ssoRecheck = 30 * time.Second

// newSessions builds the session table. u.a is only used per request.
func (u *webUI) newSessions() *session.Manager[Principal] {
	return session.New(session.Config[Principal]{
		Module: "work", SessionCookie: "work_session", CSRFCookie: "work_login_csrf",
		Secure:   u.secure,
		Now:      u.now,
		Timeouts: func() (time.Duration, time.Duration) { return uiIdle, uiAbsolute },
		Max:      uiMaxSessions, PerSubject: uiMaxPerSubject,
		CSRFMaxAge: uiLoginCSRFAge, RotateCSRF: true,
		Recheck: func(_ Principal, sso bool) time.Duration {
			if sso {
				return ssoRecheck
			}
			return 0
		},
		Subject: func(p Principal) string { return p.Subject },
		Reauth: func(r *http.Request, token string) (Principal, error) {
			p, err := u.principalFor(r, token)
			if err == nil && !p.Can(PermRead) {
				err = ErrForbidden
			}
			if err != nil {
				return Principal{}, err
			}
			return *p, nil
		},
		SSO: func() modclient.SessionAPI { return u.a.sso },
		Grant: func(subject string, permissions []string) (Principal, bool) {
			p := Principal{Subject: subject, Kind: "personal", Permissions: workPerms(permissions)}
			return p, len(p.Permissions) > 0 && p.Can(PermRead)
		},
		ValidSubject: canSubjectRe.MatchString,
	})
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("work: no randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
