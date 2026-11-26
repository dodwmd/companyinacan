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
	"sync/atomic"
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
	AttemptsPerMin  int           // HardLock only: all login attempts per client address per minute
	Window          time.Duration // failures older than this are forgotten
	LockFor         time.Duration
	// HardLock refuses attempts after repeated failures (use only when client addresses are real,
	// i.e. CAN_TRUST_PROXY behind Traefik). Without it failures only add a delay of BackoffBase
	// doubling up to BackoffMax per failure beyond the threshold.
	HardLock    bool
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

// DefaultPolicy: a (username, address) pair is locked for 15 minutes after 5 failures, an address
// after 20, and an address may not make more than 10 attempts a minute. The username lock is keyed
// on the address as well so an anonymous attacker cannot lock the real admin out from elsewhere;
// recovery for the remaining cases is `can-core admin unlock`.
var DefaultPolicy = Policy{MaxUserFailures: 5, MaxIPFailures: 20, AttemptsPerMin: 10, Window: 15 * time.Minute, LockFor: 15 * time.Minute, BackoffBase: 250 * time.Millisecond, BackoffMax: 2 * time.Second}

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
	// MinLoginTime floors login duration (default MinLoginTime); Sleep is injectable for tests.
	MinLoginTime time.Duration
	Sleep        func(context.Context, time.Duration) error
	// LocalPolicy, when set, is asked after a local login's credentials were verified whether this
	// user may sign in with a password (sso.local_login, epic E11b). A refusal looks like any other
	// failed login but does not count as a failure: valid credentials are never throttled.
	LocalPolicy func(ctx context.Context, u store.AdminUser) bool
}

// Appender is the audit log write interface.
type Appender interface {
	Append(ctx context.Context, e audit.Entry) (audit.Record, error)
}

