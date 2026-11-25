package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Errors returned by the service.
var (
	// ErrInvalidCredentials is the single answer for every failed login: unknown user, wrong
	// password, wrong or reused TOTP code, oversized input. Callers must not distinguish them.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrUnauthenticated means no valid session.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrBusy means the password-hashing slot could not be obtained in time (overload protection).
	ErrBusy = errors.New("login temporarily unavailable")
)

// Input limits applied before any hashing.
const (
	MaxUsernameLen = 64
	MaxPasswordLen = 1024
)

// LockedError is returned while a user/address pair or a client address is locked out or rate limited.
type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string {
	return fmt.Sprintf("too many failed attempts; retry in %s", e.RetryAfter.Round(time.Second))
}

// Policy is the login throttling policy.
type Policy struct {
	MaxUserFailures int           // failures per (username, client address) before lockout
	MaxIPFailures   int           // failures per client address before lockout
	AttemptsPerMin  int           // all login attempts per client address per minute, checked before hashing
	Window          time.Duration // failures older than this are forgotten
	LockFor         time.Duration
}

// DefaultPolicy: a (username, address) pair is locked for 15 minutes after 5 failures, an address
// after 20, and an address may not make more than 10 attempts a minute. The username lock is keyed
// on the address as well so an anonymous attacker cannot lock the real admin out from elsewhere;
// recovery for the remaining cases is `can-core admin unlock`.
var DefaultPolicy = Policy{MaxUserFailures: 5, MaxIPFailures: 20, AttemptsPerMin: 10, Window: 15 * time.Minute, LockFor: 15 * time.Minute}

// DefaultSessionTTL is the absolute lifetime of an admin session.
const DefaultSessionTTL = 12 * time.Hour

// CookieName is the session cookie.
const CookieName = "can_session"

// Options configures a Service; zero values take defaults.
type Options struct {
	Policy     Policy
	SessionTTL time.Duration
	Now        func() time.Time
	Log        *slog.Logger
	// HashSlots bounds concurrent argon2 evaluations (each holds ~19 MiB). Default 1.
	HashSlots int
	// HashWait is how long a login waits for a slot before giving up with ErrBusy. Default 3 s.
	HashWait time.Duration
}

// Appender is the audit log write interface.
type Appender interface {
	Append(ctx context.Context, e audit.Entry) (audit.Record, error)
}

// Service authenticates the admin and manages sessions.
type Service struct {
	db    *store.DB
	audit Appender
	opt   Options
	dummy string // hash verified for unknown users so timing does not reveal them
	slots chan struct{}

	mu      sync.Mutex
	rate    map[string]*window // per-address attempt counter
	rateGen time.Time
}

type window struct {
	start time.Time
	n     int
}

// NewService returns an auth service.
func NewService(db *store.DB, a Appender, opt Options) *Service {
	if opt.Policy == (Policy{}) {
		opt.Policy = DefaultPolicy
	}
	if opt.SessionTTL == 0 {
		opt.SessionTTL = DefaultSessionTTL
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.HashSlots < 1 {
		opt.HashSlots = 1
	}
	if opt.HashWait == 0 {
		opt.HashWait = 3 * time.Second
	}
	d, _ := HashPassword("dummy password for timing equalisation")
	return &Service{db: db, audit: a, opt: opt, dummy: d, slots: make(chan struct{}, opt.HashSlots), rate: map[string]*window{}}
}

// LoginRequest is one login attempt.
type LoginRequest struct {
	Username, Password, Code, IP string
}

// LoginResult is a successful login.
type LoginResult struct {
	Token   string // cookie value; only the hash is stored
	Session store.Session
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Service) record(ctx context.Context, actor, action, ip string) error {
	_, err := s.audit.Append(ctx, audit.Entry{Actor: actor, Action: action, Target: "admin", Detail: map[string]any{"ip": ip}})
	return err
}

// recordBestEffort logs (never swallows silently) an audit failure on a path that must not block.
func (s *Service) recordBestEffort(ctx context.Context, actor, action, ip string) {
	if err := s.record(ctx, actor, action, ip); err != nil {
		s.opt.Log.Error("audit append failed", "action", action, "error", err)
	}
}

// admit counts every login attempt per client address, before any hashing happens.
func (s *Service) admit(ip string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rate) > 4096 || now.Sub(s.rateGen) > time.Minute { // bound memory: drop expired windows
		for k, w := range s.rate {
			if now.Sub(w.start) >= time.Minute {
				delete(s.rate, k)
			}
		}
		s.rateGen = now
	}
	w := s.rate[ip]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &window{start: now}
		s.rate[ip] = w
	}
	w.n++
	if w.n > s.opt.Policy.AttemptsPerMin {
		return &LockedError{RetryAfter: time.Minute - now.Sub(w.start)}
	}
	return nil
}

