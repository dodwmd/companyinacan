package tokenreview

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrRateLimited is returned when a review that would reach the API server is refused by the rate
// limit. The module API answers 429 for it; it is not an authentication verdict.
var ErrRateLimited = errors.New("token review rate limited")

// CacheOptions tune Cached. Zero values select the defaults noted.
type CacheOptions struct {
	PositiveTTL time.Duration // default 30s (also capped by the token's own exp claim)
	NegativeTTL time.Duration // default 10s
	MaxEntries  int           // default 4096
	// Rate and Burst limit reviews that reach the API server, all callers together (default 50/s, 100).
	Rate  float64
	Burst int
	// SourceRate and SourceBurst limit one caller (see WithSource), default 5/s, 20.
	SourceRate  float64
	SourceBurst int
	Now         func() time.Time // tests
}

type sourceKey struct{}

// WithSource tags ctx with the caller's address for the per-source rate limit.
func WithSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}

type entry struct {
	id      Identity
	err     error // ErrInvalidToken for a negative entry
	expires time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Cached wraps a Reviewer with a positive cache (so a busy module does not cause one TokenReview per
// request), a negative cache (so a replayed bad token costs the API server one call per window) and
// a rate limit on the calls that remain (research R15). Infrastructure errors are never cached.
//
// Trade-off, stated plainly: a positive entry lives at most PositiveTTL (and never past the token's
// own expiry), so a token revoked by deleting its pod or ServiceAccount keeps working for up to that
// long. 30 seconds is the default.
type Cached struct {
	next Reviewer
	o    CacheOptions

	mu       sync.Mutex
	entries  map[[32]byte]*list.Element // value: *item; front = most recently used
	lru      *list.List
	inflight map[[32]byte]*call
	global   bucket
	sources  map[string]*bucket
}

// NewCached returns a Cached reviewer.
func NewCached(next Reviewer, o CacheOptions) *Cached {
	if o.PositiveTTL == 0 {
		o.PositiveTTL = 30 * time.Second
	}
	if o.NegativeTTL == 0 {
		o.NegativeTTL = 10 * time.Second
	}
	if o.MaxEntries == 0 {
		o.MaxEntries = 4096
	}
	if o.Rate == 0 {
		o.Rate, o.Burst = 50, 100
	}
	if o.SourceRate == 0 {
		o.SourceRate, o.SourceBurst = 5, 20
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Cached{next: next, o: o, entries: map[[32]byte]*list.Element{}, lru: list.New(), inflight: map[[32]byte]*call{}, sources: map[string]*bucket{}, global: bucket{tokens: float64(o.Burst), last: o.Now()}}
}

func (c *Cached) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func cacheKey(token, audience string) [32]byte {
	return sha256.Sum256([]byte(audience + "\x00" + token))
}

func take(b *bucket, rate float64, burst int, now time.Time) bool {
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// allow charges the global and (when known) the per-source bucket.
func (c *Cached) allow(ctx context.Context, now time.Time) bool {
	src, _ := ctx.Value(sourceKey{}).(string)
	c.mu.Lock()
	defer c.mu.Unlock()
	if src != "" {
		b := c.sources[src]
		if b == nil {
			if len(c.sources) >= 1024 { // bounded: a fresh start is harmless, buckets refill in seconds
				c.sources = map[string]*bucket{}
			}
			b = &bucket{tokens: float64(c.o.SourceBurst), last: now}
			c.sources[src] = b
		}
		if !take(b, c.o.SourceRate, c.o.SourceBurst, now) {
			return false
		}
	}
	return take(&c.global, c.o.Rate, c.o.Burst, now)
}

// tokenExpiry reads the exp claim of a JWT without verifying it. Safe only because it is used after
// the API server has accepted the token, and only to shorten the cache lifetime.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

type item struct {
	k [32]byte
	e entry
}

// call is one in-flight review shared by concurrent callers with the same token (singleflight), so
// a burst of identical requests costs the API server one TokenReview.
type call struct {
	done chan struct{}
	id   Identity
	err  error
}

// Review implements Reviewer.
func (c *Cached) Review(ctx context.Context, token, audience string) (Identity, error) {
	now := c.o.Now()
	k := cacheKey(token, audience)
	c.mu.Lock()
	if el, ok := c.entries[k]; ok {
		it := el.Value.(*item)
		if now.Before(it.e.expires) {
			c.lru.MoveToFront(el)
			c.mu.Unlock()
			return it.e.id, it.e.err
		}
		c.lru.Remove(el)
		delete(c.entries, k)
	}
	if cl, ok := c.inflight[k]; ok { // someone is already asking about this very token
		c.mu.Unlock()
		select {
		case <-cl.done:
			return cl.id, cl.err
		case <-ctx.Done():
			return Identity{}, ctx.Err()
		}
	}
	c.mu.Unlock()

	if !c.allow(ctx, now) {
		return Identity{}, ErrRateLimited
	}
	cl := &call{done: make(chan struct{})}
	c.mu.Lock()
	if other, ok := c.inflight[k]; ok { // lost a race with another caller since the check above
		c.mu.Unlock()
		select {
		case <-other.done:
			return other.id, other.err
		case <-ctx.Done():
			return Identity{}, ctx.Err()
		}
	}
	c.inflight[k] = cl
	c.mu.Unlock()

	id, err := c.next.Review(ctx, token, audience)
	switch {
	case err == nil:
		ttl := c.o.PositiveTTL
		if exp, ok := tokenExpiry(token); ok {
			if left := exp.Sub(now); left < ttl {
				ttl = left
			}
		}
		if ttl > 0 {
			c.store(k, entry{id: id, expires: now.Add(ttl)}, now)
		}
	case errors.Is(err, ErrInvalidToken):
		c.store(k, entry{err: err, expires: now.Add(c.o.NegativeTTL)}, now)
	}
	c.mu.Lock()
	delete(c.inflight, k)
	c.mu.Unlock()
	cl.id, cl.err = id, err
	close(cl.done)
	return id, err
}

// store inserts at the front and evicts least recently used entries beyond MaxEntries, so a flood of
// junk tokens pushes out other junk first and a busy module's entry stays hot.
func (c *Cached) store(k [32]byte, e entry, _ time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		el.Value.(*item).e = e
		c.lru.MoveToFront(el)
		return
	}
	c.entries[k] = c.lru.PushFront(&item{k: k, e: e})
	for c.lru.Len() > c.o.MaxEntries {
		last := c.lru.Back()
		c.lru.Remove(last)
		delete(c.entries, last.Value.(*item).k)
	}
}
