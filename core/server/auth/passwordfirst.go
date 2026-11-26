package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"sync"
)

// Password-first checks (guard.go) are the only place where a request needs no valid code to cause
// hashing, so they must not starve the single argon2 slot that valid sign-ins, step-ups, password
// changes and set-up links wait for:
//
//   - at most one of them holds or waits for the slot (hashGate); the others are turned away at once,
//     so anyone else waits for at most the hash running and that one;
//   - a password already verified since can-core started is recognised by a keyed MAC held in memory,
//     with no hashing at all, so a flood on the owner's own name cannot keep the owner out either.
type passwordFirst struct {
	hashGate chan struct{}
	key      [32]byte

	mu    sync.Mutex
	known map[string][]byte // stored argon2 hash -> MAC of the password that matched it
}

// maxKnown bounds the MACs kept; a changed password leaves its old entry behind.
const maxKnown = 1024

func newPasswordFirst() *passwordFirst {
	p := &passwordFirst{hashGate: make(chan struct{}, 1), known: map[string][]byte{}}
	if _, err := rand.Read(p.key[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return p
}

func (p *passwordFirst) mac(password string) []byte {
	m := hmac.New(sha256.New, p.key[:])
	m.Write([]byte(password))
	return m.Sum(nil)
}

// remember records that password matched the stored hash.
func (p *passwordFirst) remember(hash, password string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.known) >= maxKnown {
		clear(p.known)
	}
	p.known[hash] = p.mac(password)
}

// recall compares password with the one known to match hash; known is false when there is none.
func (p *passwordFirst) recall(hash, password string) (match, known bool) {
	p.mu.Lock()
	want := p.known[hash]
	p.mu.Unlock()
	if want == nil {
		return false, false
	}
	return hmac.Equal(want, p.mac(password)), true
}

// checkPasswordFirst checks the password of an attempt whose code has not earned any hashing.
// evaluated is false when it was turned away because another such check is under way: nothing was
// compared, whatever the code was.
func (s *Service) checkPasswordFirst(ctx context.Context, password, hash string) (match, evaluated bool, err error) {
	if match, known := s.pf.recall(hash, password); known {
		return match, true, nil
	}
	select {
	case s.pf.hashGate <- struct{}{}:
		defer func() { <-s.pf.hashGate }()
	default:
		return false, false, nil
	}
	match, err = s.verify(ctx, password, hash)
	return match, err == nil, err
}
