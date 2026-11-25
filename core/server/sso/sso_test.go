package sso

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/sso/ssotest"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func init() { auth.TestingParams() }

const ownerPW = "correct horse battery staple"

type env struct {
	t    *testing.T
	db   *store.DB
	idp  *ssotest.IdP
	svc  *Service
	rb   *rbac.Service
	au   *auth.Service
	al   *audit.Log
	now  time.Time
	cfg  config.SSO
	logs *bytes.Buffer
}

func newEnv(t *testing.T, mut func(*config.SSO)) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{t: t, db: db, now: time.Now().Truncate(time.Second), logs: &bytes.Buffer{}}
	db.Now = func() time.Time { return e.now }
	clock := func() time.Time { return e.now }
	e.idp = ssotest.New(t, "/realms/acme")
	e.idp.Now = clock
	hash, _ := auth.HashPassword(ownerPW)
	if _, err := db.CreateAdmin(ctx, "root", hash, auth.NewTOTPSecret()); err != nil {
		t.Fatal(err)
	}
	e.al = audit.New(db)
	e.rb = rbac.New(db, e.al, rbac.Options{Now: clock})
	e.cfg = config.SSO{
		Enabled: true, Issuer: e.idp.Issuer, ClientID: e.idp.ClientID, ClientSecretRef: "acme-sso", RedirectBaseURL: "http://admin.test", AllowInsecureDev: true,
		GroupRoles:          map[string][]string{"ops": {"operator"}, "auditors": {"auditor"}, "admins": {"owner"}, "fin": {"finance:approver"}},
		ConfirmOwnerMapping: true, LocalLogin: config.LocalLoginAll,
	}
	if mut != nil {
		mut(&e.cfg)
	}
	e.au = auth.NewService(db, e.al, auth.Options{Now: clock, MinLoginTime: time.Nanosecond, Sleep: func(context.Context, time.Duration) error { return nil },
		Policy:      auth.Policy{HardLock: true, MaxUserFailures: 5, MaxIPFailures: 20, AttemptsPerMin: 1000, Window: 15 * time.Minute, LockFor: 15 * time.Minute},
		LocalPolicy: func(c context.Context, u store.AdminUser) bool { return e.svc.AllowLocalLogin(c, u) }})
	e.svc = &Service{DB: db, Auth: e.au, RBAC: e.rb, Audit: e.al, Now: clock, Secure: true, HardLock: true,
		Secrets: StaticSecrets{"acme-sso": e.idp.ClientSecret}, Log: slog.New(slog.NewJSONHandler(e.logs, nil)),
		Source: func(context.Context) (*config.SSO, string, error) { c := e.cfg; return &c, "acme.example", nil }}
	// a module that defines the role finance:approver
	m := store.Module{ID: "finance", Version: "1", Namespace: "mod-finance", ServiceAccount: "finance", State: store.StateEnabled, HMACSecret: "x",
		Manifest: config.Module{ID: "finance", Permissions: []string{"finance.approve"}, Roles: map[string][]string{"approver": {"finance.approve"}}}}
	if err := db.UpsertModule(ctx, m, nil, nil); err != nil {
		t.Fatal(err)
	}
	return e
}

func alice() ssotest.User {
	return ssotest.User{Sub: "u-123", Email: "alice@acme.example", EmailVerified: true, Name: "Alice", Groups: []string{"ops"}}
}

// start begins a login and returns the authorization URL and the state cookie value.
func (e *env) start(next string) (string, string) {
	e.t.Helper()
	loc, ck, err := e.svc.Begin(context.Background(), next)
	if err != nil {
		e.t.Fatalf("Begin: %v", err)
	}
	if ck.SameSite != http.SameSiteLaxMode || !ck.HttpOnly || !ck.Secure || ck.Path != "/auth/oidc" {
		e.t.Fatalf("state cookie %+v", ck)
	}
	return loc, ck.Value
}

func (e *env) callback(cb, cookie, oldToken string) (Result, error) {
	u, _ := url.Parse(cb)
	return e.svc.Complete(context.Background(), CompleteRequest{Query: u.Query(), StateCookie: cookie, IP: "203.0.113.9", OldToken: oldToken})
}

func (e *env) login(u ssotest.User) (Result, error) {
	e.t.Helper()
	loc, ck := e.start("/modules")
	return e.callback(e.idp.Approve(loc, u), ck, "")
}

func (e *env) mustLogin(u ssotest.User) Result {
	e.t.Helper()
	r, err := e.login(u)
	if err != nil {
		e.t.Fatalf("login: %v", err)
	}
	return r
}

func wantDenied(t *testing.T, err error, code string) {
	t.Helper()
	var d *Denied
	if !errors.As(err, &d) || (code != "" && d.Code != code) {
		t.Fatalf("want denied %q, got %v", code, err)
	}
}

func (e *env) roles(username string) []string {
	as, _ := e.rb.SubjectAssignments(context.Background(), "user:"+username)
	var out []string
	for _, a := range as {
		out = append(out, a.Role)
	}
	return out
}

