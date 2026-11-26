// Package session holds the browser sessions of the module web UIs: the server-side session table,
// the cookies, the sign-in CSRF cookie, idle and absolute timeouts, periodic re-checking of the
// credential, single sign-on by launch ticket (specs/036-module-ui-sso) and the step-up before an
// irreversible action. Every module UI uses it, so a fix here reaches all of them; the conformance
// subpackage is the suite each of them runs.
//
// A person presents a personal access token (or a launch ticket) once. The server keeps a session
// keyed by the SHA-256 of a random identifier; the browser holds only that identifier, in an
// HttpOnly, SameSite=Strict cookie. A pasted token is kept in memory (never written to disk, never
// sent to the browser again) so that revocation is noticed. A restart ends every session.
//
// What stays with a module: who may sign in and with which permissions (Config.Reauth and
// Config.Grant), how often that is asked again (Config.Recheck), its pages and its rate limiters.
package session

import (
	"crypto/sha256"
	"net/http"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// Defaults of a session.
const (
	DefaultIdle     = 30 * time.Minute
	DefaultAbsolute = 8 * time.Hour
	DefaultRecheck  = 5 * time.Minute
	// PrivilegedRecheck bounds the interval of a session that can approve, administer or read other
	// people's sensitive data: a revoked or demoted one must not keep doing that for long.
	PrivilegedRecheck = time.Minute
	// MaxTokenLen bounds a pasted token.
	MaxTokenLen = 4096

	hostPrefix  = "__Host-"
	maxCookieID = 128
)

// Interval is the re-check interval of a session: every, but no more than PrivilegedRecheck for a
// privileged one. It only picks an interval; it grants nothing.
func Interval(every time.Duration, privileged bool) time.Duration {
	if privileged {
		return min(every, PrivilegedRecheck)
	}
	return every
}

// Config describes the sessions of one UI. The functions are called per request.
type Config[P any] struct {
	// Module is the module id can-core names the launch nonce cookie after.
	Module string
	// SessionCookie and CSRFCookie are the cookie names; Secure adds the __Host- prefix.
	SessionCookie, CSRFCookie string
	Secure                    func(r *http.Request) bool
	Now                       func() time.Time

	// Timeouts is the idle and absolute lifetime of a session.
	Timeouts func() (idle, absolute time.Duration)
	// Max bounds the table and PerSubject one person's share of it; the oldest are dropped.
	Max, PerSubject int
	// CSRFMaxAge is the lifetime of the sign-in cookie. RotateCSRF gives every sign-in form a new
	// token; otherwise a valid one is kept, so a failed attempt (or a second tab) does not
	// invalidate the form the person is looking at.
	CSRFMaxAge int
	RotateCSRF bool

	// Recheck is how old the last check of the credential may be before it is made again; zero or
	// less means on every request. sso is a launch-ticket session.
	Recheck func(p P, sso bool) time.Duration
	// Subject is who a principal is. A re-check naming somebody else ends the session.
	Subject func(p P) string
	// Reauth authenticates a pasted token again. Any error ends the session.
	Reauth func(r *http.Request, token string) (P, error)
	// Fresh is Reauth without any cached answer, for a step-up. Nil: the module has none.
	Fresh func(r *http.Request, token string) (P, error)
	// SSO is can-core's launch-ticket API; nil result: no single sign-on.
	SSO func() modclient.SessionAPI
	// Grant maps what can-core says a person may do to a principal; false: nothing here.
	Grant func(subject string, permissions []string) (P, bool)
	// ValidSubject narrows the account a launch ticket may name beyond the "user:" prefix.
	ValidSubject func(subject string) bool
}

// Session is one browser's session.
type Session[P any] struct {
	// Subject and CSRF never change.
	Subject, CSRF string

	token   string // "" for a launch-ticket session
	handle  string // can-core's session handle; "" for a token session
	created time.Time
	// deadline is can-core's session_expires_at of a launch-ticket session (the identity provider's
	// deadline caps it): the session ends then, whatever the timeouts say.
	deadline time.Time
	seq      uint64 // order of creation, for eviction

	// guarded by the manager's mutex
	last, checked time.Time
	p             P
	name          string
}

// SSO reports whether the session began with a launch ticket.
func (s *Session[P]) SSO() bool { return s.handle != "" }

// Manager is the sessions of one UI.
type Manager[P any] struct {
	cfg Config[P]

	mu  sync.Mutex
	m   map[[sha256.Size]byte]*Session[P]
	seq uint64
}

// New returns a manager.
func New[P any](cfg Config[P]) *Manager[P] {
	return &Manager[P]{cfg: cfg, m: map[[sha256.Size]byte]*Session[P]{}}
}

// ---- cookies ---------------------------------------------------------------------------------

func (m *Manager[P]) cookieName(r *http.Request, base string) string {
	if m.cfg.Secure(r) {
		return hostPrefix + base
	}
	return base
}

// SessionCookieName is the name of the session cookie as set for r.
func (m *Manager[P]) SessionCookieName(r *http.Request) string {
	return m.cookieName(r, m.cfg.SessionCookie)
}

// CSRFCookieName is the name of the sign-in cookie as set for r.
func (m *Manager[P]) CSRFCookieName(r *http.Request) string {
	return m.cookieName(r, m.cfg.CSRFCookie)
}

func (m *Manager[P]) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: m.cfg.Secure(r), SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func (m *Manager[P]) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	m.setCookie(w, r, m.SessionCookieName(r), "", -1)
}

