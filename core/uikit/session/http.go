package session

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
)

// NewToken is a 256-bit random value in URL-safe base64 (43 characters): session ids and CSRF tokens.
func NewToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:]) // never fails since Go 1.24
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Equal compares a presented secret with the expected one in constant time. Nothing equals "".
func Equal(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// SameOrigin reports whether a request that changes something may come from the UI's own pages:
// Sec-Fetch-Site, when present, must be same-origin (or none), and an Origin header, when present,
// must name this host. "Origin: null" is what a browser sends for a form posted from a page with
// Referrer-Policy: no-referrer (all of these pages), so it is accepted when, and only when, the
// browser-controlled Sec-Fetch-Site says the post is same-origin. A client that sends neither
// header is not a browser form post from elsewhere; the CSRF token still applies to it.
func SameOrigin(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	if site != "" && site != "same-origin" && site != "none" {
		return false
	}
	switch o := r.Header.Get("Origin"); o {
	case "":
		return true
	case "null":
		return site == "same-origin"
	default:
		u, err := url.Parse(o)
		return err == nil && strings.EqualFold(u.Host, r.Host)
	}
}

// ContentSecurityPolicy of every module UI page: there is no script of any kind, so scripts are
// forbidden outright; styles, images, the manifest and forms are same-origin only.
const ContentSecurityPolicy = "default-src 'none'; script-src 'none'; style-src 'self'; img-src 'self'; manifest-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// SetHeaders applies the security headers of every UI response. Cache-Control is no-store; the
// handler of a static asset or a download replaces what it needs afterwards.
func SetHeaders(h http.Header) {
	h.Set("Content-Security-Policy", ContentSecurityPolicy)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
}