func has(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (e *env) auditActions() string {
	recs, _ := e.al.List(context.Background(), 0, 1000)
	var b strings.Builder
	for _, r := range recs {
		b.WriteString(r.Action + " " + r.Target + " ")
		b.WriteString(r.Detail + "\n")
	}
	return b.String()
}

// --- the happy path ---------------------------------------------------------------------------

func TestLoginProvisionsMapsRolesAndStartsSession(t *testing.T) {
	e := newEnv(t, nil)
	r := e.mustLogin(alice())
	if r.Next != "/modules" {
		t.Fatalf("next %q", r.Next)
	}
	sess, user, err := e.au.Authenticate(context.Background(), r.Login.Token)
	if err != nil || sess.Source != store.SourceSSO || user.AuthSource != store.SourceSSO || sess.RevalidateAt.IsZero() {
		t.Fatalf("session %+v user %+v err %v", sess, user, err)
	}
	if !strings.HasPrefix(user.Username, "sso-") || strings.Contains(user.Username, "alice") {
		t.Fatalf("username %q", user.Username)
	}
	if got := e.roles(user.Username); len(got) != 1 || got[0] != "operator" {
		t.Fatalf("roles %v", got)
	}
	if user.PasswordHash != "!" || user.TOTPSecret != "" {
		t.Fatal("an SSO user has no password and no TOTP secret")
	}
	ident, err := e.db.SSOIdentityByUser(context.Background(), user.ID)
	if err != nil || ident.IssuerHash != IssuerHash(e.idp.Issuer) || ident.Subject != "u-123" {
		t.Fatalf("%+v %v", ident, err)
	}
	// The authorization request: PKCE S256, state, nonce, redirect.
	q := e.idp.LastAuthQuery
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || len(q.Get("state")) < 32 || len(q.Get("nonce")) < 32 ||
		q.Get("redirect_uri") != "http://admin.test/auth/oidc/callback" || q.Get("response_type") != "code" || !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("auth query %v", q)
	}
	log := e.auditActions()
	for _, w := range []string{"sso.provision", "rbac.sso_sync", "sso.login"} {
		if !strings.Contains(log, w) {
			t.Errorf("audit lacks %s:\n%s", w, log)
		}
	}
	if strings.Contains(log, "alice@acme.example") {
		t.Error("the audit log must not carry the email address")
	}
	// A second login reuses the same user.
	r2 := e.mustLogin(alice())
	_, user2, _ := e.au.Authenticate(context.Background(), r2.Login.Token)
	if user2.ID != user.ID || r2.Login.Token == r.Login.Token {
		t.Fatal("same user, new session token expected")
	}
}

func TestExternalSubjectFormat(t *testing.T) {
	id := Identity{IssuerHash: IssuerHash("https://idp.example"), Subject: "abc"}
	if got := id.ExternalSubject(); !strings.HasPrefix(got, "oidc:") || !strings.HasSuffix(got, ":abc") || len(IssuerHash("x")) != 16 {
		t.Fatal(got)
	}
}

// --- state, nonce, code ---------------------------------------------------------------------

func TestReplayedCallbackIsRefused(t *testing.T) {
	e := newEnv(t, nil)
	loc, ck := e.start("")
	cb := e.idp.Approve(loc, alice())
	if _, err := e.callback(cb, ck, ""); err != nil {
		t.Fatal(err)
	}
	_, err := e.callback(cb, ck, "")
	wantDenied(t, err, "state")
	if e.idp.TokenHits != 1 {
		t.Fatalf("the replay must not reach the token endpoint (%d hits)", e.idp.TokenHits)
	}
}

func TestReplayedCodeWithFreshStateIsRefusedByProvider(t *testing.T) {
	e := newEnv(t, nil)
	loc, ck := e.start("")
	cb := e.idp.Approve(loc, alice())
	if _, err := e.callback(cb, ck, ""); err != nil {
		t.Fatal(err)
	}
	// An attacker with the code and a new login transaction of their own.
	loc2, ck2 := e.start("")
	u, _ := url.Parse(cb)
	cu, _ := url.Parse(loc2)
	q := u.Query()
	q.Set("state", cu.Query().Get("state"))
	_, err := e.svc.Complete(context.Background(), CompleteRequest{Query: q, StateCookie: ck2, IP: "203.0.113.9"})
	wantDenied(t, err, "exchange")
}

func TestStateMismatchAndBadCookies(t *testing.T) {
	e := newEnv(t, nil)
	loc, ck := e.start("")
	cb := e.idp.Approve(loc, alice())
	u, _ := url.Parse(cb)
	q := u.Query()

	bad := url.Values{"code": q["code"], "state": {"not-the-state"}}
	_, err := e.svc.Complete(context.Background(), CompleteRequest{Query: bad, StateCookie: ck, IP: "198.51.100.1"})
	wantDenied(t, err, "state")

	for name, cookie := range map[string]string{"empty": "", "garbage": "abc.def", "tampered": strings.Replace(ck, ck[:4], "AAAA", 1), "unsigned": strings.SplitN(ck, ".", 2)[0] + "."} {
		_, err := e.svc.Complete(context.Background(), CompleteRequest{Query: q, StateCookie: cookie, IP: "198.51.100.1"})
		if err == nil {
			t.Errorf("%s cookie accepted", name)
		}
	}
	// Expired transaction.
	e.now = e.now.Add(StateTTL + time.Minute)
	_, err = e.svc.Complete(context.Background(), CompleteRequest{Query: q, StateCookie: ck, IP: "198.51.100.1"})
	wantDenied(t, err, "state")
	if e.idp.TokenHits != 0 {
		t.Fatal("no token request may be made for an invalid transaction")
	}
}

func TestNonceMismatch(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.Mutate = func(c map[string]any) { c["nonce"] = "someone-elses-nonce" }
	_, err := e.login(alice())
	wantDenied(t, err, "token")
	e.idp.Mutate = func(c map[string]any) { delete(c, "nonce") }
	_, err = e.login(alice())
	wantDenied(t, err, "token")
}

func TestProviderErrorParameter(t *testing.T) {
	e := newEnv(t, nil)
	loc, ck := e.start("")
	_ = loc
	cu, _ := url.Parse(loc)
	q := url.Values{"error": {"access_denied"}, "error_description": {"<script>"}, "state": {cu.Query().Get("state")}}
	_, err := e.svc.Complete(context.Background(), CompleteRequest{Query: q, StateCookie: ck, IP: "198.51.100.1"})
	wantDenied(t, err, "idp_error")
}

