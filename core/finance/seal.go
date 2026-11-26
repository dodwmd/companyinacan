package finance

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// ErrNotConfigured is a feature that needs configuration the operator has not given.
var ErrNotConfigured = errors.New("not configured")

// Sealer encrypts small identifiers (bank account numbers) with XChaCha20-Poly1305. The additional data
// binds a ciphertext to its row and column, so a value copied to another bank account fails to open.
// Output: v1.<base64url(nonce||ciphertext)>. The key comes from CAN_FINANCE_SEAL_KEY (64 hex characters).
type Sealer struct{ aead cipher.AEAD }

// NewSealer builds a Sealer from a hex key of 32 bytes.
func NewSealer(hexKey string) (*Sealer, error) {
	k, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(k) != chacha20poly1305.KeySize {
		return nil, errors.New("the seal key must be 64 hex characters (32 bytes), for example `openssl rand -hex 32`")
	}
	aead, err := chacha20poly1305.NewX(k)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plain bound to aad.
func (s *Sealer) Seal(plain, aad string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := s.aead.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return "v1." + base64.RawURLEncoding.EncodeToString(ct), nil
}

// Open decrypts a sealed value; it fails for a different aad or any change to the ciphertext.
func (s *Sealer) Open(sealed, aad string) (string, error) {
	body, ok := strings.CutPrefix(sealed, "v1.")
	if !ok {
		return "", errors.New("unknown seal version")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) < s.aead.NonceSize()+s.aead.Overhead() {
		return "", errors.New("malformed sealed value")
	}
	n := s.aead.NonceSize()
	pt, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(aad))
	if err != nil {
		return "", errors.New("the sealed value does not open")
	}
	return string(pt), nil
}
