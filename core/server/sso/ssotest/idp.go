// Package ssotest is an in-process fake OpenID Connect provider for tests: discovery, JWKS,
// authorization (code + PKCE), token and userinfo endpoints. It is imported by tests only. No real
// identity provider is ever contacted by the test suite.
package ssotest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// User is who signs in at the fake provider.
type User struct {
	Sub           string
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string
	AMR           []string
}

// IdP is the fake provider.
type IdP struct {
	Server       *httptest.Server
	Issuer       string
	ClientID     string
	ClientSecret string

	mu     sync.Mutex
	rsaKey *rsa.PrivateKey
	rsaKid string
	ecKey  *ecdsa.PrivateKey
	ecKid  string
	codes  map[string]*grant
	Now    func() time.Time

	// Hooks and switches for attack tests.
	Mutate       func(claims map[string]any)     // edit the claims before signing
	OverrideID   func(idp *IdP, g Grant) string  // replace the ID token entirely
	JWKSFail     bool                            // JWKS endpoint answers 500
	OmitAtHash   bool                            // do not add at_hash
	WithKeys     func(idp *IdP) []map[string]any // replace the published keys
	RedirectDisc string                          // when set, discovery answers 302 to it
	DiscIssuer   string                          // when set, discovery advertises this issuer
	NoEndSession bool
	UserinfoSub  string // when set, userinfo answers with this subject
	UserinfoDown bool   // userinfo answers 500

	// Counters.
	JWKSHits, TokenHits, DiscHits int
	LastAuthQuery                 url.Values
	TokenRequests                 []url.Values
}

// Grant is an issued authorization code.
type Grant struct {
	User      User
	Nonce     string
	Challenge string
	Redirect  string
	ClientID  string
	Used      bool
	Access    string
}

type grant = Grant

// New starts a fake provider. cfgIssuerSuffix lets the issuer carry a path (like /realms/acme).
func New(t testing.TB, path string) *IdP {
	t.Helper()
	i := &IdP{ClientID: "can-admin", ClientSecret: "s3cret-value", codes: map[string]*grant{}, Now: time.Now}
	i.rsaKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	i.rsaKid = "rsa-1"
	i.ecKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	i.ecKid = "ec-1"
	mux := http.NewServeMux()
	mux.HandleFunc(path+"/.well-known/openid-configuration", i.discovery)
	mux.HandleFunc(path+"/authorize", i.authorize)
	mux.HandleFunc(path+"/token", i.token)
	mux.HandleFunc(path+"/jwks", i.jwks)
	mux.HandleFunc(path+"/userinfo", i.userinfo)
	i.Server = httptest.NewServer(mux)
	i.Issuer = i.Server.URL + path
	t.Cleanup(i.Server.Close)
	return i
}

// RotateKey replaces the RSA signing key with a new one under a new kid (the old key disappears).
func (i *IdP) RotateKey(kid string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.rsaKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	i.rsaKid = kid
}

// PublicKeyPEM is the RSA public key in PEM, for HS256 confusion attacks.
func (i *IdP) PublicKeyPEM() []byte {
	b, _ := x509.MarshalPKIXPublicKey(&i.rsaKey.PublicKey)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b})
}