// --- ID token claims ----------------------------------------------------------------------------

func TestClaimsAreEnforced(t *testing.T) {
	cases := map[string]func(c map[string]any, now time.Time){
		"wrong issuer":              func(c map[string]any, _ time.Time) { c["iss"] = "https://evil.example" },
		"issuer with slash":         func(c map[string]any, _ time.Time) { c["iss"] = c["iss"].(string) + "/" },
		"wrong audience":            func(c map[string]any, _ time.Time) { c["aud"] = "other-client" },
		"audience list without azp": func(c map[string]any, _ time.Time) { c["aud"] = []string{"can-admin", "other"} },
		"azp for another client":    func(c map[string]any, _ time.Time) { c["aud"] = []string{"can-admin", "other"}; c["azp"] = "other" },
		"azp mismatch single aud":   func(c map[string]any, _ time.Time) { c["azp"] = "other" },
		"expired":                   func(c map[string]any, n time.Time) { c["exp"] = n.Add(-5 * time.Minute).Unix() },
		"no exp":                    func(c map[string]any, _ time.Time) { delete(c, "exp") },
		"not yet valid":             func(c map[string]any, n time.Time) { c["nbf"] = n.Add(10 * time.Minute).Unix() },
		"issued in the future":      func(c map[string]any, n time.Time) { c["iat"] = n.Add(10 * time.Minute).Unix() },
		"stale iat": func(c map[string]any, n time.Time) {
			c["iat"] = n.Add(-time.Hour).Unix()
			c["exp"] = n.Add(time.Hour).Unix()
		},
		"no subject":      func(c map[string]any, _ time.Time) { delete(c, "sub") },
		"numeric subject": func(c map[string]any, _ time.Time) { c["sub"] = 42 },
		"wrong at_hash":   func(c map[string]any, _ time.Time) { c["at_hash"] = "AAAAAAAAAAAAAAAAAAAAAA" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.idp.Mutate = func(c map[string]any) { mut(c, e.now) }
			_, err := e.login(alice())
			wantDenied(t, err, "token")
			if n, _ := e.db.ListSSOIdentities(context.Background(), 10); len(n) != 0 {
				t.Fatal("a refused token must not provision a user")
			}
		})
	}
}

func TestAudienceListWithMatchingAzpIsAccepted(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.Mutate = func(c map[string]any) { c["aud"] = []string{"can-admin", "other"}; c["azp"] = "can-admin" }
	e.mustLogin(alice())
}

func TestSkewIsTolerated(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.Mutate = func(c map[string]any) {
		c["exp"] = e.now.Add(-10 * time.Second).Unix()
		c["nbf"] = e.now.Add(10 * time.Second).Unix()
	}
	e.mustLogin(alice())
}

func TestAtHashAbsentIsFine(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.OmitAtHash = true
	e.mustLogin(alice())
}

func TestAlgorithmAllowlist(t *testing.T) {
	for _, alg := range []string{"none", "HS256"} {
		t.Run(alg, func(t *testing.T) {
			e := newEnv(t, nil)
			e.mustLogin(alice())                           // warm the key cache
			e.now = e.now.Add(keyMinRefetch + time.Second) // a refetch would be allowed now
			hits := e.idp.JWKSHits
			e.idp.OverrideID = func(i *ssotest.IdP, g ssotest.Grant) string { return i.IDToken(g, alg) }
			_, err := e.login(alice())
			wantDenied(t, err, "token")
			if e.idp.JWKSHits != hits {
				t.Fatal("a token with a forbidden algorithm must be refused before any key lookup or refetch")
			}
		})
	}
	t.Run("HS256 signed with the public key bytes as secret", func(t *testing.T) {
		e := newEnv(t, nil)
		e.idp.OverrideID = func(i *ssotest.IdP, g ssotest.Grant) string { return i.IDToken(g, "HS256") }
		_, err := e.login(alice())
		wantDenied(t, err, "token")
	})
	t.Run("ES256 is accepted", func(t *testing.T) {
		e := newEnv(t, nil)
		e.idp.OverrideID = func(i *ssotest.IdP, g ssotest.Grant) string { return i.IDToken(g, "ES256") }
		e.mustLogin(alice())
	})
	t.Run("critical headers are refused", func(t *testing.T) {
		e := newEnv(t, nil)
		e.idp.OverrideID = func(i *ssotest.IdP, g ssotest.Grant) string {
			return i.Sign("RS256", map[string]any{"crit": []string{"exp"}}, i.Claims(g))
		}
		_, err := e.login(alice())
		wantDenied(t, err, "token")
	})
	t.Run("signature by an unpublished key", func(t *testing.T) {
		e := newEnv(t, nil)
		e.idp.OverrideID = func(i *ssotest.IdP, g ssotest.Grant) string { return i.SignWithForeignKey(i.Claims(g)) }
		_, err := e.login(alice())
		wantDenied(t, err, "token")
	})
	t.Run("tampered claims", func(t *testing.T) {
		e := newEnv(t, nil)
		e.idp.OverrideID = func(i *ssotest.IdP, g ssotest.Grant) string {
			parts := strings.Split(i.IDToken(g, "RS256"), ".")
			c := i.Claims(g)
			c["sub"] = "admin"
			cb, _ := json.Marshal(c)
			parts[1] = base64url(cb)
			return strings.Join(parts, ".")
		}
		_, err := e.login(alice())
		wantDenied(t, err, "token")
	})
}

// --- keys ----------------------------------------------------------------------------------------

