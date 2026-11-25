// Package webpush is a minimal Web Push sender (RFC 8030 delivery, RFC 8291 aes128gcm payload
// encryption, RFC 8292 VAPID) built on the standard library only. It knows nothing about can-core's
// notifications: it encrypts a payload for one browser subscription and posts it to the push
// service the browser named, through a client that refuses non-public destinations and redirects.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/dodwmd/companyinacan/core/server/netguard"
)

// Limits.
const (
	// MaxPayload is the largest plaintext one record carries: the 4096-byte record (RFC 8291
	// recommends push services accept at least 4096 octets) minus the 86-byte header and the
	// 17 bytes of padding delimiter and AEAD tag. Notifications here are far smaller.
	MaxPayload   = 3993
	recordSize   = 4096
	MaxEndpoint  = 500
	maxResponse  = 4 << 10
	maxRetryWait = 60 * time.Second
)

// Urgency values of RFC 8030 section 5.3.
const (
	UrgencyVeryLow = "very-low"
	UrgencyLow     = "low"
	UrgencyNormal  = "normal"
	UrgencyHigh    = "high"
)

var b64 = base64.RawURLEncoding

// ErrGone means the push service says the subscription no longer exists (404 or 410): the caller
// must delete it.
var ErrGone = errors.New("push subscription is gone")

// --- keys and VAPID ------------------------------------------------------------------------------

// Key is the application server's VAPID key pair (ECDSA P-256).
type Key struct{ priv *ecdsa.PrivateKey }

// GenerateKey creates a new VAPID key pair.
func GenerateKey() (*Key, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Key{k}, nil
}

// ParseKey reads the base64url form written by Marshal (the 32-byte private scalar).
func ParseKey(s string) (*Key, error) {
	raw, err := b64.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("VAPID private key must be 32 bytes, base64url")
	}
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, errors.New("VAPID private key is not a valid P-256 scalar")
	}
	return &Key{k}, nil
}

// Marshal returns the private scalar as base64url. It is a secret.
func (k *Key) Marshal() string {
	raw, _ := k.priv.Bytes()
	return b64.EncodeToString(raw)
}

// PublicKey returns the uncompressed public point, base64url: the applicationServerKey the browser
// subscribes with and the `k` of the VAPID header.
func (k *Key) PublicKey() string {
	e, _ := k.priv.ECDH()
	return b64.EncodeToString(e.PublicKey().Bytes())
}

// authorization builds the RFC 8292 `vapid t=<jwt>, k=<key>` header value for a push service.
func (k *Key) authorization(endpoint, contact string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("bad endpoint")
	}
	hdr := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now.Add(12 * time.Hour).Unix(), "sub": contact})
	signing := hdr + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	der, err := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", err
	}
	var sig struct{ R, S *big.Int }
	if _, err := asn1.Unmarshal(der, &sig); err != nil {
		return "", err
	}
	raw := make([]byte, 64) // JWS ES256 signatures are the fixed-width r||s, not DER
	sig.R.FillBytes(raw[:32])
	sig.S.FillBytes(raw[32:])
	return "vapid t=" + signing + "." + b64.EncodeToString(raw) + ", k=" + k.PublicKey(), nil
}

// --- payload encryption (RFC 8291) ---------------------------------------------------------------

// Encrypt encrypts plaintext for the subscription keys (the browser's p256dh point and 16-byte auth
// secret) into one aes128gcm record with a fresh salt and ephemeral key.
func Encrypt(plaintext, uaPublic, authSecret []byte) ([]byte, error) {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encrypt(plaintext, uaPublic, authSecret, as, salt)
}

func encrypt(plaintext, uaPublic, authSecret []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("payload of %d bytes exceeds %d", len(plaintext), MaxPayload)
	}
	if len(authSecret) != 16 {
		return nil, errors.New("auth secret must be 16 bytes")
	}
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, errors.New("invalid user-agent public key")
	}
	secret, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPub := as.PublicKey().Bytes()
	// IKM = HKDF(salt=auth_secret, ikm=ecdh_secret, info="WebPush: info\0" || ua_public || as_public)
	info := append(append([]byte("WebPush: info\x00"), uaPublic...), asPub...)
	ikm, err := hkdf.Key(sha256.New, secret, authSecret, string(info), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	// One record: plaintext || 0x02 (last-record delimiter), no further padding.
	rec := gcm.Seal(nil, nonce, append(append([]byte(nil), plaintext...), 0x02), nil)
	// Header: salt(16) || record size (4, big endian) || key id length (1) || key id (as_public).
	out := make([]byte, 0, 21+len(asPub)+len(rec))
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPub)))
	out = append(out, asPub...)
	return append(out, rec...), nil
}

// --- subscriptions -------------------------------------------------------------------------------

// Subscription is what the browser's PushSubscription.toJSON() gives: the push service endpoint and
// the two keys that encrypt payloads for that browser.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

// Validate checks the subscription before it is stored. The endpoint is attacker-supplied (any
// signed-in user posts it) and can-core will connect to it, so it must be an https URL on port 443
// naming a public DNS host: no address literals, userinfo, fragment, single-label or internal names.
// The connection itself is checked again at connect time (see NewClient). allowInsecure skips the
// endpoint rules (tests only).
func (s Subscription) Validate(allowInsecure bool) error { return s.ValidateWith(allowInsecure, nil) }

