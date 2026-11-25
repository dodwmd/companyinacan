package sso

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// StateCookiePrefix names the short-lived cookies that carry a login transaction (state, nonce, PKCE
// verifier, where to go next) from /auth/oidc/login to /auth/oidc/callback. Each login has its own
// cookie, can_oidc_<flow id>, where the flow id is the first FlowIDLen characters of its state, so
// two tabs signing in at once do not overwrite each other. The browser's set is bounded by the
// handler (see MaxFlowCookies) and each cookie lives StateTTL at most.
const StateCookiePrefix = "can_oidc_"

// FlowIDLen is the length of the flow id at the start of a state value.
const FlowIDLen = 8

// MaxFlowCookies is how many transaction cookies a browser may hold; starting another login beyond
// it expires the older ones.
const MaxFlowCookies = 4

// FlowCookieName returns the cookie name for a state value, or false if the state is malformed.
func FlowCookieName(state string) (string, bool) {
	if len(state) < FlowIDLen+16 || len(state) > 128 {
		return "", false
	}
	for _, c := range state[:FlowIDLen] {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return "", false
		}
	}
	return StateCookiePrefix + state[:FlowIDLen], true
}

// StateTTL bounds how long a login may take at the identity provider.
const StateTTL = 10 * time.Minute

type stateData struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x,omitempty"`
	Exp      int64  `json:"e"`
}

// errState is every way the state cookie can be wrong; callers must not distinguish them.
var errState = errors.New("invalid login state")

func randString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// pkceChallenge is the S256 code challenge of a verifier.
func pkceChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func sign(key []byte, payload string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("can-oidc-state|" + payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// seal encodes and signs sd. The cookie is HMAC-signed (not encrypted): it holds nothing the
// browser that owns it may not see, and tampering is detected.
func seal(key []byte, sd stateData) string {
	b, _ := json.Marshal(sd)
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + sign(key, p)
}

func open(key []byte, v string, now time.Time) (stateData, error) {
	var sd stateData
	p, mac, ok := strings.Cut(v, ".")
	if !ok || len(v) > 2048 {
		return sd, errState
	}
	if !hmac.Equal([]byte(mac), []byte(sign(key, p))) {
		return sd, errState
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || json.Unmarshal(b, &sd) != nil {
		return sd, errState
	}
	if now.Unix() > sd.Exp || sd.State == "" || sd.Nonce == "" || sd.Verifier == "" {
		return sd, errState
	}
	return sd, nil
}

// StateCookie builds the transaction cookie. SameSite=Lax (not Strict): the callback is a
// cross-site top-level navigation from the identity provider and must carry it. Path-scoped to the
// OIDC routes and HttpOnly.
func StateCookie(name, value string, secure bool) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/auth/oidc", MaxAge: int(StateTTL.Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode}
}

// ClearStateCookie expires the transaction cookie.
func ClearStateCookie(name string, secure bool) *http.Cookie {
	return &http.Cookie{Name: name, Value: "", Path: "/auth/oidc", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode}
}

// SafeNext reduces a post-login destination to a local path: anything that could leave this site
// (absolute URLs, //host, backslashes, control characters) becomes "/".
func SafeNext(next string) string {
	if next == "" || len(next) > 200 || next[0] != '/' || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n\t\x00") {
		return "/"
	}
	for _, r := range next {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	if strings.HasPrefix(u.Path, "/auth/oidc") || u.Path == "/login" {
		return "/"
	}
	return next
}

func constEq(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
