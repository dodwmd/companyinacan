package sso

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
)

// Token validation limits.
const (
	clockSkew      = 30 * time.Second
	maxIDTokenAge  = 10 * time.Minute // iat older than this is a replayed or stale token
	maxGroups      = 500
	maxClaimString = 512
	discoveryTTL   = time.Hour
)

// Discovery is the subset of the provider metadata that is used.
type Discovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	EndSessionEndpoint    string   `json:"end_session_endpoint"`
	IDTokenAlgs           []string `json:"id_token_signing_alg_values_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

// Identity is what a verified sign-in yields.
type Identity struct {
	Issuer        string
	IssuerHash    string
	Subject       string // the subject claim
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string
	AMR           []string
	// AuthTime is the auth_time claim: when the person last authenticated at the provider (zero =
	// the provider did not say).
	AuthTime time.Time
}

// ExternalSubject is the stable subject of an identity: oidc:<issuer-hash>:<sub>.
func (i Identity) ExternalSubject() string { return "oidc:" + i.IssuerHash + ":" + i.Subject }

// IssuerHash is the first 16 hex characters of sha256(issuer).
func IssuerHash(issuer string) string {
	h := sha256.Sum256([]byte(issuer))
	return hex.EncodeToString(h[:])[:16]
}

// Provider talks to one identity provider with one configuration.
type Provider struct {
	cfg    config.SSO // normalised
	secret string
	client *http.Client
	now    func() time.Time
	keys   *keySet

	mu         sync.Mutex
	disc       *Discovery
	discAt     time.Time
	discTry    time.Time
	discFlight *flight
}

// NewProvider returns a provider for a validated, normalised configuration.
func NewProvider(cfg config.SSO, secret string, client *http.Client, now func() time.Time) *Provider {
	if client == nil {
		client = NewHTTPClient(cfg.AllowInsecureDev)
	}
	if now == nil {
		now = time.Now
	}
	p := &Provider{cfg: cfg, secret: secret, client: client, now: now}
	p.keys = newKeySet(p.fetchKeys, now)
	return p
}

// RedirectURI is the callback URL registered at the provider.
func (p *Provider) RedirectURI() string { return p.cfg.RedirectBaseURL + "/auth/oidc/callback" }

// Discover returns the provider metadata, cached for an hour (a failed refresh keeps a copy that is
// less than a day old). The answer is validated: the issuer must equal the configured one exactly
// and every endpoint must be https (and public unless allow_insecure_dev).
func (p *Provider) Discover(ctx context.Context) (*Discovery, error) {
	now := p.now()
	p.mu.Lock()
	d, at, try := p.disc, p.discAt, p.discTry
	if d != nil && now.Sub(at) < discoveryTTL {
		p.mu.Unlock()
		return d, nil
	}
	if f := p.discFlight; f != nil { // join the fetch in progress
		p.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.mu.Lock()
		d, at = p.disc, p.discAt
		p.mu.Unlock()
		if f.err != nil && (d == nil || p.now().Sub(at) >= keyMaxStale) {
			return nil, f.err
		}
		return d, nil
	}
	if !try.IsZero() && now.Sub(try) < keyMinRefetch {
		p.mu.Unlock()
		if d != nil && now.Sub(at) < keyMaxStale {
			return d, nil
		}
		return nil, errors.New("discovery is failing; not retried yet")
	}
	f := &flight{done: make(chan struct{})}
	p.discFlight, p.discTry = f, now
	p.mu.Unlock()

	// One fetch is shared by every waiting caller, so it must not die with the first caller's
	// request context: it runs detached, bounded by its own timeout.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout+2*time.Second)
	nd, err := p.fetchDiscovery(fctx)
	cancel()

	p.mu.Lock()
	if err == nil {
		p.disc, p.discAt = nd, now
	}
	p.discFlight = nil
	d, at = p.disc, p.discAt
	p.mu.Unlock()
	f.err = err
	close(f.done)
	if err != nil {
		if d != nil && now.Sub(at) < keyMaxStale {
			return d, nil
		}
		return nil, err
	}
	return nd, nil
}

// FetchDiscovery fetches and validates the metadata without touching the cache (the admin "test
// discovery" button).
func (p *Provider) FetchDiscovery(ctx context.Context) (*Discovery, error) {
	return p.fetchDiscovery(ctx)
}

func (p *Provider) fetchDiscovery(ctx context.Context) (*Discovery, error) {
	var d Discovery
	u := strings.TrimRight(p.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	if err := call(ctx, p.client, http.MethodGet, u, nil, [2]string{}, "", maxDiscBytes, &d); err != nil {
		return nil, err
	}
	if d.Issuer != p.cfg.Issuer {
		return nil, errors.New("discovery issuer does not match the configured issuer")
	}
	for name, v := range map[string]string{"authorization_endpoint": d.AuthorizationEndpoint, "token_endpoint": d.TokenEndpoint, "jwks_uri": d.JWKSURI} {
		if err := config.CheckHTTPSURL(v, p.cfg.AllowInsecureDev, false); err != nil {
			return nil, fmt.Errorf("discovery %s %v", name, err)
		}
	}
	for name, v := range map[string]string{"userinfo_endpoint": d.UserinfoEndpoint, "end_session_endpoint": d.EndSessionEndpoint} {
		if v != "" {
			if err := config.CheckHTTPSURL(v, p.cfg.AllowInsecureDev, false); err != nil {
				return nil, fmt.Errorf("discovery %s %v", name, err)
			}
		}
	}
	if len(d.IDTokenAlgs) > 0 && !contains(d.IDTokenAlgs, AlgRS256) && !contains(d.IDTokenAlgs, AlgES256) {
		return nil, errors.New("provider signs ID tokens with neither RS256 nor ES256")
	}
	if len(d.CodeChallengeMethods) > 0 && !contains(d.CodeChallengeMethods, "S256") {
		return nil, errors.New("provider does not support PKCE S256")
	}
	return &d, nil
}

func (p *Provider) fetchKeys(ctx context.Context) ([]Key, error) {
	d, err := p.Discover(ctx)
	if err != nil {
		return nil, err
	}
	var raw []byte
	if err := call(ctx, p.client, http.MethodGet, d.JWKSURI, nil, [2]string{}, "", maxJWKSBytes, &raw); err != nil {
		return nil, err
	}
	return ParseJWKS(raw)
}

// AuthURL is the authorization request for the browser.
func (p *Provider) AuthURL(ctx context.Context, state, nonce, challenge string, fresh ...bool) (string, error) {
	d, err := p.Discover(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.cfg.ClientID)
	q.Set("redirect_uri", p.RedirectURI())
	q.Set("scope", strings.Join(p.cfg.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	// OpenID Connect makes auth_time a required claim when max_age is requested. A year forces no
	// new sign-in; it is asked so that irreversible actions can tell how fresh a sign-in is.
	q.Set("max_age", "31536000")
	if len(fresh) > 0 && fresh[0] { // step-up: the person authenticates again now
		q.Set("max_age", "0")
		q.Set("prompt", "login")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Tokens is the token endpoint answer.
type Tokens struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// Exchange redeems an authorization code (client_secret_basic, PKCE verifier).
func (p *Provider) Exchange(ctx context.Context, code, verifier string) (*Tokens, error) {
	d, err := p.Discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {p.RedirectURI()}, "code_verifier": {verifier}}
	var t Tokens
	if err := call(ctx, p.client, http.MethodPost, d.TokenEndpoint, form, [2]string{p.cfg.ClientID, p.secret}, "", maxTokenResp, &t); err != nil {
		return nil, err
	}
	if t.IDToken == "" {
		return nil, errors.New("token endpoint returned no id_token")
	}
	if !strings.EqualFold(t.TokenType, "bearer") && t.AccessToken != "" {
		return nil, errors.New("token endpoint returned an unexpected token type")
	}
	return &t, nil
}

// Errors of ID token validation.
var ErrClaims = errors.New("invalid ID token claims")

// VerifyIDToken validates raw as an ID token for this client and returns the identity. wantNonce is
// the nonce of the login that is being completed; accessToken is checked against at_hash when the
// token carries one. userinfo is consulted only to fill in an email or groups the token lacks.
func (p *Provider) VerifyIDToken(ctx context.Context, raw, wantNonce, accessToken string) (*Identity, error) {
	h, payload, signingInput, sig, err := SplitToken(raw)
	if err != nil {
		return nil, err
	}
	key, err := p.keys.Find(ctx, h.Kid, h.Alg)
	if err != nil {
		return nil, err
	}
	if err := VerifySignature(key, h.Alg, signingInput, sig); err != nil {
		return nil, err
	}
	c, err := DecodeClaims(payload)
	if err != nil {
		return nil, err
	}
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrClaims, why) }
	if iss, _ := c["iss"].(string); iss == "" || subtle.ConstantTimeCompare([]byte(iss), []byte(p.cfg.Issuer)) != 1 {
		return nil, bad("iss")
	}
	if !audOK(c, p.cfg.ClientID) {
		return nil, bad("aud")
	}
	now := p.now()
	exp, ok := numDate(c["exp"])
	if !ok || !now.Before(exp.Add(clockSkew)) {
		return nil, bad("exp")
	}
	iat, ok := numDate(c["iat"])
	if !ok || iat.After(now.Add(clockSkew)) || now.Sub(iat) > maxIDTokenAge+clockSkew {
		return nil, bad("iat")
	}
	if v, present := c["nbf"]; present {
		if nbf, ok := numDate(v); !ok || nbf.After(now.Add(clockSkew)) {
			return nil, bad("nbf")
		}
	}
	n, _ := c["nonce"].(string)
	if wantNonce == "" || n == "" || subtle.ConstantTimeCompare([]byte(n), []byte(wantNonce)) != 1 {
		return nil, bad("nonce")
	}
	if v, present := c["at_hash"]; present {
		s, _ := v.(string)
		sum := sha256.Sum256([]byte(accessToken))
		want := base64.RawURLEncoding.EncodeToString(sum[:16])
		if accessToken == "" || s == "" || subtle.ConstantTimeCompare([]byte(s), []byte(want)) != 1 {
			return nil, bad("at_hash")
		}
	}
	id := &Identity{Issuer: p.cfg.Issuer, IssuerHash: IssuerHash(p.cfg.Issuer)}
	sub, ok := c[p.cfg.Claims.Subject].(string)
	if !ok || sub == "" || len(sub) > 255 {
		return nil, bad("subject")
	}
	id.Subject = sub
	p.fillClaims(id, c)
	id.AMR = stringList(c["amr"], 20)
	// Only from the verified ID token, never from userinfo. A time in the future beyond the clock
	// skew is not believed.
	if at, ok := numDate(c["auth_time"]); ok && !at.After(now.Add(clockSkew)) {
		id.AuthTime = at
		if at.After(now) {
			id.AuthTime = now
		}
	}
	if (id.Email == "" || len(id.Groups) == 0 && len(p.cfg.GroupRoles) > 0) && accessToken != "" {
		if d, derr := p.Discover(ctx); derr == nil && d.UserinfoEndpoint != "" {
			var ui map[string]any
			if err := call(ctx, p.client, http.MethodGet, d.UserinfoEndpoint, nil, [2]string{}, accessToken, maxInfoBytes, &ui); err == nil {
				if s, _ := ui["sub"].(string); s == sub { // a userinfo answer for another subject is ignored
					p.fillClaims(id, ui)
				}
			}
		}
	}
	return id, nil
}

// fillClaims copies email, name and groups from claims into id where id has none yet.
func (p *Provider) fillClaims(id *Identity, c map[string]any) {
	if id.Email == "" {
		if e, ok := c[p.cfg.Claims.Email].(string); ok && len(e) <= maxClaimString {
			id.Email = strings.TrimSpace(e)
			id.EmailVerified = truthy(c["email_verified"])
		}
	}
	if id.Name == "" {
		if n, ok := c[p.cfg.Claims.Name].(string); ok && len(n) <= maxClaimString {
			id.Name = n
		}
	}
	if len(id.Groups) == 0 {
		id.Groups = stringList(c[p.cfg.Claims.Groups], maxGroups)
	}
}

func audOK(c map[string]any, clientID string) bool {
	var auds []string
	switch a := c["aud"].(type) {
	case string:
		auds = []string{a}
	case []any:
		for _, x := range a {
			s, ok := x.(string)
			if !ok {
				return false
			}
			auds = append(auds, s)
		}
	default:
		return false
	}
	found := false
	for _, a := range auds {
		found = found || subtle.ConstantTimeCompare([]byte(a), []byte(clientID)) == 1
	}
	if !found {
		return false
	}
	azp, has := c["azp"]
	if has {
		s, _ := azp.(string)
		return subtle.ConstantTimeCompare([]byte(s), []byte(clientID)) == 1
	}
	return len(auds) == 1 // several audiences need an azp naming this client
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true"
	}
	return false
}

func numDate(v any) (time.Time, bool) {
	n, ok := v.(interface{ Float64() (float64, error) })
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f <= 0 || f > 1<<40 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

func stringList(v any, max int) []string {
	var out []string
	switch x := v.(type) {
	case string:
		if x != "" && len(x) <= 256 {
			out = []string{x}
		}
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" && len(s) <= 256 {
				out = append(out, s)
			}
			if len(out) >= max {
				break
			}
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