// acquire takes a hashing slot, bounding concurrent 19 MiB argon2 evaluations.
func (s *Service) acquire(ctx context.Context) (func(), error) {
	t := time.NewTimer(s.opt.HashWait)
	defer t.Stop()
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, nil
	case <-t.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Login verifies username, password and TOTP code. Rate limits and locks are checked before any
// credential is evaluated, and argon2 runs under a concurrency cap.
func (s *Service) Login(ctx context.Context, r LoginRequest) (LoginResult, error) {
	now := s.opt.Now()
	if err := s.admit(r.IP, now); err != nil {
		return LoginResult{}, err
	}
	userKey, ipKey := "user:"+strings.ToLower(r.Username)+"|"+r.IP, "ip:"+r.IP
	if len(r.Username) > MaxUsernameLen || len(r.Password) > MaxPasswordLen || len(r.Username) == 0 {
		// Never reaches argon2; counts against the address only (no per-name state for junk names).
		if _, _, err := s.db.RegisterFailure(ctx, ipKey, s.opt.Policy.MaxIPFailures, s.opt.Policy.Window, s.opt.Policy.LockFor, now); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	for _, k := range []string{userKey, ipKey} {
		a, err := s.db.GetLoginAttempt(ctx, k)
		if err != nil {
			return LoginResult{}, err
		}
		if now.Before(a.LockedUntil) {
			return LoginResult{}, &LockedError{RetryAfter: a.LockedUntil.Sub(now)}
		}
	}

	u, uerr := s.db.AdminByUsername(ctx, r.Username)
	hash := s.dummy
	if uerr == nil {
		hash = u.PasswordHash
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	pwOK, verr := VerifyPassword(r.Password, hash)
	release()
	if verr != nil {
		return LoginResult{}, verr
	}
	ok := uerr == nil && pwOK
	if ok {
		step, valid := ValidateTOTP(u.TOTPSecret, r.Code, now)
		if valid {
			valid, _ = s.db.AdvanceTOTPStep(ctx, u.ID, step) // single use
		}
		ok = valid
	}
	if !ok {
		if err := s.fail(ctx, userKey, s.opt.Policy.MaxUserFailures, r.Username, r.IP, now); err != nil {
			return LoginResult{}, err
		}
		if err := s.fail(ctx, ipKey, s.opt.Policy.MaxIPFailures, r.Username, r.IP, now); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	_ = s.db.ClearLoginAttempt(ctx, userKey)
	if NeedsRehash(u.PasswordHash) {
		if release, err := s.acquire(ctx); err == nil {
			if nh, err := HashPassword(r.Password); err == nil {
				_ = s.db.SetPasswordHash(ctx, u.ID, nh)
			}
			release()
		}
	}
	tok := randToken()
	sess := store.Session{IDHash: hashToken(tok), UserID: u.ID, CSRF: randToken(), IP: r.IP, CreatedAt: now, ExpiresAt: now.Add(s.opt.SessionTTL)}
	if err := s.db.CreateSession(ctx, sess); err != nil {
		return LoginResult{}, err
	}
	// Fail closed: a login that cannot be audited does not get a session.
	if err := s.record(ctx, u.Username, "admin.login", r.IP); err != nil {
		_ = s.db.DeleteSession(ctx, sess.IDHash)
		return LoginResult{}, fmt.Errorf("audit log unavailable, refusing login: %w", err)
	}
	_, _ = s.db.DeleteExpiredSessions(ctx, now)
	return LoginResult{Token: tok, Session: sess}, nil
}

func (s *Service) fail(ctx context.Context, key string, max int, username, ip string, now time.Time) error {
	_, locked, err := s.db.RegisterFailure(ctx, key, max, s.opt.Policy.Window, s.opt.Policy.LockFor, now)
	if err != nil {
		return err
	}
	if locked { // one audit entry per lock, not one per refused attempt
		s.recordBestEffort(ctx, username, "admin.login.locked", ip)
	} else if strings.HasPrefix(key, "user:") {
		s.recordBestEffort(ctx, username, "admin.login.failed", ip)
	}
	return nil
}

// Authenticate resolves a cookie token to a live session and its user.
func (s *Service) Authenticate(ctx context.Context, token string) (store.Session, store.AdminUser, error) {
	if token == "" {
		return store.Session{}, store.AdminUser{}, ErrUnauthenticated
	}
	sess, err := s.db.SessionByHash(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Session{}, store.AdminUser{}, ErrUnauthenticated
		}
		return store.Session{}, store.AdminUser{}, err
	}
	if !s.opt.Now().Before(sess.ExpiresAt) {
		_ = s.db.DeleteSession(ctx, sess.IDHash)
		return store.Session{}, store.AdminUser{}, ErrUnauthenticated
	}
	u, err := s.db.AdminByID(ctx, sess.UserID)
	if err != nil {
		return store.Session{}, store.AdminUser{}, ErrUnauthenticated
	}
	return sess, u, nil
}

// Logout destroys the session.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.db.DeleteSession(ctx, hashToken(token))
}

// CSRFValid compares a presented CSRF token with the session's, in constant time.
func CSRFValid(sess store.Session, presented string) bool {
	return presented != "" && subtle.ConstantTimeCompare([]byte(sess.CSRF), []byte(presented)) == 1
}

// SessionCookie builds the session cookie: HttpOnly, SameSite=Strict, Secure unless secure is false
// (plain-HTTP local development only).
func SessionCookie(token string, expires time.Time, secure bool) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: token, Path: "/", Expires: expires, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode}
}

// ClearCookie expires the session cookie.
func ClearCookie(secure bool) *http.Cookie {
	return &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode}
}