func TestKeyRotationRefetchesOnceAndIsRateLimited(t *testing.T) {
	e := newEnv(t, nil)
	e.mustLogin(alice())
	if e.idp.JWKSHits != 1 {
		t.Fatalf("jwks hits %d", e.idp.JWKSHits)
	}
	// Rotation: the provider signs with a new kid. A refetch is allowed after the minimum interval.
	e.idp.RotateKey("rsa-2")
	e.now = e.now.Add(keyMinRefetch + time.Second)
	e.mustLogin(alice())
	if e.idp.JWKSHits != 2 {
		t.Fatalf("jwks hits %d, want 2", e.idp.JWKSHits)
	}
	// A second rotation right away must not trigger another fetch: fail closed instead.
	e.idp.RotateKey("rsa-3")
	_, err := e.login(alice())
	wantDenied(t, err, "token")
	if e.idp.JWKSHits != 2 {
		t.Fatalf("refetch not rate limited: %d", e.idp.JWKSHits)
	}
	// Later it recovers.
	e.now = e.now.Add(keyMinRefetch + time.Second)
	e.mustLogin(alice())
}

func TestForgedKidFloodDoesNotFloodTheProvider(t *testing.T) {
	e := newEnv(t, nil)
	e.mustLogin(alice())
	e.idp.OverrideID = func(i *ssotest.IdP, g ssotest.Grant) string {
		return i.Sign("RS256", map[string]any{"kid": "forged"}, i.Claims(g))
	}
	for k := 0; k < 10; k++ {
		_, err := e.login(alice())
		wantDenied(t, err, "token")
	}
	if e.idp.JWKSHits > 2 {
		t.Fatalf("forged kids caused %d JWKS fetches", e.idp.JWKSHits)
	}
}

func TestJWKSFetchFailureFailsClosed(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.JWKSFail = true
	_, err := e.login(alice())
	wantDenied(t, err, "token")
	if n, _ := e.db.ListSSOIdentities(context.Background(), 10); len(n) != 0 {
		t.Fatal("no user may be provisioned")
	}
}

func TestJWKSOutageAfterWarmupKeepsKnownKeysAndRefusesUnknown(t *testing.T) {
	e := newEnv(t, nil)
	e.mustLogin(alice())
	e.idp.JWKSFail = true
	e.now = e.now.Add(2 * time.Hour) // keys are due for a refresh, which fails
	e.mustLogin(alice())             // the known key is still good
	e.idp.RotateKey("rsa-9")
	e.now = e.now.Add(time.Minute)
	_, err := e.login(alice())
	wantDenied(t, err, "token")
}

func TestParseJWKSRejectsWeakAndUnusableKeys(t *testing.T) {
	for _, doc := range []string{
		`{"keys":[]}`, `not json`, `{"keys":[{"kty":"oct","k":"AAAA"}]}`,
		`{"keys":[{"kty":"RSA","n":"AQAB","e":"AQAB"}]}`,
		`{"keys":[{"kty":"EC","crv":"P-384","x":"AA","y":"AA"}]}`,
		`{"keys":[{"kty":"EC","crv":"P-256","x":"AQ","y":"AQ"}]}`,
		`{"keys":[{"kty":"RSA","use":"enc","n":"AQAB","e":"AQAB"}]}`,
	} {
		if ks, err := ParseJWKS([]byte(doc)); err == nil {
			t.Errorf("%s accepted: %v", doc, ks)
		}
	}
}

// --- policy: email, amr, roles ---------------------------------------------------------------

func TestUnverifiedEmailIsRefused(t *testing.T) {
	e := newEnv(t, nil)
	u := alice()
	u.EmailVerified = false
	_, err := e.login(u)
	wantDenied(t, err, "email_unverified")
}

func TestEmailDomainAllowlist(t *testing.T) {
	e := newEnv(t, func(c *config.SSO) { c.AllowedEmailDomains = []string{"acme.example"} })
	u := alice()
	u.Email = "mallory@evil.example"
	_, err := e.login(u)
	wantDenied(t, err, "email_domain")
	u.Email = "mallory@sub.acme.example" // a subdomain is not the domain
	_, err = e.login(u)
	wantDenied(t, err, "email_domain")
	u.Email = "mallory@acme.example@evil.example"
	_, err = e.login(u)
	wantDenied(t, err, "email_domain")
	e.mustLogin(alice())
}

func TestEmailVerifiedNotRequiredWhenConfiguredOff(t *testing.T) {
	f := false
	e := newEnv(t, func(c *config.SSO) { c.RequireEmailVerified = &f })
	u := alice()
	u.EmailVerified = false
	e.mustLogin(u)
}

func TestAMRRequirement(t *testing.T) {
	e := newEnv(t, func(c *config.SSO) { c.RequireAMR = []string{"mfa", "hwk"} })
	_, err := e.login(alice())
	wantDenied(t, err, "amr")
	u := alice()
	u.AMR = []string{"pwd"}
	_, err = e.login(u)
	wantDenied(t, err, "amr")
	u.AMR = []string{"pwd", "mfa"}
	e.mustLogin(u)
}

