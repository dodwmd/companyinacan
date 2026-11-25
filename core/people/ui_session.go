package people

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

// Sessions of the web UI. The user presents a personal access token once at /login; the server
// authenticates it with the same Authenticator chain the JSON API uses and keeps a server-side
// session keyed by the SHA-256 of a random identifier. The browser only holds that identifier in an
// HttpOnly, SameSite=Strict cookie scoped to this origin. The token is held in memory (never
// written to disk, never sent to the browser again) so that revocation is noticed: every
// RecheckEvery the session asks the Authenticator again, and a token that no longer authenticates
// ends the session. The admin UI's session cookie is not a credential here and is never read.

const (
	defaultIdle      = 30 * time.Minute
	defaultAbsolute  = 8 * time.Hour
	defaultRecheck   = 5 * time.Minute
	maxSessions      = 5000
	maxPerSubject    = 5
	maxTokenLen      = 4096
	sessionCookie    = "can_people_session"
	loginCSRFCookie  = "can_people_login_csrf"
	hostPrefix       = "__Host-"
	loginCSRFMaxAge  = 15 * 60
	failedLoginBurst = 10
)

type uiSession struct {
	subject string
	princ   Principal
	token   string
	csrf    string
	created time.Time
	last    time.Time
	checked time.Time
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[[sha256.Size]byte]*uiSession{}
	}
	id := newToken()
	subject := p.Subject
	ss := &uiSession{subject: subject, princ: p, token: token, csrf: newToken(), created: now, last: now, checked: now}
	s.m[sidKey(id)] = ss
	var own []([sha256.Size]byte)
	for k, v := range s.m {
		if v.subject == subject {
			own = append(own, k)
		}
	}
	for len(own) > maxPerSubject {
		oldest := 0
		for i, k := range own {
			if s.m[k].created.Before(s.m[own[oldest]].created) {
				oldest = i
			}
		}
		delete(s.m, own[oldest])
		own = append(own[:oldest], own[oldest+1:]...)
	}
	for len(s.m) > maxSessions {
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
		return hostPrefix + sessionCookie
	}
	return sessionCookie
}

func (u *UI) loginCSRFName() string {
	if u.Secure {
		return hostPrefix + loginCSRFCookie
	}
	return loginCSRFCookie
}

func (u *UI) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: u.Secure, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
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

// current returns the live session of the request, enforcing idle and absolute timeouts and the
// periodic re-authentication, plus the caller resolved through the Service. nil means "log in".
func (u *UI) current(w http.ResponseWriter, r *http.Request) (*uiSession, Actor, bool) {
	ck, err := r.Cookie(u.sessionCookieName())
	if err != nil {
		return nil, Actor{}, false
	}
	ss := u.sessions.get(ck.Value)
	if ss == nil {
		u.setCookie(w, u.sessionCookieName(), "", -1)
		return nil, Actor{}, false
	}
	now := u.Now()
	kill := func() (*uiSession, Actor, bool) {
		u.sessions.drop(ck.Value)
		u.setCookie(w, u.sessionCookieName(), "", -1)
		return nil, Actor{}, false
	}
	if now.Sub(ss.last) > u.IdleTimeout || now.Sub(ss.created) > u.AbsoluteTimeout {
		return kill()
	}
	princ := ss.princ
	if now.Sub(ss.checked) > u.RecheckEvery {
		// Periodic re-authentication: a revoked token ends the session; roles are refreshed.
		p, err := u.authenticateToken(r, ss.token)
		if err != nil || p.Subject != ss.subject {
			return kill()
		}
		princ = *p
	}
	p := &princ
	a, err := u.Svc.ResolveActor(r.Context(), p)
	if err != nil || !a.HasPerson || !a.Active() {
		return kill()
	}
	u.sessions.mu.Lock()
	ss.last = now
	if now.Sub(ss.checked) > u.RecheckEvery {
		ss.checked, ss.princ = now, princ
	}
	u.sessions.mu.Unlock()
	return ss, a, true
}

func constEq(a, b string) bool {
	return a != "" && len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