func (i *IdP) discovery(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	i.DiscHits++
	redir, di := i.RedirectDisc, i.DiscIssuer
	i.mu.Unlock()
	if redir != "" {
		http.Redirect(w, r, redir, http.StatusFound)
		return
	}
	iss := i.Issuer
	if di != "" {
		iss = di
	}
	doc := map[string]any{
		"issuer": iss, "authorization_endpoint": i.Issuer + "/authorize", "token_endpoint": i.Issuer + "/token",
		"jwks_uri": i.Issuer + "/jwks", "userinfo_endpoint": i.Issuer + "/userinfo",
		"id_token_signing_alg_values_supported": []string{"RS256", "ES256"}, "code_challenge_methods_supported": []string{"S256"},
	}
	if !i.NoEndSession {
		doc["end_session_endpoint"] = i.Issuer + "/logout"
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (i *IdP) jwks(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	i.JWKSHits++
	fail, with := i.JWKSFail, i.WithKeys
	i.mu.Unlock()
	if fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	keys := []map[string]any{i.rsaJWK(), i.ecJWK()}
	if with != nil {
		keys = with(i)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

func (i *IdP) rsaJWK() map[string]any {
	i.mu.Lock()
	defer i.mu.Unlock()
	return map[string]any{"kty": "RSA", "kid": i.rsaKid, "use": "sig", "alg": "RS256",
		"n": b64(i.rsaKey.N.Bytes()), "e": b64(big.NewInt(int64(i.rsaKey.E)).Bytes())}
}

// ecPoint is the uncompressed public point (0x04 || X || Y).
func (i *IdP) ecPoint() []byte {
	b, _ := i.ecKey.PublicKey.Bytes()
	return b
}

func (i *IdP) ecJWK() map[string]any {
	return map[string]any{"kty": "EC", "kid": i.ecKid, "use": "sig", "alg": "ES256", "crv": "P-256",
		"x": b64(i.ecPoint()[1:33]), "y": b64(i.ecPoint()[33:65])}
}

// Approve plays the user's browser at the authorization endpoint: it issues a code for authURL (the
// URL can-core redirected to) and returns the callback URL the provider would redirect back to.
func (i *IdP) Approve(authURL string, u User) string {
	pu, _ := url.Parse(authURL)
	q := pu.Query()
	i.mu.Lock()
	i.LastAuthQuery = q
	code := randStr()
	i.codes[code] = &grant{User: u, Nonce: q.Get("nonce"), Challenge: q.Get("code_challenge"), Redirect: q.Get("redirect_uri"), ClientID: q.Get("client_id"), Access: randStr()}
	i.mu.Unlock()
	cb, _ := url.Parse(q.Get("redirect_uri"))
	cq := cb.Query()
	cq.Set("code", code)
	cq.Set("state", q.Get("state"))
	cb.RawQuery = cq.Encode()
	return cb.String()
}

func (i *IdP) authorize(w http.ResponseWriter, r *http.Request) { http.Error(w, "use Approve", 400) }

func randStr() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return b64(b)
}

func (i *IdP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	i.mu.Lock()
	i.TokenHits++
	i.TokenRequests = append(i.TokenRequests, r.PostForm)
	i.mu.Unlock()
	id, sec, ok := r.BasicAuth()
	id, _ = url.QueryUnescape(id)
	sec, _ = url.QueryUnescape(sec)
	if !ok || id != i.ClientID || sec != i.ClientSecret || r.PostFormValue("grant_type") != "authorization_code" {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	i.mu.Lock()
	g := i.codes[r.PostFormValue("code")]
	if g == nil || g.Used || g.Redirect != r.PostFormValue("redirect_uri") {
		i.mu.Unlock()
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	g.Used = true
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	if b64(sum[:]) != g.Challenge {
		i.mu.Unlock()
		http.Error(w, `{"error":"invalid_grant","error_description":"pkce"}`, http.StatusBadRequest)
		return
	}
	gg := *g
	i.mu.Unlock()
	var idt string
	if i.OverrideID != nil {
		idt = i.OverrideID(i, gg)
	} else {
		idt = i.IDToken(gg, "RS256")
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id_token": idt, "access_token": gg.Access, "token_type": "Bearer", "expires_in": 300})
}

func (i *IdP) userinfo(w http.ResponseWriter, r *http.Request) {
	if i.UserinfoDown {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, g := range i.codes {
		if g.Access == tok && tok != "" {
			sub := g.User.Sub
			if i.UserinfoSub != "" {
				sub = i.UserinfoSub
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sub": sub, "email": g.User.Email, "email_verified": g.User.EmailVerified, "name": g.User.Name, "groups": g.User.Groups})
			return
		}
	}
	http.Error(w, "no", http.StatusUnauthorized)
}

// Claims is the standard claim set for a grant.
func (i *IdP) Claims(g Grant) map[string]any {
	now := i.Now()
	c := map[string]any{
		"iss": i.Issuer, "aud": i.ClientID, "sub": g.User.Sub, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"nonce": g.Nonce, "email": g.User.Email, "email_verified": g.User.EmailVerified, "name": g.User.Name,
	}
	if g.User.Groups != nil {
		c["groups"] = g.User.Groups
	}
	if g.User.AMR != nil {
		c["amr"] = g.User.AMR
	}
	if !i.OmitAtHash {
		sum := sha256.Sum256([]byte(g.Access))
		c["at_hash"] = b64(sum[:16])
	}
	if i.Mutate != nil {
		i.Mutate(c)
	}
	return c
}

// IDToken builds a signed ID token for the grant with the named algorithm: RS256, ES256, none, or
// HS256 (keyed with the RSA public key PEM, the classic confusion attack).
func (i *IdP) IDToken(g Grant, alg string) string {
	return i.Sign(alg, nil, i.Claims(g))
}

// Sign signs claims. header entries override the defaults.
func (i *IdP) Sign(alg string, header map[string]any, claims map[string]any) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	h := map[string]any{"alg": alg, "typ": "JWT"}
	switch alg {
	case "RS256":
		h["kid"] = i.rsaKid
	case "ES256":
		h["kid"] = i.ecKid
	}
	for k, v := range header {
		if v == nil {
			delete(h, k)
		} else {
			h[k] = v
		}
	}
	hb, _ := json.Marshal(h)
	cb, _ := json.Marshal(claims)
	in := b64(hb) + "." + b64(cb)
	var sig []byte
	switch alg {
	case "RS256":
		sum := sha256.Sum256([]byte(in))
		sig, _ = rsa.SignPKCS1v15(rand.Reader, i.rsaKey, crypto.SHA256, sum[:])
	case "ES256":
		sum := sha256.Sum256([]byte(in))
		r, s, _ := ecdsa.Sign(rand.Reader, i.ecKey, sum[:])
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	case "HS256":
		b, _ := x509.MarshalPKIXPublicKey(&i.rsaKey.PublicKey)
		key := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b})
		m := hmac.New(sha256.New, key)
		m.Write([]byte(in))
		sig = m.Sum(nil)
	case "none":
		sig = nil
	default:
		panic(fmt.Sprintf("ssotest: unknown alg %q", alg))
	}
	return in + "." + b64(sig)
}

// SignWithForeignKey signs RS256 with a key the provider never published.
func (i *IdP) SignWithForeignKey(claims map[string]any) string {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	h := map[string]any{"alg": "RS256", "typ": "JWT", "kid": i.rsaKid}
	hb, _ := json.Marshal(h)
	cb, _ := json.Marshal(claims)
	in := b64(hb) + "." + b64(cb)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	return in + "." + b64(sig)
}