func TestUnverifiedEmailCannotTakeOverAnAccount(t *testing.T) {
	e := newEnv(t, nil)
	// root is the owner who signs in locally. An attacker's account at the provider claims root's
	// name and an email that looks like the owner's.
	for _, ver := range []bool{false, true} {
		u := ssotest.User{Sub: "attacker", Email: "root@acme.example", EmailVerified: ver, Name: "root", Groups: []string{"ops"}}
		r, err := e.login(u)
		if !ver {
			wantDenied(t, err, "email_unverified")
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		_, user, _ := e.au.Authenticate(context.Background(), r.Login.Token)
		if user.Username == "root" || user.AuthSource != store.SourceSSO {
			t.Fatalf("signed in as %q: email must never select an account", user.Username)
		}
		if has(e.roles(user.Username), "owner") {
			t.Fatal("no owner role for the attacker")
		}
	}
	root, _ := e.db.AdminByUsername(context.Background(), "root")
	if _, err := e.db.SSOIdentityByUser(context.Background(), root.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("root must not have been linked")
	}
}

func TestSameEmailDifferentIssuerSubjectIsADifferentUser(t *testing.T) {
	e := newEnv(t, nil)
	a := e.mustLogin(alice())
	u2 := alice()
	u2.Sub = "u-999"
	b := e.mustLogin(u2)
	_, ua, _ := e.au.Authenticate(context.Background(), a.Login.Token)
	_, ub, _ := e.au.Authenticate(context.Background(), b.Login.Token)
	if ua.ID == ub.ID {
		t.Fatal("two subjects, one user")
	}
}

func TestGroupRemovalRevokesRolesOnNextLogin(t *testing.T) {
	e := newEnv(t, nil)
	u := alice()
	u.Groups = []string{"ops", "auditors", "fin"}
	r := e.mustLogin(u)
	_, user, _ := e.au.Authenticate(context.Background(), r.Login.Token)
	got := e.roles(user.Username)
	for _, w := range []string{"operator", "auditor", "finance:approver"} {
		if !has(got, w) {
			t.Fatalf("roles %v lack %s", got, w)
		}
	}
	// A role an administrator assigned by hand is not the sync's to remove.
	if err := e.rb.Grant(context.Background(), "root", "user:"+user.Username, "auditor", "scope/x"); err != nil {
		t.Fatal(err)
	}
	u.Groups = []string{"ops"}
	e.mustLogin(u)
	got = e.roles(user.Username)
	if has(got, "finance:approver") || !has(got, "operator") {
		t.Fatalf("roles after removal: %v", got)
	}
	as, _ := e.rb.SubjectAssignments(context.Background(), "user:"+user.Username)
	manual := false
	for _, a := range as {
		manual = manual || (a.Role == "auditor" && a.Scope == "scope/x")
	}
	if !manual {
		t.Fatal("the hand-made scoped assignment must survive the sync")
	}
	u.Groups = nil
	e.mustLogin(u)
	if has(e.roles(user.Username), "operator") {
		t.Fatalf("operator must be revoked: %v", e.roles(user.Username))
	}
	recs, _ := e.al.List(context.Background(), 0, 1000)
	var seen bool
	for _, r := range recs {
		if r.Action != "rbac.sso_sync" {
			continue
		}
		var d struct{ Before, After, Removed []string }
		_ = json.Unmarshal([]byte(r.Detail), &d)
		if len(d.Removed) == 2 && len(d.Before) == 3 && len(d.After) == 1 {
			seen = true
		}
	}
	if !seen {
		t.Errorf("revocation not audited with before/after:\n%s", e.auditActions())
	}
}

func TestOwnerMappingNeedsConfirmationAndNeverRemovesTheLastOwner(t *testing.T) {
	e := newEnv(t, func(c *config.SSO) { c.ConfirmOwnerMapping = false })
	if _, _, err := e.svc.Begin(context.Background(), ""); err == nil {
		t.Fatal("an unconfirmed owner mapping makes the configuration invalid")
	}
	// Unknown mapped roles (a module that is not installed) are skipped, not fatal.
	e2 := newEnv(t, func(c *config.SSO) { c.GroupRoles["ghosts"] = []string{"ghost:viewer"} })
	u := alice()
	u.Groups = []string{"ghosts", "ops"}
	r := e2.mustLogin(u)
	_, user, _ := e2.au.Authenticate(context.Background(), r.Login.Token)
	if got := e2.roles(user.Username); len(got) != 1 || got[0] != "operator" {
		t.Fatalf("roles %v", got)
	}
	// An SSO owner who is the only owner is kept when the group is dropped.
	e3 := newEnv(t, nil)
	if err := e3.rb.Revoke(context.Background(), rbac.CLIActor, "user:root", "owner", ""); err == nil {
		// root is the only owner: expected to fail; make the SSO user the only owner another way
		t.Fatal("last owner revoked")
	}
	adm := alice()
	adm.Groups = []string{"admins"}
	r3 := e3.mustLogin(adm)
	_, au, _ := e3.au.Authenticate(context.Background(), r3.Login.Token)
	if !has(e3.roles(au.Username), "owner") {
		t.Fatal("the mapped owner role must be assigned")
	}
	adm.Groups = nil
	e3.mustLogin(adm)
	if has(e3.roles(au.Username), "owner") {
		t.Fatal("with root also an owner, the SSO owner role is removed")
	}
}

func TestRequireMappedRole(t *testing.T) {
	e := newEnv(t, func(c *config.SSO) { c.RequireMappedRole = true })
	u := alice()
	u.Groups = []string{"nobody"}
	_, err := e.login(u)
	wantDenied(t, err, "no_role")
	if n, _ := e.db.ListSSOIdentities(context.Background(), 10); len(n) != 0 {
		t.Fatal("a refused user must not be provisioned")
	}
	e.mustLogin(alice())
	u2 := alice()
	u2.Groups = nil
	_, err = e.login(u2)
	wantDenied(t, err, "no_role")
	// the earlier operator role was revoked by the refused sign-in
	idn, _ := e.db.SSOIdentityByExternal(context.Background(), IssuerHash(e.idp.Issuer), "u-123")
	if has(e.roles(idn.Username), "operator") {
		t.Fatal("roles must be revoked even when the sign-in is refused")
	}
}

// --- session machinery --------------------------------------------------------------------------

func TestSessionFixationOldSessionIsDestroyed(t *testing.T) {
	e := newEnv(t, nil)
	first := e.mustLogin(alice())
	loc, ck := e.start("")
	r, err := e.callback(e.idp.Approve(loc, alice()), ck, first.Login.Token)
	if err != nil {
		t.Fatal(err)
	}
	if r.Login.Token == first.Login.Token {
		t.Fatal("token reused")
	}
	if _, _, err := e.au.Authenticate(context.Background(), first.Login.Token); err == nil {
		t.Fatal("the session the browser presented before login must be destroyed")
	}
}

func TestSessionsAreRevalidated(t *testing.T) {
	e := newEnv(t, func(c *config.SSO) { c.RevalidateInterval = "10m" })
	r := e.mustLogin(alice())
	if _, _, err := e.au.Authenticate(context.Background(), r.Login.Token); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(11 * time.Minute)
	if _, _, err := e.au.Authenticate(context.Background(), r.Login.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("an SSO session past its revalidation time must not authenticate: %v", err)
	}
}

func TestLocalLoginPolicy(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(c *config.SSO) { c.LocalLogin = config.LocalLoginOwnerOnly })
	root, _ := e.db.AdminByUsername(ctx, "root")
	if !e.svc.AllowLocalLogin(ctx, root) {
		t.Fatal("owner_only: the owner may sign in locally")
	}
	hash, _ := auth.HashPassword(ownerPW)
	op, _ := e.db.CreateAdmin(ctx, "ops", hash, auth.NewTOTPSecret())
	if e.svc.AllowLocalLogin(ctx, op) {
		t.Fatal("owner_only: a non-owner may not sign in locally")
	}
	e.cfg.LocalLogin = config.LocalLoginDisabled
	e.cfg.ConfirmDisableLocalLogin = true
	e.svc.Reload()
	if !e.svc.AllowLocalLogin(ctx, root) {
		t.Fatal("disabled but no SSO owner exists: the local owner keeps the break-glass login")
	}
	adm := alice()
	adm.Groups = []string{"admins"}
	e.mustLogin(adm)
	if e.svc.AllowLocalLogin(ctx, root) {
		t.Fatal("disabled with an SSO owner: local login is off")
	}
	st, _ := e.svc.Status(ctx)
	if st.LocalLoginEffective != config.LocalLoginDisabled {
		t.Fatalf("%+v", st)
	}
}

