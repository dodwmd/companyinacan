// Package sso is OpenID Connect single sign-on for the admin UI and API (epic E11b,
// specs/021-oidc-sso). It implements the authorization code flow with PKCE and the small subset of
// OIDC Core, discovery and JWS that this needs, with the standard library only.
//
// Design rules (the failure modes this is written against):
//
//   - A user is found by (issuer, sub), never by email. Linking an identity to an existing local
//     account is an explicit owner action. An unverified or issuer-controlled email cannot take
//     over an account.
//   - Everything unexpected fails closed: a token that does not verify, keys that cannot be
//     fetched, a discovery document that does not match, a mapping that cannot be applied.
//   - Every outbound call has a timeout, a size cap, no redirects, https and a public address.
package sso

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Errors of the sign-in. Callers show one generic message for all of them; Reason() is for the log
// and audit entry (a short code, never user data).
var (
	ErrNotConfigured = errors.New("single sign-on is not configured")
	ErrBusy          = errors.New("sign-in is busy")
)

// Denied is a sign-in that was refused.
type Denied struct{ Code string }

func (d *Denied) Error() string { return "sign-in refused: " + d.Code }

func deny(code string) error { return &Denied{Code: code} }

// ConfigSource returns the SSO block of the applied company.yaml (nil when absent) and the company domain.
type ConfigSource func(ctx context.Context) (*config.SSO, string, error)

// FileSource reads the company file. It validates only the SSO block (the full company validation
// runs when the file is applied).
func FileSource(path string) ConfigSource {
	return func(ctx context.Context) (*config.SSO, string, error) {
		fi, err := os.Stat(path)
		if err != nil {
			return nil, "", err
		}
		if fi.Size() > 1<<20 {
			return nil, "", errors.New("company file is too large")
		}
		raw, err := os.ReadFile(path) //nolint:gosec // operator-configured path
		if err != nil {
			return nil, "", err
		}
		doc, ps := config.LoadBytes(path, raw)
		if doc == nil {
			return nil, "", fmt.Errorf("company file: %s", ps[0].Message)
		}
		c, err := config.DecodeCompany(doc)
		if err != nil {
			return nil, "", errors.New("company file does not decode")
		}
		return c.SSO, c.Domain, nil
	}
}

// Service is single sign-on for can-core.
type Service struct {
	DB      *store.DB
	Auth    *auth.Service
	RBAC    *rbac.Service
	Audit   *audit.Log
	Source  ConfigSource
	Secrets SecretReader
	Log     *slog.Logger
	Now     func() time.Time
	// Client overrides the outbound HTTP client (tests); nil builds the SSRF-guarded one.
	Client *http.Client
	Secure bool
	// HardLock mirrors the login throttling mode (real client addresses behind a trusted proxy).
	HardLock bool

	once     sync.Once
	stateKey []byte
	sem      chan struct{}

	mu    sync.Mutex
	cur   *current
	curAt time.Time
	used  map[string]time.Time
}

// configTTL is how long the company file's SSO block is cached.
const configTTL = 30 * time.Second

type current struct {
	cfg  config.SSO
	hash string
	prov *Provider
	err  error // configuration invalid or secret unavailable
	off  bool  // not configured or disabled
}

func (s *Service) init() {
	s.once.Do(func() {
		s.stateKey = []byte(randString(32))
		s.sem = make(chan struct{}, 4)
		s.used = map[string]time.Time{}
		if s.Now == nil {
			s.Now = time.Now
		}
		if s.Log == nil {
			s.Log = slog.Default()
		}
	})
}

// load returns the current configuration and provider, reloading the company file at most every
// configTTL. A change of configuration (or of the client secret) builds a fresh provider, so
// cached discovery and keys never outlive the settings they were fetched for.
func (s *Service) load(ctx context.Context) *current {
	s.init()
	s.mu.Lock()
	if s.cur != nil && s.Now().Sub(s.curAt) < configTTL {
		c := s.cur
		s.mu.Unlock()
		return c
	}
	prev := s.cur
	s.mu.Unlock()

	c := s.build(ctx, prev)
	s.mu.Lock()
	s.cur, s.curAt = c, s.Now()
	s.mu.Unlock()
	return c
}

// Reload drops the cached configuration (after a company.yaml change in the same process).
func (s *Service) Reload() {
	s.init()
	s.mu.Lock()
	s.cur = nil
	s.mu.Unlock()
}

