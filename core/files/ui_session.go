package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Sessions of the web UI (specs/050-files-ui). A session begins with a launch ticket from the admin
// UI, or with a token pasted at /login that the Authenticator chain of the JSON API accepts. The
// server keeps it, keyed by the SHA-256 of a random identifier; the browser holds only that
// identifier, in an HttpOnly, SameSite=Strict cookie. A pasted token is kept in memory (never
// written to disk, never sent to the browser again) so that revocation is noticed: every recheck
// interval the session asks again, and a credential that no longer authenticates ends it.

const (
	uiDefaultIdle     = 30 * time.Minute
	uiDefaultAbsolute = 8 * time.Hour
	uiRecheckDefault  = 5 * time.Minute
	uiRecheckPrivil   = time.Minute // administrators: a revoked one must not hold or erase for long
	uiMaxSessions     = 2000
	uiMaxPerSubject   = 5
	uiMaxTokenLen     = 4096
	uiSessionCookie   = "can_files_session"
	uiLoginCSRFCookie = "can_files_login_csrf"
	uiHostPrefix      = "__Host-"
	uiLoginCSRFMaxAge = 15 * 60
	uiFailedLogins    = 10
)

type uiSession struct {
	subject string
	princ   Principal // never carries groups: those are resolved on every request
	token   string
	csrf    string
	created time.Time
	last    time.Time
	checked time.Time
	// ssoHandle is set for a session that began with a launch ticket (specs/036-module-ui-sso):
	// there is no token, only the opaque handle can-core answers revalidation for. name is the
	// person's display name from can-core. last, checked, princ and name are guarded by the
	// store's mutex.
	ssoHandle string
	name      string
	// ssoExpires is can-core's session_expires_at (the identity provider's deadline caps it): the
	// session ends then, whatever the timeouts say. Set at creation, never changed.
	ssoExpires time.Time
}

// pastDeadline reports whether a launch-ticket session has reached can-core's session_expires_at.
func (ss *uiSession) pastDeadline(now time.Time) bool {
	return ss.ssoHandle != "" && !now.Before(ss.ssoExpires)
}

type sessionStore struct {
	mu sync.Mutex
	m  map[[sha256.Size]byte]*uiSession
}

func newToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func sidKey(id string) [sha256.Size]byte { return sha256.Sum256([]byte(id)) }

// add stores a new session and returns its identifier. The oldest sessions of the subject, and the
// oldest overall, are dropped when the bounds are exceeded.
func (s *sessionStore) add(ss *uiSession, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[[sha256.Size]byte]*uiSession{}
	}
	id := newToken()
	ss.csrf, ss.created, ss.last, ss.checked = newToken(), now, now, now
	s.m[sidKey(id)] = ss
	dropOldest := func(of string, limit int) {
		for {
			var oldest [sha256.Size]byte
			var at time.Time
			n := 0
			for k, v := range s.m {
				if of != "" && v.subject != of {
					continue
				}
				if n == 0 || v.created.Before(at) {
					oldest, at = k, v.created
				}
				n++
			}
			if n <= limit {
				return
			}
			delete(s.m, oldest)
		}
	}
	dropOldest(ss.subject, uiMaxPerSubject)
	dropOldest("", uiMaxSessions)
	return id
}

func (s *sessionStore) get(id string) *uiSession {
	if id == "" || len(id) > 128 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[sidKey(id)]
}

func (s *sessionStore) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, sidKey(id))
}

func (u *UI) sessionCookieName() string {
	if u.Secure {
		return uiHostPrefix + uiSessionCookie
	}
	return uiSessionCookie
}

func (u *UI) loginCSRFName() string {
	if u.Secure {
		return uiHostPrefix + uiLoginCSRFCookie
	}
	return uiLoginCSRFCookie
}

func (u *UI) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: u.Secure, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
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
	if token == "" || len(token) > uiMaxTokenLen || strings.ContainsAny(token, " \r\n\t") {
		return nil, nil
	}
	p, err := check(ctx, token)
	if err != nil || p == nil || p.Restore || !p.Has(PermRead) {
		return nil, err
	}
	return p, nil
}

// current returns the live session of the request, enforcing the idle and absolute timeouts and
// the periodic re-authentication. false means "sign in".
func (u *UI) current(w http.ResponseWriter, r *http.Request) (*uiSession, Principal, bool) {
	ck, err := r.Cookie(u.sessionCookieName())
	if err != nil {
		return nil, Principal{}, false
	}
	kill := func() (*uiSession, Principal, bool) {
		u.sessions.drop(ck.Value)
		u.setCookie(w, u.sessionCookieName(), "", -1)
		return nil, Principal{}, false
	}
	ss := u.sessions.get(ck.Value)
	if ss == nil {
		return kill()
	}
	now := u.Now()
	// Snapshot the mutable fields under the lock: concurrent requests of one session write them.
	u.sessions.mu.Lock()
	last, created, checked, princ, token, handle := ss.last, ss.created, ss.checked, ss.princ, ss.token, ss.ssoHandle
	u.sessions.mu.Unlock()
	if now.Sub(last) > u.IdleTimeout || now.Sub(created) > u.AbsoluteTimeout || ss.pastDeadline(now) {
		return kill()
	}
	recheck := now.Sub(checked) > u.recheckFor(&princ)
	name := ""
	if recheck {
		var p *Principal
		if handle != "" {
			var ok bool
			if p, name, ok = u.revalidateSSO(r.Context(), handle, ss.subject); !ok {
				return kill()
			}
		} else {
			var err error
			if p, err = u.authenticateToken(r.Context(), token); err != nil || p == nil || p.Subject != ss.subject {
				return kill()
			}
		}
		princ = *p
	}
	u.sessions.mu.Lock()
	ss.last = now
	if recheck {
		ss.checked, ss.princ = now, princ
		if name != "" {
			ss.name = name
		}
	}
	u.sessions.mu.Unlock()
	return ss, princ, true
}

func constEq(a, b string) bool {
	return a != "" && len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// recheckFor is how often a session re-authenticates: every minute for an administrator,
// RecheckEvery for everyone else. This only picks an interval; it grants nothing.
func (u *UI) recheckFor(p *Principal) time.Duration {
	if p.Has(PermAdmin) {
		return min(u.RecheckEvery, uiRecheckPrivil)
	}
	return u.RecheckEvery
}