func TestSSOUserCannotLoginLocally(t *testing.T) {
	e := newEnv(t, nil)
	r := e.mustLogin(alice())
	_, user, _ := e.au.Authenticate(context.Background(), r.Login.Token)
	for _, pw := range []string{"", "!", ownerPW} {
		if _, err := e.au.Login(context.Background(), auth.LoginRequest{Username: user.Username, Password: pw, Code: "000000", IP: "203.0.113.1"}); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("password %q: %v", pw, err)
		}
	}
}

// --- throttling ---------------------------------------------------------------------------------

func TestInvalidCallbacksAreThrottledButValidOnesNeverAre(t *testing.T) {
	e := newEnv(t, nil)
	ip := "203.0.113.9"
	var locked *auth.LockedError
	for k := 0; k < 30; k++ {
		_, err := e.svc.Complete(context.Background(), CompleteRequest{Query: url.Values{"code": {"x"}, "state": {"y"}}, StateCookie: "bad", IP: ip})
		if errors.As(err, &locked) {
			break
		}
	}
	if locked == nil {
		t.Fatal("a flood of invalid callbacks must eventually be refused in hard-lock mode")
	}
	// The real user behind the same address, with a genuine transaction, still gets in.
	e.mustLogin(alice())
}

// --- outbound safety ---------------------------------------------------------------------------

func TestDiscoveryMustMatchTheConfiguredIssuer(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.DiscIssuer = "https://evil.example/realms/acme"
	if _, _, err := e.svc.Begin(context.Background(), ""); err == nil {
		t.Fatal("a discovery document for another issuer must be refused")
	}
}

func TestDiscoveryRedirectIsNotFollowed(t *testing.T) {
	e := newEnv(t, nil)
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer target.Close()
	e.idp.RedirectDisc = target.URL + "/latest/meta-data/"
	if _, _, err := e.svc.Begin(context.Background(), ""); err == nil {
		t.Fatal("redirects must not be followed")
	}
	if hits != 0 {
		t.Fatalf("the redirect target was contacted %d times", hits)
	}
}

func TestSSRFGuardRefusesNonPublicAddresses(t *testing.T) {
	e := newEnv(t, nil)
	c := NewHTTPClient(false) // the production client, not the dev one
	var out map[string]any
	err := call(context.Background(), c, "GET", e.idp.Issuer+"/.well-known/openid-configuration", nil, [2]string{}, "", 1<<10, &out)
	if err == nil || !errors.Is(err, ErrSSRF) && !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("a loopback provider must be refused by the dialer: %v", err)
	}
	if e.idp.DiscHits != 0 {
		t.Fatal("no connection may have been made")
	}
}

func TestResponsesAreSizeCapped(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.WithKeys = nil
	c := NewHTTPClient(true)
	var raw []byte
	err := call(context.Background(), c, "GET", e.idp.Issuer+"/jwks", nil, [2]string{}, "", 100, &raw)
	if err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("an oversized answer must be refused: %v", err)
	}
}

func TestConfigRefusesPrivateIssuerWithoutDevFlag(t *testing.T) {
	e := newEnv(t, func(c *config.SSO) { c.AllowInsecureDev = false })
	if _, _, err := e.svc.Begin(context.Background(), ""); err == nil {
		t.Fatal("http://127.0.0.1 issuer without allow_insecure_dev must be refused")
	}
	st, _ := e.svc.Status(context.Background())
	if st.Enabled || st.Error == "" {
		t.Fatalf("%+v", st)
	}
}