func (s *Service) build(ctx context.Context, prev *current) *current {
	if s.Source == nil {
		return &current{off: true}
	}
	raw, domain, err := s.Source(ctx)
	if err != nil {
		s.Log.Error("sso: reading the company file failed", "error", err)
		return &current{err: errors.New("the company file cannot be read")}
	}
	if raw == nil || !raw.Enabled {
		return &current{off: true}
	}
	if issues := config.ValidateSSO(raw, domain); len(issues) > 0 {
		return &current{cfg: *raw, err: fmt.Errorf("sso configuration is invalid: %s %s", issues[0].Path, issues[0].Message)}
	}
	cfg := raw.Normalised(domain)
	var secret string
	if s.Secrets != nil {
		secret, err = s.Secrets.Secret(ctx, cfg.ClientSecretRef)
	} else {
		err = errors.New("no secret reader")
	}
	if err != nil || secret == "" {
		s.Log.Error("sso: client secret unavailable", "ref", cfg.ClientSecretRef, "error", err)
		return &current{cfg: cfg, err: errors.New("the client secret cannot be read from its Secret")}
	}
	j, _ := json.Marshal(cfg)
	sum := sha256.Sum256(append(j, secret...))
	h := hex.EncodeToString(sum[:])
	if prev != nil && prev.hash == h && prev.prov != nil {
		return prev
	}
	return &current{cfg: cfg, hash: h, prov: NewProvider(cfg, secret, s.Client, s.Now)}
}

// Enabled reports whether sign-in with the identity provider is offered. A broken configuration
// is enabled-but-failing, shown as an error on sign-in rather than silently hiding the button.
func (s *Service) Enabled(ctx context.Context) bool {
	c := s.load(ctx)
	return !c.off
}

// Begin starts a login: it returns where to send the browser and the transaction cookie.
func (s *Service) Begin(ctx context.Context, next string) (string, *http.Cookie, error) {
	c := s.load(ctx)
	if c.off {
		return "", nil, ErrNotConfigured
	}
	if c.err != nil {
		return "", nil, c.err
	}
	sd := stateData{State: randString(6) + randString(32), Nonce: randString(32), Verifier: randString(48), Next: SafeNext(next), Exp: s.Now().Add(StateTTL).Unix()}
	loc, err := c.prov.AuthURL(ctx, sd.State, sd.Nonce, pkceChallenge(sd.Verifier))
	if err != nil {
		s.Log.Error("sso: starting login failed", "error", err)
		return "", nil, errors.New("the identity provider cannot be reached")
	}
	name, _ := FlowCookieName(sd.State)
	return loc, StateCookie(name, seal(s.stateKey, sd), s.Secure), nil
}

// CompleteRequest is the callback.
type CompleteRequest struct {
	Query       url.Values
	StateCookie string
	IP          string
	// OldToken is the session cookie the browser presented, if any. It is destroyed: a login never
	// continues an existing session (session fixation).
	OldToken string
}

// Result is a completed sign-in.
type Result struct {
	Login auth.LoginResult
	Next  string
	Roles []string
}

func (s *Service) failAudit(ctx context.Context, code, ip string) {
	if _, err := s.Audit.Append(ctx, audit.Entry{Actor: "sso", Action: "sso.login.failed", Target: "admin", Detail: map[string]any{"reason": code, "ip": ip}}); err != nil {
		s.Log.Error("audit append failed", "action", "sso.login.failed", "error", err)
	}
}

