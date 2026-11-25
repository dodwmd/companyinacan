package sso

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Only these algorithms are accepted. "none" and the HMAC family are refused outright: a
// symmetric algorithm with the provider's public key as the secret is the classic downgrade.
const (
	AlgRS256 = "RS256"
	AlgES256 = "ES256"
)

const (
	maxTokenBytes = 64 << 10
	maxJWKSKeys   = 32
	minRSABits    = 2048
)

// Key is a verification key from a JWKS.
type Key struct {
	Kid string
	Alg string // AlgRS256 or AlgES256, from the key type
	Pub crypto.PublicKey
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// ParseJWKS reads a JWKS document, keeping RSA keys of at least 2048 bits and P-256 keys that are
// usable for signatures. Unusable keys are skipped, not fatal; no usable key is an error.
func ParseJWKS(raw []byte) ([]Key, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	if len(doc.Keys) > maxJWKSKeys {
		return nil, errors.New("jwks: too many keys")
	}
	var out []Key
	for _, k := range doc.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			if k.Alg != "" && k.Alg != AlgRS256 {
				continue
			}
			n, e := b64Int(k.N), b64Int(k.E)
			if n == nil || e == nil || n.BitLen() < minRSABits || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
				continue
			}
			out = append(out, Key{Kid: k.Kid, Alg: AlgRS256, Pub: &rsa.PublicKey{N: n, E: int(e.Int64())}})
		case "EC":
			if k.Crv != "P-256" || (k.Alg != "" && k.Alg != AlgES256) {
				continue
			}
			x, y := b64Int(k.X), b64Int(k.Y)
			if x == nil || y == nil || x.BitLen() > 256 || y.BitLen() > 256 {
				continue
			}
			pt := append(append([]byte{4}, x.FillBytes(make([]byte, 32))...), y.FillBytes(make([]byte, 32))...)
			pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pt) // rejects points not on the curve
			if err != nil {
				continue
			}
			out = append(out, Key{Kid: k.Kid, Alg: AlgES256, Pub: pub})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("jwks: no usable signing key")
	}
	return out, nil
}

func b64Int(s string) *big.Int {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil || len(b) == 0 {
		return nil
	}
	return new(big.Int).SetBytes(b)
}

// Header is the part of the JOSE header that is read.
type Header struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Typ  string   `json:"typ"`
	Crit []string `json:"crit"`
}

// Errors of token parsing and verification.
var (
	ErrToken      = errors.New("invalid token")
	ErrAlg        = errors.New("unsupported token algorithm")
	ErrSignature  = errors.New("bad token signature")
	ErrUnknownKey = errors.New("unknown signing key")
)

// SplitToken parses the compact serialisation without verifying anything: header, claims JSON and
// the signing input and signature for VerifySignature.
func SplitToken(tok string) (h Header, claims []byte, signingInput string, sig []byte, err error) {
	if len(tok) == 0 || len(tok) > maxTokenBytes {
		return h, nil, "", nil, ErrToken
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return h, nil, "", nil, ErrToken
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	cb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sb, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return h, nil, "", nil, ErrToken
	}
	if err := json.Unmarshal(hb, &h); err != nil {
		return h, nil, "", nil, ErrToken
	}
	if h.Alg != AlgRS256 && h.Alg != AlgES256 {
		return h, nil, "", nil, ErrAlg
	}
	if len(h.Crit) > 0 {
		return h, nil, "", nil, ErrToken // no critical extensions are understood
	}
	return h, cb, parts[0] + "." + parts[1], sb, nil
}

// VerifySignature checks sig over signingInput with k, which must match alg.
func VerifySignature(k Key, alg, signingInput string, sig []byte) error {
	if k.Alg != alg {
		return ErrAlg
	}
	sum := sha256.Sum256([]byte(signingInput))
	switch pub := k.Pub.(type) {
	case *rsa.PublicKey:
		if rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
			return ErrSignature
		}
	case *ecdsa.PublicKey:
		if len(sig) != 64 {
			return ErrSignature
		}
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		if !ecdsa.Verify(pub, sum[:], r, s) {
			return ErrSignature
		}
	default:
		return ErrAlg
	}
	return nil
}

// DecodeClaims parses claims JSON keeping numbers exact.
func DecodeClaims(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, ErrToken
	}
	return m, nil
}