// Service authenticates the admin and manages sessions.
type Service struct {
	db        *store.DB
	audit     Appender
	opt       Options
	dummy     string // hash verified for unknown users so timing does not reveal them
	slots     chan struct{}
	dummyTOTP string
	sleepers  atomic.Int64

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
	if opt.MinLoginTime == 0 {
		opt.MinLoginTime = MinLoginTime
	}
	if opt.Sleep == nil {
		opt.Sleep = sleepCtx
	}
	if opt.HashWait == 0 {
		opt.HashWait = 3 * time.Second
	}
	return &Service{db: db, audit: a, opt: opt, dummy: dummyHash(), dummyTOTP: NewTOTPSecret(), slots: make(chan struct{}, opt.HashSlots), rate: map[string]*window{}}
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

// verify runs argon2 verification under the concurrency cap; the slot is always released.
func (s *Service) verify(ctx context.Context, password, hash string) (bool, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	defer releaseHashMemory() // runs before release: the next hash starts with the memory given back
	return VerifyPassword(password, hash)
}

func (s *Service) hash(ctx context.Context, password string) (string, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	defer releaseHashMemory()
	return HashPassword(password)
}

// MinLoginTime is the floor on how long a login takes, so that a login whose TOTP code is wrong
// (which never reaches argon2) cannot be told apart by timing from one that is verified.
const MinLoginTime = 150 * time.Millisecond

// maxSleepers bounds logins waiting out a failure back-off.
const maxSleepers = 256

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// backoff is the delay owed for a failure count: none below the threshold, then doubling.
func (s *Service) backoff(failures, threshold int) time.Duration {
	if failures < threshold {
		return 0
	}
	d := s.opt.Policy.BackoffBase
	for i := threshold; i < failures && d < s.opt.Policy.BackoffMax; i++ {
		d *= 2
	}
	if d > s.opt.Policy.BackoffMax {
		d = s.opt.Policy.BackoffMax
	}
	return d
}

// Login verifies username, password and TOTP code.
//
// Order of work, cheapest first, so unauthenticated traffic cannot consume the 19 MiB argon2
// working set: the TOTP code is checked first (a guess hits with probability about 3e-6), and
// argon2 only runs for requests whose code is valid, under a concurrency cap. Every login takes at
// least MinLoginTime, hiding which step failed.
//
// Throttling has two modes (Policy.HardLock). Behind a trusted proxy client addresses are real, so
// repeated failures refuse further attempts from that (username, address) pair or address. When
// addresses are not trustworthy (every client looks like the ingress), refusing would let an
// anonymous attacker lock the admin out, so failures only add an increasing delay: the admin with
// valid credentials and TOTP code always gets in, just slower.
func (s *Service) Login(ctx context.Context, r LoginRequest) (LoginResult, error) {
	start := s.opt.Now()
	now := start
	pol := s.opt.Policy
	if pol.HardLock {
		if err := s.admit(r.IP, now); err != nil {
			return LoginResult{}, err
		}
	}
	userKey, ipKey := "user:"+strings.ToLower(r.Username)+"|"+r.IP, "ip:"+r.IP
	finish := func(res LoginResult, err error) (LoginResult, error) {
		if err != nil && !errors.Is(err, ErrInvalidCredentials) {
			return res, err
		}
		if rest := s.opt.MinLoginTime - s.opt.Now().Sub(start); rest > 0 {
			if serr := s.opt.Sleep(ctx, rest); serr != nil {
				return LoginResult{}, serr
			}
		}
		return res, err
	}
	if len(r.Username) > MaxUsernameLen || len(r.Password) > MaxPasswordLen || len(r.Username) == 0 {
		// Never reaches argon2; counts against the address only (no per-name state for junk names).
		if _, _, err := s.db.RegisterFailure(ctx, ipKey, pol.MaxIPFailures, pol.Window, pol.LockFor, now); err != nil {
			return LoginResult{}, err
		}
		return finish(LoginResult{}, ErrInvalidCredentials)
	}
	var delay time.Duration
	var held []reservation // HardLock only: counted before checking
	keys := []reservation{{key: userKey, max: pol.MaxUserFailures}, {key: ipKey, max: pol.MaxIPFailures}}
	if pol.HardLock {
		var err error
		if held, err = s.reserve(ctx, keys, now); err != nil {
			return LoginResult{}, err
		}
	} else {
		for _, k := range keys {
			a, err := s.db.GetLoginAttempt(ctx, k.key)
			if err != nil {
				return LoginResult{}, err
			}
			if now.Sub(a.FirstFailed) <= pol.Window { // counters decay with the window
				if d := s.backoff(a.Failures, k.max); d > delay {
					delay = d
				}
			}
		}
	}
	if delay > 0 {
		if s.sleepers.Add(1) > maxSleepers {
			s.sleepers.Add(-1)
			return LoginResult{}, ErrBusy
		}
		err := s.opt.Sleep(ctx, delay)
		s.sleepers.Add(-1)
		if err != nil {
			return LoginResult{}, err
		}
	}

	u, uerr := s.db.AdminByUsername(ctx, r.Username)
	secret, hash := s.dummyTOTP, s.dummy
	if uerr == nil && u.AuthSource != store.SourceLocal {
		uerr = store.ErrNotFound // SSO users have no password: indistinguishable from an unknown user
	}
	if uerr == nil && (u.TOTPSecret == "" || u.PasswordHash == store.UnusedPassword) {
		uerr = store.ErrNotFound // not set up yet: indistinguishable from an unknown user
	}
	if uerr == nil {
		secret, hash = u.TOTPSecret, u.PasswordHash
	}
	step, totpOK := ValidateTOTP(secret, r.Code, now)
	ok := false
	if totpOK {
		pwOK, verr := s.verify(ctx, r.Password, hash)
		if verr != nil {
			s.release(ctx, held) // nothing was compared
			return LoginResult{}, verr
		}
		// A disabled account is refused before anything is consumed or cleared: the TOTP step
		// stays unused and the failure counts like any other bad login.
		if uerr == nil && pwOK && !s.db.AdminDisabled(ctx, u.ID) {
			ok, _ = s.db.AdvanceTOTPStep(ctx, u.ID, step) // single use; consumed only once the password is right
		}
	}
	if !ok {
		if pol.HardLock { // already counted: only the audit entries are left
			for _, h := range held {
				s.auditFailure(ctx, h.key, h.locked, r.Username, r.IP)
			}
		} else {
			for _, k := range keys {
				if err := s.fail(ctx, k.key, k.max, r.Username, r.IP, now); err != nil {
					return LoginResult{}, err
				}
			}
		}
		account := ""
		if uerr == nil {
			account = u.Username
		}
		s.noteFailure(ctx, account, r.IP, now)
		return finish(LoginResult{}, ErrInvalidCredentials)
	}
	if len(held) > 0 {
		s.release(ctx, held[1:]) // the address keeps its earlier failures; the pair is cleared below
	}
	_ = s.db.ClearLoginAttempt(ctx, userKey)
	if s.opt.LocalPolicy != nil && !s.opt.LocalPolicy(ctx, u) {
		s.recordBestEffort(ctx, u.Username, "admin.login.refused_local", r.IP)
		return finish(LoginResult{}, ErrInvalidCredentials)
	}
	if NeedsRehash(u.PasswordHash) {
		if nh, err := s.hash(ctx, r.Password); err == nil {
			_ = s.db.SetPasswordHash(ctx, u.ID, nh)
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
	return finish(LoginResult{Token: tok, Session: sess}, nil)
}

func (s *Service) fail(ctx context.Context, key string, max int, username, ip string, now time.Time) error {
	_, locked, err := s.db.RegisterFailure(ctx, key, max, s.opt.Policy.Window, s.opt.Policy.LockFor, now)
	if err != nil {
		return err
	}
	s.auditFailure(ctx, key, locked, username, ip)
	return nil
}

func (s *Service) auditFailure(ctx context.Context, key string, locked bool, username, ip string) {
	if locked { // one audit entry per lock, not one per refused attempt
		s.recordBestEffort(ctx, username, "admin.login.locked", ip)
	} else if strings.HasPrefix(key, "user:") {
		s.recordBestEffort(ctx, username, "admin.login.failed", ip)
	}
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
	if !s.opt.Now().Before(sess.ExpiresAt) || (!sess.RevalidateAt.IsZero() && !s.opt.Now().Before(sess.RevalidateAt)) {
		_ = s.db.DeleteSession(ctx, sess.IDHash)
		return store.Session{}, store.AdminUser{}, ErrUnauthenticated
	}
	u, err := s.db.AdminByID(ctx, sess.UserID)
	if err != nil || s.db.AdminDisabled(ctx, u.ID) {
		return store.Session{}, store.AdminUser{}, ErrUnauthenticated
	}
	return sess, u, nil
}

// CreateSSOSession starts a session for a user who signed in through the identity provider. The
// token is fresh (never one the browser presented before: no session fixation) and the session must
// be re-established at the provider after revalidate. The caller audits and, if that fails, deletes
// the session with Logout. authTime is the provider's auth_time (zero = it did not say).
func (s *Service) CreateSSOSession(ctx context.Context, userID int64, ip string, revalidate time.Duration, authTime time.Time) (LoginResult, error) {
	now := s.opt.Now()
	tok := randToken()
	sess := store.Session{IDHash: hashToken(tok), UserID: userID, CSRF: randToken(), IP: ip, CreatedAt: now, ExpiresAt: now.Add(s.opt.SessionTTL), Source: store.SourceSSO, AuthAt: authTime}
	if revalidate > 0 {
		sess.RevalidateAt = now.Add(revalidate)
		if sess.RevalidateAt.After(sess.ExpiresAt) {
			sess.RevalidateAt = sess.ExpiresAt
		}
	}
	if err := s.db.CreateSession(ctx, sess); err != nil {
		return LoginResult{}, err
	}
	_, _ = s.db.DeleteExpiredSessions(ctx, now)
	return LoginResult{Token: tok, Session: sess}, nil
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