// ValidateWith is Validate plus an optional host-suffix allowlist (see HostAllowed); an empty list
// allows any public host.
func (s Subscription) ValidateWith(allowInsecure bool, allow []string) error {
	if !allowInsecure {
		if err := validEndpoint(s.Endpoint); err != nil {
			return err
		}
		if !HostAllowed(s.Endpoint, allow) {
			return errors.New("endpoint host is not on this server's push service allowlist")
		}
	} else if s.Endpoint == "" || len(s.Endpoint) > MaxEndpoint {
		return errors.New("endpoint missing or too long")
	}
	pub, err := b64.DecodeString(s.P256dh)
	if err != nil {
		return errors.New("p256dh is not base64url")
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return errors.New("p256dh is not a P-256 public key")
	}
	if a, err := b64.DecodeString(s.Auth); err != nil || len(a) != 16 {
		return errors.New("auth must be 16 bytes, base64url")
	}
	return nil
}

// HostAllowed reports whether the endpoint's host equals or is a subdomain of one of the allowed
// suffixes (for example "googleapis.com" allows fcm.googleapis.com). An empty list allows every host.
func HostAllowed(ep string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	u, err := url.Parse(ep)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	for _, a := range allow {
		a = strings.ToLower(strings.Trim(strings.TrimSpace(a), "."))
		if a != "" && (host == a || strings.HasSuffix(host, "."+a)) {
			return true
		}
	}
	return false
}

func validEndpoint(ep string) error {
	if ep == "" || len(ep) > MaxEndpoint || strings.IndexFunc(ep, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) || r > unicode.MaxASCII }) >= 0 {
		return errors.New("endpoint missing, too long or has invalid characters")
	}
	u, err := url.Parse(ep)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return errors.New("endpoint must be a plain https URL")
	}
	if p := u.Port(); p != "" && p != "443" {
		return errors.New("endpoint must use port 443")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if _, err := netip.ParseAddr(host); err == nil || strings.ContainsAny(host, ":[]") {
		return errors.New("endpoint must name a host, not an address")
	}
	labels := strings.Split(host, ".")
	last := labels[len(labels)-1]
	if len(labels) < 2 || strings.IndexFunc(last, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0 {
		return errors.New("endpoint must name a public DNS host")
	}
	for _, bad := range []string{"localhost", "local", "internal", "intranet", "lan", "home", "corp", "svc", "cluster"} {
		if last == bad || strings.HasSuffix(host, "."+bad+".") || strings.Contains(host, "."+bad+".") {
			return errors.New("endpoint must name a public DNS host")
		}
	}
	return nil
}

// --- sending (RFC 8030) --------------------------------------------------------------------------

// Options are the per-message delivery hints.
type Options struct {
	// TTL is how long the push service keeps the message for an offline device, in seconds.
	TTL     int
	Urgency string
	// Topic replaces an undelivered message with the same topic (1 to 32 base64url characters).
	Topic string
}

// Result is the push service's answer.
type Result struct {
	Status int
	// Retryable is set for network errors, 429 and 5xx.
	Retryable  bool
	RetryAfter time.Duration
}

// Client posts encrypted messages to push services.
type Client struct {
	Key *Key
	// Contact is the VAPID `sub` claim: a mailto: or https: URL the push service can reach the
	// operator on.
	Contact string
	HTTP    *http.Client
	Now     func() time.Time
	// AllowHosts is the optional host-suffix allowlist enforced on every Send; AllowInsecure skips
	// the endpoint rules (tests with a loopback fake only).
	AllowHosts    []string
	AllowInsecure bool
}

// NewClient returns a Client whose HTTP client never follows redirects, connects only to public
// addresses (judged after DNS, at connect time), ignores proxy environment variables and bounds
// every exchange.
func NewClient(key *Key, contact string) *Client {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, Control: netguard.DenyPrivate}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &Client{Key: key, Contact: contact, Now: time.Now, HTTP: &http.Client{
		Transport:     tr,
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Send encrypts payload for sub and posts it. A 404 or 410 returns ErrGone. Errors never contain
// the endpoint URL (it is the subscription's capability).
func (c *Client) Send(ctx context.Context, sub Subscription, payload []byte, o Options) (Result, error) {
	// Validated again here, not only when the subscription was stored: the row may predate a stricter
	// rule or an allowlist, and nothing but this check stands between a stored URL and a connection.
	if err := sub.ValidateWith(c.AllowInsecure, c.AllowHosts); err != nil {
		return Result{}, errors.New("subscription refused: " + err.Error())
	}
	p256dh, err1 := b64.DecodeString(sub.P256dh)
	auth, err2 := b64.DecodeString(sub.Auth)
	if err1 != nil || err2 != nil {
		return Result{}, errors.New("subscription keys are not base64url")
	}
	body, err := Encrypt(payload, p256dh, auth)
	if err != nil {
		return Result{}, err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	authz, err := c.Key.authorization(sub.Endpoint, c.Contact, now())
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, errors.New("push request could not be built")
	}
	ttl := o.TTL
	if ttl <= 0 {
		ttl = 3600
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(ttl))
	switch o.Urgency {
	case UrgencyVeryLow, UrgencyLow, UrgencyNormal, UrgencyHigh:
		req.Header.Set("Urgency", o.Urgency)
	}
	if validTopic(o.Topic) {
		req.Header.Set("Topic", o.Topic)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Result{Retryable: true}, fmt.Errorf("push request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	res := Result{Status: resp.StatusCode}
	switch {
	case resp.StatusCode/100 == 2:
		return res, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return res, ErrGone
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5:
		res.Retryable = true
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			res.RetryAfter = min(time.Duration(s)*time.Second, maxRetryWait)
		}
	}
	return res, fmt.Errorf("push service answered %d", resp.StatusCode)
}

func validTopic(t string) bool {
	if t == "" || len(t) > 32 {
		return false
	}
	for _, r := range t {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