func TestMissingSecretFailsClosed(t *testing.T) {
	e := newEnv(t, nil)
	e.svc.Secrets = StaticSecrets{}
	if _, _, err := e.svc.Begin(context.Background(), ""); err == nil {
		t.Fatal("no secret, no sign-in")
	}
}

func TestNotConfigured(t *testing.T) {
	e := newEnv(t, nil)
	e.cfg.Enabled = false
	e.svc.Reload()
	if e.svc.Enabled(context.Background()) {
		t.Fatal("disabled")
	}
	if _, _, err := e.svc.Begin(context.Background(), ""); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
	if _, err := e.svc.Complete(context.Background(), CompleteRequest{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatal(err)
	}
}

func TestStatusNeverContainsTheSecretAndTestDiscoveryWorks(t *testing.T) {
	e := newEnv(t, nil)
	st, err := e.svc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), e.idp.ClientSecret) || !st.Enabled || st.ClientSecretRef != "acme-sso" || len(st.Mappings) != 4 {
		t.Fatalf("%s", b)
	}
	res, err := e.svc.TestDiscovery(context.Background(), "root")
	if err != nil || !res.OK || res.Keys != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	e.idp.DiscIssuer = "https://other.example"
	res, _ = e.svc.TestDiscovery(context.Background(), "root")
	if res.OK {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(e.auditActions(), "sso.test") {
		t.Fatal("the test is audited")
	}
	for _, l := range strings.Split(e.logs.String(), "\n") {
		if strings.Contains(l, e.idp.ClientSecret) {
			t.Fatal("the secret reached the log")
		}
	}
}

func TestNoPersonalDataInLogs(t *testing.T) {
	e := newEnv(t, nil)
	e.idp.Mutate = func(c map[string]any) { c["aud"] = "other" }
	_, _ = e.login(alice())
	e.mustLoginNoMutate()
	for _, pii := range []string{"alice@acme.example", "Alice", "u-123"} {
		if strings.Contains(e.logs.String(), pii) {
			t.Errorf("log contains %q:\n%s", pii, e.logs.String())
		}
	}
}

func (e *env) mustLoginNoMutate() {
	e.idp.Mutate = nil
	e.mustLogin(alice())
}

// --- linking ------------------------------------------------------------------------------------

func TestLinkIsExplicitOwnerOnlyAndAudited(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	r := e.mustLogin(alice())
	_, jit, _ := e.au.Authenticate(ctx, r.Login.Token)
	ident, _ := e.db.SSOIdentityByUser(ctx, jit.ID)
	hash, _ := auth.HashPassword(ownerPW)
	if _, err := e.db.CreateAdmin(ctx, "alice", hash, auth.NewTOTPSecret()); err != nil {
		t.Fatal(err)
	}
	if err := e.rb.Grant(ctx, "root", "user:alice", "auditor", ""); err != nil {
		t.Fatal(err)
	}
	// A non-owner (the JIT operator) may not link.
	if err := e.svc.Link(ctx, jit.Username, ident.ID, "alice"); !errors.Is(err, ErrLinkForbidden) {
		t.Fatalf("%v", err)
	}
	if err := e.svc.Link(ctx, "root", ident.ID, "nobody"); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("%v", err)
	}
	if err := e.svc.Link(ctx, "root", ident.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.au.Authenticate(ctx, r.Login.Token); err == nil {
		t.Fatal("the provisioned user's sessions end with the link")
	}
	// The next SSO sign-in is the local account, with its hand-assigned roles plus the mapped ones.
	r2 := e.mustLogin(alice())
	_, u2, _ := e.au.Authenticate(ctx, r2.Login.Token)
	if u2.Username != "alice" || u2.AuthSource != store.SourceLocal {
		t.Fatalf("%+v", u2)
	}
	if got := e.roles("alice"); !has(got, "auditor") || !has(got, "operator") {
		t.Fatalf("roles %v", got)
	}
	if !strings.Contains(e.auditActions(), "sso.link") {
		t.Fatal("link must be audited")
	}
	// Linking a second identity to the same account is refused.
	u := alice()
	u.Sub = "other"
	e.mustLogin(u)
	idn, _ := e.db.SSOIdentityByExternal(ctx, IssuerHash(e.idp.Issuer), "other")
	if err := e.svc.Link(ctx, "root", idn.ID, "alice"); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("%v", err)
	}
	// Unlink removes the link, keeps the local account.
	idn2, _ := e.db.SSOIdentityByExternal(ctx, IssuerHash(e.idp.Issuer), "u-123")
	if err := e.svc.Unlink(ctx, "root", idn2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.AdminByUsername(ctx, "alice"); err != nil {
		t.Fatal("the local account must survive an unlink")
	}
}

// --- module introspection --------------------------------------------------------------------

