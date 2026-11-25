package forms

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Timing-token errors.
var (
	ErrTokenInvalid = errors.New("form token invalid")
	ErrTokenExpired = errors.New("form token expired")
	ErrTooFast      = errors.New("form submitted too quickly")
)

// TokenSigner issues and checks signed timestamp tokens. A token proves when the form
// was rendered, so a submission that arrives faster than a human could fill it is rejected.
// It is stateless: nothing is stored per token.
type TokenSigner struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// NewTokenSigner builds a signer. now may be nil (time.Now).
func NewTokenSigner(key []byte, ttl time.Duration, now func() time.Time) *TokenSigner {
	if now == nil {
		now = time.Now
	}
	return &TokenSigner{key: key, ttl: ttl, now: now}
}

func (s *TokenSigner) mac(formID, ts string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(formID))
	m.Write([]byte{0})
	m.Write([]byte(ts))
	return m.Sum(nil)
}

// Issue returns a token bound to the form id and the current time.
func (s *TokenSigner) Issue(formID string) string {
	ts := strconv.FormatInt(s.now().Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(ts)) + "." + base64.RawURLEncoding.EncodeToString(s.mac(formID, ts))
}

// Check verifies signature, form binding, age (<= ttl) and minimum fill time.
func (s *TokenSigner) Check(formID, token string, minFill time.Duration) error {
	a, b, ok := strings.Cut(token, ".")
	if !ok {
		return ErrTokenInvalid
	}
	tsRaw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil {
		return ErrTokenInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		return ErrTokenInvalid
	}
	if !hmac.Equal(sig, s.mac(formID, string(tsRaw))) {
		return ErrTokenInvalid
	}
	sec, err := strconv.ParseInt(string(tsRaw), 10, 64)
	if err != nil {
		return ErrTokenInvalid
	}
	age := s.now().Sub(time.Unix(sec, 0))
	switch {
	case age < 0:
		return ErrTokenInvalid
	case age > s.ttl:
		return ErrTokenExpired
	case age < minFill:
		return ErrTooFast
	}
	return nil
}

// RateLimiter is a per-key token bucket with a hard bound on tracked keys, so memory
// stays constant under a flood of distinct addresses.
type RateLimiter struct {
	mu      sync.Mutex
	perMin  float64
	burst   float64
	maxKeys int
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows perMinute requests per key (also the burst size) and tracks at
// most maxKeys keys. now may be nil.
func NewRateLimiter(perMinute, maxKeys int, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{perMin: float64(perMinute), burst: float64(perMinute), maxKeys: maxKeys, now: now, buckets: make(map[string]*bucket)}
}

// Allow consumes one token for key. When it refuses it returns how long to wait.
func (r *RateLimiter) Allow(key string) (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.buckets[key]
	if !ok {
		if len(r.buckets) >= r.maxKeys {
			r.evict(now)
		}
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[key] = b
	}
	b.tokens = min(r.burst, b.tokens+now.Sub(b.last).Minutes()*r.perMin)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / r.perMin * float64(time.Minute))
	return false, wait.Round(time.Second) + time.Second
}

// evict drops buckets that have refilled completely (they carry no information); if none
// qualify it drops arbitrary entries until there is room.
func (r *RateLimiter) evict(now time.Time) {
	for k, b := range r.buckets {
		if b.tokens+now.Sub(b.last).Minutes()*r.perMin >= r.burst {
			delete(r.buckets, k)
		}
	}
	for k := range r.buckets {
		if len(r.buckets) < r.maxKeys {
			break
		}
		delete(r.buckets, k)
	}
}

// Len reports the number of tracked keys.
func (r *RateLimiter) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buckets)
}

// CaptchaVerifier checks a challenge response from the client. Implementations must treat
// a rejected response as (false, nil) and reserve errors for failures to ask.
type CaptchaVerifier interface {
	Verify(ctx context.Context, response, remoteIP string) (bool, error)
}

// Verification endpoints.
const (
	TurnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	HCaptchaVerifyURL  = "https://api.hcaptcha.com/siteverify"
)

// SiteVerify speaks the siteverify protocol shared by Turnstile and hCaptcha.
type SiteVerify struct {
	URL    string
	Secret string
	Client *http.Client
}

// Verify implements CaptchaVerifier.
func (v *SiteVerify) Verify(ctx context.Context, response, remoteIP string) (bool, error) {
	if response == "" || len(response) > 4096 {
		return false, nil
	}
	form := url.Values{"secret": {v.Secret}, "response": {response}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("captcha verify: upstream status %d", resp.StatusCode)
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return false, fmt.Errorf("captcha verify: %w", err)
	}
	return out.Success, nil
}

// NewCaptchaVerifier returns the verifier for a provider, or nil for "none".
func NewCaptchaVerifier(provider, secret string, client *http.Client) (CaptchaVerifier, error) {
	switch provider {
	case "", "none":
		return nil, nil
	case "turnstile":
		if secret == "" {
			return nil, errors.New("captcha secret is required for turnstile")
		}
		return &SiteVerify{URL: TurnstileVerifyURL, Secret: secret, Client: client}, nil
	case "hcaptcha":
		if secret == "" {
			return nil, errors.New("captcha secret is required for hcaptcha")
		}
		return &SiteVerify{URL: HCaptchaVerifyURL, Secret: secret, Client: client}, nil
	}
	return nil, fmt.Errorf("unknown captcha provider %q", provider)
}

// CaptchaResponseField is the form field the provider's widget posts its token in.
func CaptchaResponseField(provider string) string {
	switch provider {
	case "turnstile":
		return "cf-turnstile-response"
	case "hcaptcha":
		return "h-captcha-response"
	}
	return ""
}
