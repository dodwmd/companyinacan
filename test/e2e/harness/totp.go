// Package harness is the helper layer of the end-to-end suite (specs/032-e2e-docs): an HTTP
// client that reaches the compose edge by virtual host without DNS, an RFC 6238 TOTP generator
// for the admin login, and a few JSON conveniences. It depends on the standard library only.
package harness

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP is defined over HMAC-SHA1
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var secretLine = regexp.MustCompile(`(?i)TOTP secret[^:]*:\s*([A-Za-z2-7]{16,})`)

// ParseTOTPSecret finds the base32 secret in the output of `can-core admin bootstrap` or
// `admin reset-password --rotate-totp` ("New TOTP secret (shown once): ..."), or "" if absent.
func ParseTOTPSecret(out string) string {
	if m := secretLine.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// TOTP returns the 6-digit RFC 6238 code (SHA-1, 30 second step) for a base32 secret at time t.
func TOTP(secret string, t time.Time) (string, error) {
	s := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(secret))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("totp secret is not base32: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(t.Unix()/30)) //nolint:gosec // time is positive
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1_000_000
	return fmt.Sprintf("%06d", code), nil
}
