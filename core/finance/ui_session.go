package finance

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Sessions of the web UI (specs/031-finance-ui). The user presents a personal access token once at
// /login; the server authenticates it with the same Authenticator chain the JSON API uses and keeps
// a server-side session keyed by the SHA-256 of a random identifier. The browser holds only that
// identifier, in an HttpOnly, SameSite=Strict cookie. The token is kept in memory (never written to
// disk, never sent to the browser again) so that revocation is noticed: every recheck interval the
// session asks the Authenticator again, and a token that no longer authenticates ends the session.

const (
	uiDefaultIdle     = 30 * time.Minute
	uiDefaultAbsolute = 8 * time.Hour
	uiRecheckDefault  = 5 * time.Minute
	uiRecheckPrivil   = time.Minute // approvers and admins: a revoked approver must not approve for long
	uiMaxSessions     = 5000
	uiMaxPerSubject   = 5
	uiMaxTokenLen     = 4096
	uiSessionCookie   = "can_finance_session"
	uiLoginCSRFCookie = "can_finance_login_csrf"
	uiHostPrefix      = "__Host-"
	uiLoginCSRFMaxAge = 15 * 60
	uiFailedLogins    = 10
)

type uiSession struct {
	subject string
	princ   Principal
	token   string
	csrf    string
	created time.Time
	last    time.Time
	checked time.Time
	// ssoHandle is set for a session that began with a launch ticket (specs/036-module-ui-sso):
	// there is no token, only the opaque handle can-core answers revalidation for. name is the
	// person's display name from can-core. Both are guarded by the store's mutex.
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

// create stores a new session and returns its identifier. The oldest sessions of the subject, and
// the oldest overall, are dropped when the bounds are exceeded.
func (s *sessionStore) create(p Principal, token string, now time.Time) (string, *uiSession) {
	return s.add(&uiSession{subject: p.Subject, princ: p, token: token}, now)
}

// createSSO is create for a launch-ticket session: no token, the can-core session handle instead.
func (s *sessionStore) createSSO(p Principal, handle, name string, expires, now time.Time) (string, *uiSession) {
	return s.add(&uiSession{subject: p.Subject, princ: p, ssoHandle: handle, name: name, ssoExpires: expires}, now)
}

func (s *sessionStore) add(ss *uiSession, now time.Time) (string, *uiSession) {
	p := ss.princ
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[[sha256.Size]byte]*uiSession{}
	}
	id := newToken()
	ss.csrf, ss.created, ss.last, ss.checked = newToken(), now, now, now
	s.m[sidKey(id)] = ss
	var own [][sha256.Size]byte
	for k, v := range s.m {
		if v.subject == p.Subject {
			own = append(own, k)
		}
	}
	for len(own) > uiMaxPerSubject {
		oldest := 0
		for i, k := range own {
			if s.m[k].created.Before(s.m[own[oldest]].created) {
				oldest = i
			}
		}
		delete(s.m, own[oldest])
		own = append(own[:oldest], own[oldest+1:]...)
	}
	for len(s.m) > uiMaxSessions {
		var ok [sha256.Size]byte
		var oc time.Time
		first := true
		for k, v := range s.m {
			if first || v.created.Before(oc) {
				ok, oc, first = k, v.created, false
			}
		}
		delete(s.m, ok)
	}
	return id, ss
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

// authenticateToken runs the shared Authenticator over a bearer token, as if it came in an
// Authorization header of the API.
func (u *UI) authenticateToken(r *http.Request, token string) (*Principal, error) {
	if token == "" || len(token) > uiMaxTokenLen || strings.ContainsAny(token, " \r\n\t") {
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

// current returns the live session of the request, enforcing the idle and absolute timeouts and
// the periodic re-authentication. false means "log in".
func (u *UI) current(w http.ResponseWriter, r *http.Request) (*uiSession, Principal, bool) {
	ck, err := r.Cookie(u.sessionCookieName())
	if err != nil {
		return nil, Principal{}, false
	}
	ss := u.sessions.get(ck.Value)
	if ss == nil {
		u.setCookie(w, u.sessionCookieName(), "", -1)
		return nil, Principal{}, false
	}
	now := u.Now()
	kill := func() (*uiSession, Principal, bool) {
		u.sessions.drop(ck.Value)
		u.setCookie(w, u.sessionCookieName(), "", -1)
		return nil, Principal{}, false
	}
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
			if p, name, ok = u.revalidateSSO(r, handle, ss.subject); !ok {
				return kill()
			}
		} else {
			var err error
			if p, err = u.authenticateToken(r, token); err != nil || p.Subject != ss.subject {
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

// recheckFor is how often a session re-authenticates: every minute for a session that can approve
// or administer, RecheckEvery for everyone else. This only picks an interval; it grants nothing.
func (u *UI) recheckFor(p *Principal) time.Duration {
	if p.Can(PermApprove) || p.Can(PermAdmin) {
		return min(u.RecheckEvery, uiRecheckPrivil)
	}
	return u.RecheckEvery
}