func TestIntrospectSession(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, nil)
	u := alice()
	u.AMR = []string{"pwd", "mfa"}
	u.Groups = []string{"ops", "fin", "extra"}
	r := e.mustLogin(u)
	ui, err := e.svc.IntrospectSession(ctx, r.Login.Token)
	if err != nil || !ui.Active || ui.Kind != "user" || !strings.HasPrefix(ui.Sub, "oidc:") || !strings.HasSuffix(ui.Sub, ":u-123") ||
		ui.Email != "alice@acme.example" || !has(ui.Roles, "operator") || !has(ui.Roles, "finance:approver") || !has(ui.Groups, "extra") || !has(ui.AMR, "mfa") {
		t.Fatalf("%+v %v", ui, err)
	}
	// Local sessions, garbage and revoked sessions are inactive.
	l, err := e.au.CreateSSOSession(ctx, 1, "1.1.1.1", 0) // a session of root, source sso, but root has no identity
	if err != nil {
		t.Fatal(err)
	}
	if ui, _ := e.svc.IntrospectSession(ctx, l.Token); ui.Active {
		t.Fatal("a session of a user without an SSO identity is inactive")
	}
	if ui, _ := e.svc.IntrospectSession(ctx, "garbage"); ui.Active {
		t.Fatal("garbage")
	}
	_ = e.au.Logout(ctx, r.Login.Token)
	if ui, _ := e.svc.IntrospectSession(ctx, r.Login.Token); ui.Active {
		t.Fatal("revoked")
	}
}

func TestLogoutURL(t *testing.T) {
	e := newEnv(t, func(c *config.SSO) { c.RPLogout = true })
	r := e.mustLogin(alice())
	if got := e.svc.LogoutURL(context.Background(), r.Login.Session); !strings.HasPrefix(got, e.idp.Issuer+"/logout?") || !strings.Contains(got, "post_logout_redirect_uri=http%3A%2F%2Fadmin.test%2Flogin") {
		t.Fatalf("%q", got)
	}
	if got := e.svc.LogoutURL(context.Background(), store.Session{Source: store.SourceLocal}); got != "" {
		t.Fatalf("%q", got)
	}
	e.cfg.RPLogout = false
	e.svc.Reload()
	if got := e.svc.LogoutURL(context.Background(), r.Login.Session); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"": "/", "/modules": "/modules", "/audit?actor=x": "/audit?actor=x",
		"//evil.example": "/", "https://evil.example": "/", "/\\evil.example": "/", "/%0d%0a": "/%0d%0a", "javascript:alert(1)": "/",
		"/a\r\nSet-Cookie: x=y": "/", "evil": "/", "/login": "/", "/auth/oidc/login": "/", "/x\x00": "/", "///evil": "/",
		"/" + strings.Repeat("a", 300): "/",
	} {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapRolesDeterministic(t *testing.T) {
	cfg := config.SSO{GroupRoles: map[string][]string{"a": {"operator", "auditor"}, "b": {"operator"}}}
	got := MapRoles(cfg, []string{"b", "a", "zzz"})
	if strings.Join(got, ",") != "auditor,operator" {
		t.Fatal(got)
	}
}

func base64url(b []byte) string {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out []byte
	for i := 0; i < len(b); i += 3 {
		var v uint32
		n := 0
		for j := 0; j < 3; j++ {
			v <<= 8
			if i+j < len(b) {
				v |= uint32(b[i+j])
				n++
			}
		}
		for k := 0; k < n+1; k++ {
			out = append(out, enc[(v>>(18-6*uint(k)))&63])
		}
	}
	return string(out)
}

func TestDiscoveryIsSingleFlightAndSurvivesACancelledCaller(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.svc.Status(context.Background()); err != nil { // load the provider once
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := e.svc.Begin(context.Background(), ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if e.idp.DiscHits != 1 {
		t.Fatalf("discovery fetched %d times for 12 concurrent logins", e.idp.DiscHits)
	}
	// A caller whose request is already cancelled does not poison the shared fetch.
	e2 := newEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := e2.svc.Begin(ctx, ""); err != nil {
		t.Fatalf("the shared fetch must not depend on the first caller's context: %v", err)
	}
}

func TestFlowCookieNames(t *testing.T) {
	if n, ok := FlowCookieName("abcdEFG1" + strings.Repeat("x", 32)); !ok || n != "can_oidc_abcdEFG1" {
		t.Fatal(n, ok)
	}
	for _, bad := range []string{"", "short", "ab;d EFG" + strings.Repeat("x", 32), "../../../" + strings.Repeat("x", 32), strings.Repeat("a", 200)} {
		if _, ok := FlowCookieName(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTwoLoginsAtOnceDoNotInterfere(t *testing.T) {
	e := newEnv(t, nil)
	loc1, ck1 := e.start("/a")
	loc2, ck2 := e.start("/b")
	r2, err := e.callback(e.idp.Approve(loc2, alice()), ck2, "")
	if err != nil || r2.Next != "/b" {
		t.Fatal(err)
	}
	r1, err := e.callback(e.idp.Approve(loc1, alice()), ck1, "")
	if err != nil || r1.Next != "/a" {
		t.Fatal(err)
	}
}

func TestLocalLoginDisabledFallsBackToOwnerOnlyWhenSSOIsFailing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(c *config.SSO) { c.LocalLogin = config.LocalLoginDisabled; c.ConfirmDisableLocalLogin = true })
	adm := alice()
	adm.Groups = []string{"admins"}
	e.mustLogin(adm)
	root, _ := e.db.AdminByUsername(ctx, "root")
	if e.svc.AllowLocalLogin(ctx, root) {
		t.Fatal("disabled and healthy: the local owner is refused")
	}
	e.svc.Secrets = StaticSecrets{} // the secret becomes unreadable
	e.svc.Reload()
	if !e.svc.AllowLocalLogin(ctx, root) {
		t.Fatal("SSO failing: the owner must keep the break-glass login")
	}
	st, _ := e.svc.Status(ctx)
	if st.LocalLoginEffective != config.LocalLoginOwnerOnly {
		t.Fatalf("%+v", st)
	}
	hash, _ := auth.HashPassword(ownerPW)
	op, _ := e.db.CreateAdmin(ctx, "ops", hash, auth.NewTOTPSecret())
	if e.svc.AllowLocalLogin(ctx, op) {
		t.Fatal("a non-owner is not let in by the fallback")
	}
}
