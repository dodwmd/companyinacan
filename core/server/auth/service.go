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
	"net/http"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Errors returned by the service.
var (
	// ErrInvalidCredentials is the single answer for every failed login: unknown user, wrong
	// password, wrong or reused TOTP code. Callers must not distinguish them.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrUnauthenticated means no valid session.
	ErrUnauthenticated = errors.New("unauthenticated")
)

// LockedError is returned while a user or client address is locked out.
type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string {
	return fmt.Sprintf("too many failed attempts; retry in %s", e.RetryAfter.Round(time.Second))
}

// Policy is the login throttling policy.
type Policy struct {
	MaxUserFailures int           // failures per username before lockout
	MaxIPFailures   int           // failures per client address before lockout
	Window          time.Duration // failures older than this are forgotten
	LockFor         time.Duration
}

// DefaultPolicy locks a username for 15 minutes after 5 failures and a client address after 20.
var DefaultPolicy = Policy{MaxUserFailures: 5, MaxIPFailures: 20, Window: 15 * time.Minute, LockFor: 15 * time.Minute}

// DefaultSessionTTL is the absolute lifetime of an admin session.
const DefaultSessionTTL = 12 * time.Hour

// CookieName is the session cookie.
const CookieName = "can_session"

// Options configures a Service; zero values take defaults.
type Options struct {
	Policy     Policy
	SessionTTL time.Duration
	Now        func() time.Time
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
	d, _ := HashPassword("dummy password for timing equalisation")
	return &Service{db: db, audit: a, opt: opt, dummy: d}
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

func (s *Service) record(ctx context.Context, actor, action, ip string, extra map[string]any) {
	d := map[string]any{"ip": ip}
	for k, v := range extra {
		d[k] = v
	}
	_, _ = s.audit.Append(ctx, audit.Entry{Actor: actor, Action: action, Target: "admin", Detail: d})
}

// Login verifies username, password and TOTP code. Locked keys are refused before any credential
// is evaluated.
func (s *Service) Login(ctx context.Context, r LoginRequest) (LoginResult, error) {
	now := s.opt.Now()
	userKey, ipKey := "user:"+strings.ToLower(r.Username), "ip:"+r.IP
	for _, k := range []string{userKey, ipKey} {
		a, err := s.db.GetLoginAttempt(ctx, k)
		if err != nil {
			return LoginResult{}, err
		}
		if now.Before(a.LockedUntil) {
			s.record(ctx, r.Username, "admin.login.locked", r.IP, nil)
			return LoginResult{}, &LockedError{RetryAfter: a.LockedUntil.Sub(now)}
		}
	}

	u, uerr := s.db.AdminByUsername(ctx, r.Username)
	hash := s.dummy
	if uerr == nil {
		hash = u.PasswordHash
	}
	pwOK, verr := VerifyPassword(r.Password, hash)
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
		if err := s.fail(ctx, userKey, s.opt.Policy.MaxUserFailures, now); err != nil {
			return LoginResult{}, err
		}
		if err := s.fail(ctx, ipKey, s.opt.Policy.MaxIPFailures, now); err != nil {
			return LoginResult{}, err
		}
		s.record(ctx, r.Username, "admin.login.failed", r.IP, nil)
		return LoginResult{}, ErrInvalidCredentials
	}
	_ = s.db.ClearLoginAttempt(ctx, userKey)
	if NeedsRehash(u.PasswordHash) {
		if nh, err := HashPassword(r.Password); err == nil {
			_ = s.db.SetPasswordHash(ctx, u.ID, nh)
		}
	}
	tok := randToken()
	sess := store.Session{IDHash: hashToken(tok), UserID: u.ID, CSRF: randToken(), IP: r.IP, CreatedAt: now, ExpiresAt: now.Add(s.opt.SessionTTL)}
	if err := s.db.CreateSession(ctx, sess); err != nil {
		return LoginResult{}, err
	}
	_, _ = s.db.DeleteExpiredSessions(ctx, now)
	s.record(ctx, u.Username, "admin.login", r.IP, nil)
	return LoginResult{Token: tok, Session: sess}, nil
}

func (s *Service) fail(ctx context.Context, key string, max int, now time.Time) error {
	a, err := s.db.GetLoginAttempt(ctx, key)
	if err != nil {
		return err
	}
	expiredLock := !a.LockedUntil.IsZero() && a.LockedUntil.Unix() > 0 && !now.Before(a.LockedUntil)
	if a.Failures == 0 || expiredLock || now.Sub(a.FirstFailed) > s.opt.Policy.Window {
		a = store.LoginAttempt{Key: key, FirstFailed: now}
	}
	a.Failures++
	if a.Failures >= max {
		a.LockedUntil = now.Add(s.opt.Policy.LockFor)
	}
	return s.db.PutLoginAttempt(ctx, a)
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