// LoginCSRF returns the double-submit token of the sign-in forms (there is no session yet to bind a
// token to) and sets its cookie.
func (m *Manager[P]) LoginCSRF(w http.ResponseWriter, r *http.Request) string {
	tok := ""
	if !m.cfg.RotateCSRF {
		if ck, err := r.Cookie(m.CSRFCookieName(r)); err == nil && len(ck.Value) == 43 {
			tok = ck.Value
		}
	}
	if tok == "" {
		tok = NewToken()
	}
	m.setCookie(w, r, m.CSRFCookieName(r), tok, m.cfg.CSRFMaxAge)
	return tok
}

// CheckLoginCSRF reports whether the posted "csrf" field matches the sign-in cookie.
func (m *Manager[P]) CheckLoginCSRF(r *http.Request) bool {
	ck, err := r.Cookie(m.CSRFCookieName(r))
	return err == nil && Equal(r.PostFormValue("csrf"), ck.Value)
}

// ---- the table -------------------------------------------------------------------------------

func key(id string) [sha256.Size]byte { return sha256.Sum256([]byte(id)) }

func (m *Manager[P]) expired(s *Session[P], now time.Time) bool {
	idle, abs := m.cfg.Timeouts()
	return now.Sub(s.last) > idle || now.Sub(s.created) > abs || (s.handle != "" && !now.Before(s.deadline))
}

// add stores a new session under a new identifier. Expired sessions are swept, and the oldest of
// the subject and the oldest overall are dropped when the bounds are exceeded.
func (m *Manager[P]) add(s *Session[P]) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.cfg.Now()
	for k, v := range m.m {
		if m.expired(v, now) {
			delete(m.m, k)
		}
	}
	id := NewToken()
	m.seq++
	s.CSRF, s.created, s.last, s.checked, s.seq = NewToken(), now, now, now, m.seq
	m.m[key(id)] = s
	m.trim(m.cfg.PerSubject, func(v *Session[P]) bool { return v.Subject == s.Subject })
	m.trim(m.cfg.Max, func(*Session[P]) bool { return true })
	return id
}

