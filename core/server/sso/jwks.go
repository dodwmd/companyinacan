package sso

import (
	"context"
	"sync"
	"time"
)

// Key cache limits.
const (
	keyTTL        = time.Hour        // refresh keys older than this (on use)
	keyMinRefetch = 30 * time.Second // at most one refetch per interval, whatever tokens arrive
	keyMaxStale   = 24 * time.Hour   // a cached key set older than this is not trusted any more
)

// keySet caches the provider's signing keys. A token with an unknown kid triggers one refetch
// (key rotation), shared by all concurrent callers (single flight) and rate limited, so a flood of
// forged kids cannot turn into a flood of requests to the provider. A failed fetch never lets an
// unknown key through: the answer is ErrUnknownKey.
type keySet struct {
	fetch func(ctx context.Context) ([]Key, error)
	now   func() time.Time

	mu        sync.Mutex
	keys      []Key
	fetchedAt time.Time // last successful fetch
	lastTry   time.Time // last fetch attempt, successful or not
	inflight  *flight
}

type flight struct {
	done chan struct{}
	err  error
}

func newKeySet(fetch func(context.Context) ([]Key, error), now func() time.Time) *keySet {
	return &keySet{fetch: fetch, now: now}
}

// Find returns the key for kid (any key of the algorithm when kid is empty and exactly one such key
// exists).
func (k *keySet) Find(ctx context.Context, kid, alg string) (Key, error) {
	if key, ok := k.lookup(kid, alg, true); ok {
		return key, nil
	}
	if err := k.refresh(ctx); err != nil {
		// A failed refetch never admits an unknown key; a known one was returned above.
		if key, ok := k.lookup(kid, alg, true); ok {
			return key, nil
		}
		return Key{}, ErrUnknownKey
	}
	if key, ok := k.lookup(kid, alg, false); ok {
		return key, nil
	}
	return Key{}, ErrUnknownKey
}

func (k *keySet) lookup(kid, alg string, mustBeFresh bool) (Key, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	if len(k.keys) == 0 || now.Sub(k.fetchedAt) > keyMaxStale {
		return Key{}, false
	}
	if mustBeFresh && now.Sub(k.fetchedAt) > keyTTL && now.Sub(k.lastTry) >= keyMinRefetch {
		return Key{}, false // due for a refresh; the caller refreshes first
	}
	var match []Key
	for _, key := range k.keys {
		if key.Alg == alg && (kid == "" || key.Kid == kid) {
			match = append(match, key)
		}
	}
	if len(match) == 1 || (kid != "" && len(match) > 0) {
		return match[0], true
	}
	return Key{}, false
}

// refresh refetches unless one happened (or was attempted) within keyMinRefetch; concurrent
// callers share one fetch.
func (k *keySet) refresh(ctx context.Context) error {
	k.mu.Lock()
	if k.inflight != nil {
		f := k.inflight
		k.mu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !k.lastTry.IsZero() && k.now().Sub(k.lastTry) < keyMinRefetch {
		have := len(k.keys) > 0 && k.now().Sub(k.fetchedAt) <= keyMaxStale
		k.mu.Unlock()
		if have {
			return nil // rate limited: answer from the cache we have
		}
		return ErrUnknownKey
	}
	f := &flight{done: make(chan struct{})}
	k.inflight = f
	k.lastTry = k.now()
	k.mu.Unlock()

	keys, err := k.fetch(ctx)

	k.mu.Lock()
	if err == nil {
		k.keys, k.fetchedAt = keys, k.now()
	}
	k.inflight = nil
	k.mu.Unlock()
	f.err = err
	close(f.done)
	return err
}
