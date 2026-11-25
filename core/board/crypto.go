package board

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

var keyIDRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Keyring holds the field-encryption keys. The first key is active (used to seal); the rest
// are retired keys kept so older ciphertext still opens until it is re-sealed (rotation).
type Keyring struct {
	active string
	keys   map[string][]byte
}

// ParseKeyring parses lines of `<key-id> <64 hex chars>`; blank lines and #-comments are
// ignored and the first entry is the active key.
func ParseKeyring(s string) (*Keyring, error) {
	kr := &Keyring{keys: map[string][]byte{}}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, hx, ok := strings.Cut(line, " ")
		hx = strings.TrimSpace(hx)
		if !ok || !keyIDRe.MatchString(id) {
			return nil, errors.New("keyring: each line must be `<key-id> <hex key>` with an id of [a-z0-9-]")
		}
		if _, dup := kr.keys[id]; dup {
			return nil, fmt.Errorf("keyring: duplicate key id %q", id)
		}
		k, err := hex.DecodeString(hx)
		if err != nil || len(k) != chacha20poly1305.KeySize {
			return nil, fmt.Errorf("keyring: key %q must be %d bytes of hex", id, chacha20poly1305.KeySize)
		}
		if kr.active == "" {
			kr.active = id
		}
		kr.keys[id] = k
	}
	if kr.active == "" {
		return nil, errors.New("keyring: no keys")
	}
	return kr, nil
}

// LoadKeyring reads a keyring file (a mounted Secret). It refuses files readable by group or
// others so a misconfigured mount fails closed.
func LoadKeyring(path string) (*Keyring, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("keyring: %w", err)
	}
	// A symlink could point the service at a file the operator never meant to trust (and a
	// Secret volume's ..data links must not be used: mount the key with subPath).
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("keyring: %s must be a regular file, not a symlink (mount the Secret key with subPath)", path)
	}
	if err := checkOwner(path, st); err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("keyring: %s must not be accessible by group or others (mode %o)", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path) //nolint:gosec // operator-configured secret path
	if err != nil {
		return nil, fmt.Errorf("keyring: %w", err)
	}
	return ParseKeyring(string(b))
}

// ActiveID is the id of the key new data is sealed with.
func (k *Keyring) ActiveID() string { return k.active }

// Seal encrypts plain with XChaCha20-Poly1305. aad binds the ciphertext to its row and
// column, so a value copied to another person or field fails to open. Output:
// v1.<key-id>.<base64url(nonce||ciphertext)>.
func (k *Keyring) Seal(plain, aad string) (string, error) {
	aead, err := chacha20poly1305.NewX(k.keys[k.active])
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return "v1." + k.active + "." + base64.RawURLEncoding.EncodeToString(ct), nil
}

// Open decrypts a Seal output. Any failure returns one generic error.
func (k *Keyring) Open(sealed, aad string) (string, error) {
	errBad := errors.New("cannot decrypt field")
	parts := strings.Split(sealed, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", errBad
	}
	key, ok := k.keys[parts[1]]
	if !ok {
		return "", errBad
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[2])
	aead, aerr := chacha20poly1305.NewX(key)
	if err != nil || aerr != nil || len(raw) < aead.NonceSize()+aead.Overhead() {
		return "", errBad
	}
	pt, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(aad))
	if err != nil {
		return "", errBad
	}
	return string(pt), nil
}

// NeedsRotation reports whether sealed was written with a key other than the active one.
func (k *Keyring) NeedsRotation(sealed string) bool {
	parts := strings.Split(sealed, ".")
	return len(parts) != 3 || parts[1] != k.active
}