// trim drops the oldest matching sessions until at most limit are left. The mutex is held.
func (m *Manager[P]) trim(limit int, match func(*Session[P]) bool) {
	for {
		var oldest [sha256.Size]byte
		var seq uint64
		n := 0
		for k, v := range m.m {
			if !match(v) {
				continue
			}
			if n == 0 || v.seq < seq {
				oldest, seq = k, v.seq
			}
			n++
		}
		if n <= limit {
			return
		}
		delete(m.m, oldest)
	}
}

func (m *Manager[P]) get(id string) *Session[P] {
	if id == "" || len(id) > maxCookieID {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.m[key(id)]
}

func (m *Manager[P]) drop(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, key(id))
}

// Len is the number of sessions held.
func (m *Manager[P]) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}

// Name is the display name can-core gave for a launch-ticket session, "" otherwise.
func (m *Manager[P]) Name(s *Session[P]) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return s.name
}

// ---- the life of a session -------------------------------------------------------------------

func (m *Manager[P]) replace(w http.ResponseWriter, r *http.Request, s *Session[P]) {
	// A presented session id is never adopted (session fixation); the one it names is ended.
	if old, err := r.Cookie(m.SessionCookieName(r)); err == nil {
		m.drop(old.Value)
	}
	m.setCookie(w, r, m.SessionCookieName(r), m.add(s), 0)
	m.setCookie(w, r, m.CSRFCookieName(r), "", -1)
}

// Start signs the browser in with a token the module has just authenticated. The cookie lasts as
// long as the browser session; the server enforces the timeouts.
func (m *Manager[P]) Start(w http.ResponseWriter, r *http.Request, p P, token string) {
	m.replace(w, r, &Session[P]{Subject: m.cfg.Subject(p), p: p, token: token})
}

// End signs the browser out.
func (m *Manager[P]) End(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(m.SessionCookieName(r)); err == nil {
		m.drop(ck.Value)
	}
	m.clearSessionCookie(w, r)
}

// Current returns the live session of the request and the principal it holds now, enforcing the
// timeouts and the periodic re-check. false means "sign in"; a cookie that no longer opens a
// session is cleared. Any doubt at a re-check ends the session.
func (m *Manager[P]) Current(w http.ResponseWriter, r *http.Request) (*Session[P], P, bool) {
	var none P
	ck, err := r.Cookie(m.SessionCookieName(r))
	if err != nil {
		return nil, none, false
	}
	kill := func() (*Session[P], P, bool) {
		m.drop(ck.Value)
		m.clearSessionCookie(w, r)
		return nil, none, false
	}
	s := m.get(ck.Value)
	if s == nil {
		return kill()
	}
	now := m.cfg.Now()
	// Snapshot the mutable fields under the lock: concurrent requests of one session write them.
	m.mu.Lock()
	dead, checked, p, name := m.expired(s, now), s.checked, s.p, s.name
	m.mu.Unlock()
	if dead {
		return kill()
	}
	every := m.cfg.Recheck(p, s.SSO())
	recheck := every <= 0 || now.Sub(checked) >= every
	if recheck {
		var ok bool
		if s.SSO() {
			var current string
			if p, current, ok = m.revalidate(r, s); ok && current != "" {
				name = current
			}
		} else {
			p, err = m.cfg.Reauth(r, s.token)
			ok = err == nil && m.cfg.Subject(p) == s.Subject
		}
		if !ok {
			return kill()
		}
	}
	m.mu.Lock()
	s.last = now
	if recheck {
		s.checked, s.p, s.name = now, p, name
	}
	m.mu.Unlock()
	return s, p, true
}

// Live is the subject of the unexpired session this browser holds and the name to show for it,
// without re-checking or extending it.
func (m *Manager[P]) Live(r *http.Request) (subject, shown string) {
	ck, err := r.Cookie(m.SessionCookieName(r))
	if err != nil {
		return "", ""
	}
	s := m.get(ck.Value)
	if s == nil {
		return "", ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.expired(s, m.cfg.Now()) {
		return "", ""
	}
	if s.name != "" {
		return s.Subject, s.name
	}
	return s.Subject, s.Subject
}
