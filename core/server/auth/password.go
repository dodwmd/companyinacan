// Package auth implements admin authentication: argon2id passwords, TOTP (RFC 6238), server-side
// sessions with CSRF tokens, and login throttling with lockout.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the argon2id cost parameters.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	KeyLen  uint32
	SaltLen uint32
}

// DefaultParams follows the OWASP minimum for argon2id (m=19 MiB, t=2, p=1), chosen so the
// resident cost of a login stays small on a 2 GB server (SPEC §3.2).
var DefaultParams = Params{Memory: 19 * 1024, Time: 2, Threads: 1, KeyLen: 32, SaltLen: 16}

var currentParams = DefaultParams

// TestingParams lowers the cost for unit tests. It must not be called by production code.
func TestingParams() { currentParams = Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16} }

// MinPasswordLen is the shortest accepted admin password.
const MinPasswordLen = 12

// ErrWeakPassword is returned for passwords shorter than MinPasswordLen.
var ErrWeakPassword = fmt.Errorf("password must be at least %d characters", MinPasswordLen)

var b64 = base64.RawStdEncoding

// HashPassword returns a PHC-format argon2id hash.
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordLen {
		return "", ErrWeakPassword
	}
	p := currentParams
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

type parsed struct {
	Params
	salt, key []byte
}

func parse(h string) (parsed, error) {
	var out parsed
	parts := strings.Split(h, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return out, errors.New("not an argon2id hash")
	}
	var ver int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &ver); err != nil || ver != argon2.Version {
		return out, errors.New("unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &out.Memory, &out.Time, &out.Threads); err != nil {
		return out, errors.New("bad argon2 parameters")
	}
	var err error
	if out.salt, err = b64.DecodeString(parts[4]); err != nil || len(out.salt) == 0 {
		return out, errors.New("bad salt")
	}
	if out.key, err = b64.DecodeString(parts[5]); err != nil || len(out.key) == 0 {
		return out, errors.New("bad key")
	}
	out.KeyLen = uint32(len(out.key))
	return out, nil
}

// VerifyPassword checks password against a PHC hash in constant time. A malformed hash returns an error.
func VerifyPassword(password, hash string) (bool, error) {
	p, err := parse(hash)
	if err != nil {
		return false, err
	}
	if p.Memory > 1<<20 || p.Time > 16 { // refuse absurd parameters from a corrupted row
		return false, errors.New("argon2 parameters out of range")
	}
	got := argon2.IDKey([]byte(password), p.salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return subtle.ConstantTimeCompare(got, p.key) == 1, nil
}

// NeedsRehash reports whether a stored hash is weaker than the current policy.
func NeedsRehash(hash string) bool {
	p, err := parse(hash)
	if err != nil {
		return true
	}
	c := currentParams
	return p.Memory < c.Memory || p.Time < c.Time || p.Threads < c.Threads
}
