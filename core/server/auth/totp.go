package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 default; authenticator apps use HMAC-SHA1
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"hash"
	"net/url"
	"strings"
	"time"
)

// Algorithm is the HMAC hash used for TOTP.
type Algorithm string

// Supported algorithms (RFC 6238 §1.2). Authenticator apps use SHA1.
const (
	SHA1   Algorithm = "SHA1"
	SHA256 Algorithm = "SHA256"
	SHA512 Algorithm = "SHA512"
)

const (
	totpDigits = 6
	totpPeriod = 30
	totpSkew   = 1 // accept one step either side for clock drift
)

func (a Algorithm) hasher() func() hash.Hash {
	switch a {
	case SHA256:
		return sha256.New
	case SHA512:
		return sha512.New
	default:
		return sha1.New
	}
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh 160-bit base32 secret.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return b32.EncodeToString(b)
}

// DecodeTOTPSecret decodes a base32 secret (case and spaces tolerated).
func DecodeTOTPSecret(s string) ([]byte, error) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "=", "").Replace(s))
	return b32.DecodeString(s)
}

// Code computes the HOTP/TOTP value (RFC 4226 §5.3 with the RFC 6238 time counter) for the time
// step containing t.
func Code(key []byte, alg Algorithm, digits, period int, t time.Time) string {
	return hotp(key, alg, digits, uint64(t.Unix()/int64(period)))
}

func hotp(key []byte, alg Algorithm, digits int, counter uint64) string {
	mac := hmac.New(alg.hasher(), key)
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], counter)
	mac.Write(c[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// ValidateTOTP checks a 6-digit code against the secret around now and returns the matching time
// step, which the caller must persist to make codes single-use.
func ValidateTOTP(secret, code string, now time.Time) (int64, bool) {
	if len(code) != totpDigits {
		return 0, false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	key, err := DecodeTOTPSecret(secret)
	if err != nil {
		return 0, false
	}
	step := now.Unix() / totpPeriod
	match, found := int64(0), false
	for d := int64(-totpSkew); d <= totpSkew; d++ { // no early exit: constant work
		want := hotp(key, SHA1, totpDigits, uint64(step+d))
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			match, found = step+d, true
		}
	}
	return match, found
}

// TOTPURI builds the otpauth:// provisioning URI shown once at bootstrap.
func TOTPURI(issuer, account, secret string) string {
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + url.PathEscape(issuer) + ":" + url.PathEscape(account) + "?" + q.Encode()
}