// Complete finishes a login. Any refusal is a *Denied (or *auth.LockedError for a flood of invalid
// callbacks); nothing about why is shown to the browser.
func (s *Service) Complete(ctx context.Context, req CompleteRequest) (Result, error) {
	c := s.load(ctx)
	if c.off {
		return Result{}, ErrNotConfigured
	}
	if c.err != nil {
		return Result{}, c.err
	}
	now := s.Now()
	// 1. The transaction cookie. Callbacks without a valid one are cheap to refuse and are the only
	//    ones that are ever throttled; a callback that holds a valid, unused, signed state is never
	//    refused for the sake of an address's earlier failures.
	sd, err := open(s.stateKey, req.StateCookie, now)
	if err == nil && !constEq(sd.State, req.Query.Get("state")) {
		err = errState
	}
	if err == nil && !s.markUsed(sd.State, now) {
		err = errState // replayed callback
	}
	if err != nil {
		pol := auth.DefaultPolicy
		_, locked, ferr := s.DB.RegisterFailure(ctx, "sso-ip:"+req.IP, pol.MaxIPFailures, pol.Window, pol.LockFor, now)
		if ferr != nil {
			return Result{}, ferr
		}
		s.failAudit(ctx, "state", req.IP)
		if locked && s.HardLock {
			return Result{}, &auth.LockedError{RetryAfter: pol.LockFor}
		}
		if s.HardLock {
			if a, gerr := s.DB.GetLoginAttempt(ctx, "sso-ip:"+req.IP); gerr == nil && now.Before(a.LockedUntil) {
				return Result{}, &auth.LockedError{RetryAfter: a.LockedUntil.Sub(now)}
			}
		}
		return Result{}, deny("state")
	}
	// A destroyed fixation candidate: whatever session the browser had ends here.
	if req.OldToken != "" {
		_ = s.Auth.Logout(ctx, req.OldToken)
	}
	if req.Query.Get("error") != "" {
		s.failAudit(ctx, "idp_error", req.IP)
		return Result{}, deny("idp_error")
	}
	code := req.Query.Get("code")
	if code == "" || len(code) > 2048 {
		s.failAudit(ctx, "no_code", req.IP)
		return Result{}, deny("no_code")
	}
	// 2. Bound concurrent exchanges (each is an outbound call).
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-time.After(5 * time.Second):
		return Result{}, ErrBusy
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	tok, err := c.prov.Exchange(ctx, code, sd.Verifier)
	if err != nil {
		s.Log.Warn("sso: code exchange failed", "error", err)
		s.failAudit(ctx, "exchange", req.IP)
		return Result{}, deny("exchange")
	}
	id, err := c.prov.VerifyIDToken(ctx, tok.IDToken, sd.Nonce, tok.AccessToken)
	if err != nil {
		s.Log.Warn("sso: ID token refused", "error", err)
		s.failAudit(ctx, "token", req.IP)
		return Result{}, deny("token")
	}
	// 3. Policy on the claims.
	if code := checkPolicy(c.cfg, id); code != "" {
		s.failAudit(ctx, code, req.IP)
		return Result{}, deny(code)
	}
	want := MapRoles(c.cfg, id.Groups)
	if c.cfg.RequireMappedRole && len(want) == 0 {
		// Still revoke what the sync granted earlier, then refuse.
		if ident, e := s.DB.SSOIdentityByExternal(ctx, id.IssuerHash, id.Subject); e == nil && s.RBAC != nil {
			_, _ = s.RBAC.SyncSSORoles(ctx, "user:"+ident.Username, nil, false)
		}
		s.failAudit(ctx, "no_role", req.IP)
		return Result{}, deny("no_role")
	}
	// 4. The user: found by (issuer, sub), created on first sight, never matched by email.
	groupsJSON, _ := json.Marshal(nonNilS(id.Groups))
	amrJSON, _ := json.Marshal(nonNilS(id.AMR))
	ident, err := s.DB.SSOIdentityByExternal(ctx, id.IssuerHash, id.Subject)
	switch {
	case err == nil:
		if err := s.DB.TouchSSOIdentity(ctx, ident.ID, id.Email, id.Name, string(groupsJSON), string(amrJSON)); err != nil {
			return Result{}, err
		}
	case errors.Is(err, store.ErrNotFound):
		ident, err = s.DB.CreateSSOUser(ctx, usernameFor(id), store.SSOIdentity{IssuerHash: id.IssuerHash, Subject: id.Subject, Email: id.Email, Name: id.Name, GroupsJSON: string(groupsJSON), AMRJSON: string(amrJSON)},
			func(tx *sql.Tx) error {
				_, aerr := s.Audit.AppendTx(ctx, tx, audit.Entry{Actor: "sso", Action: "sso.provision", Target: "user:" + usernameFor(id), Detail: map[string]any{"issuer_hash": id.IssuerHash, "external_subject": id.ExternalSubject()}})
				return aerr
			})
		if errors.Is(err, store.ErrSSOConflict) { // lost a race with a parallel first login: use the winner
			ident, err = s.DB.SSOIdentityByExternal(ctx, id.IssuerHash, id.Subject)
		}
		if err != nil {
			return Result{}, err
		}
	default:
		return Result{}, err
	}
	if s.DB.AdminDisabled(ctx, ident.UserID) {
		s.failAudit(ctx, "disabled", req.IP)
		return Result{}, deny("disabled")
	}
	// 5. Roles follow the groups on every sign-in; revoked mappings are removed (and audited by the sync).
	var roles []string
	if s.RBAC != nil {
		res, err := s.RBAC.SyncSSORoles(ctx, "user:"+ident.Username, want, c.cfg.ConfirmOwnerMapping)
		if err != nil {
			return Result{}, fmt.Errorf("role sync failed, refusing sign-in: %w", err)
		}
		roles = res.After
	}
	// 6. The session: fresh token, revalidated at the provider after the configured interval.
	lr, err := s.Auth.CreateSSOSession(ctx, ident.UserID, req.IP, c.cfg.RevalidateEvery())
	if err != nil {
		return Result{}, err
	}
	if _, err := s.Audit.Append(ctx, audit.Entry{Actor: ident.Username, Action: "sso.login", Target: "admin", Detail: map[string]any{
		"ip": req.IP, "issuer_hash": id.IssuerHash, "roles": nonNilS(roles), "groups": len(id.Groups), "linked": ident.UserSource == store.SourceLocal,
	}}); err != nil { // fail closed: a sign-in that cannot be audited does not get a session
		_ = s.Auth.Logout(ctx, lr.Token)
		return Result{}, fmt.Errorf("audit log unavailable, refusing sign-in: %w", err)
	}
	_ = s.DB.ClearLoginAttempt(ctx, "sso-ip:"+req.IP)
	return Result{Login: lr, Next: sd.Next, Roles: roles}, nil
}

func (s *Service) markUsed(state string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.used) > 4096 {
		for k, t := range s.used {
			if now.Sub(t) > StateTTL {
				delete(s.used, k)
			}
		}
		if len(s.used) > 4096 {
			return false // under a flood, refuse rather than grow
		}
	}
	if _, dup := s.used[state]; dup {
		return false
	}
	s.used[state] = now
	return true
}

// checkPolicy applies email and authentication-method rules; it returns a refusal code or "".
func checkPolicy(cfg config.SSO, id *Identity) string {
	if cfg.EmailVerifiedRequired() && !id.EmailVerified {
		return "email_unverified"
	}
	if len(cfg.AllowedEmailDomains) > 0 {
		at := strings.LastIndexByte(id.Email, '@')
		if at < 1 || !id.EmailVerified && cfg.EmailVerifiedRequired() {
			return "email_domain"
		}
		dom := strings.ToLower(id.Email[at+1:])
		ok := false
		for _, d := range cfg.AllowedEmailDomains {
			ok = ok || dom == d
		}
		if !ok {
			return "email_domain"
		}
	}
	if len(cfg.RequireAMR) > 0 {
		ok := false
		for _, a := range id.AMR {
			ok = ok || contains(cfg.RequireAMR, a)
		}
		if !ok {
			return "amr"
		}
	}
	return ""
}

// MapRoles is the roles the group mapping gives for groups, sorted and without duplicates.
func MapRoles(cfg config.SSO, groups []string) []string {
	set := map[string]bool{}
	for _, g := range groups {
		for _, r := range cfg.GroupRoles[g] {
			set[r] = true
		}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// usernameFor is the admin username of a provisioned user: short, stable, valid as an RBAC subject
// and free of the characters that admin usernames and the audit log treat specially.
func usernameFor(id *Identity) string {
	h := sha256.Sum256([]byte(id.Subject))
	return "sso-" + id.IssuerHash[:8] + "-" + hex.EncodeToString(h[:])[:16]
}

func nonNilS(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// LogoutURL is where to send the browser after the local session is gone, or "" when the identity
// provider session is not ended (rp_logout off, no end_session_endpoint, or a local session).
func (s *Service) LogoutURL(ctx context.Context, sess store.Session) string {
	if sess.Source != store.SourceSSO {
		return ""
	}
	c := s.load(ctx)
	if c.off || c.err != nil || !c.cfg.RPLogout {
		return ""
	}
	d, err := c.prov.Discover(ctx)
	if err != nil || d.EndSessionEndpoint == "" {
		return ""
	}
	u, err := url.Parse(d.EndSessionEndpoint)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("client_id", c.cfg.ClientID)
	q.Set("post_logout_redirect_uri", c.cfg.RedirectBaseURL+"/login")
	u.RawQuery = q.Encode()
	return u.String()
}

// AllowLocalLogin implements sso.local_login for auth.Options.LocalPolicy. The break-glass path
// must survive a broken configuration, so an unreadable one behaves like owner_only.
func (s *Service) AllowLocalLogin(ctx context.Context, u store.AdminUser) bool {
	c := s.load(ctx)
	if c.off {
		return true
	}
	mode := c.cfg.LocalLogin
	if c.err != nil {
		mode = config.LocalLoginOwnerOnly // a failing provider or secret never turns the break-glass login off
	}
	switch mode {
	case "", config.LocalLoginAll:
		return true
	case config.LocalLoginDisabled:
		// Never lock everybody out: with no SSO owner, the local owner keeps the break-glass login.
		if n, err := s.DB.SSOOwnerCount(ctx); err == nil && n > 0 {
			return false
		}
	}
	if s.RBAC == nil {
		return false
	}
	ok, err := s.RBAC.HasRole(ctx, "user:"+u.Username, rbac.Owner)
	return err == nil && ok
}
