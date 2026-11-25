package equity

import (
	"sync"
	"time"
)

// rateLimiter is a token bucket per key with a bounded key table. It exists to slow credential
// guessing and bulk scraping of equity data; it is not a substitute for network policy.
type rateLimiter struct {
	mu    sync.Mutex
	cap   float64 // burst
	rate  float64 // tokens per second
	max   int     // tracked keys; the table is reset when exceeded (fails open on the table, never on a key)
	now   func() time.Time
	items map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newRateLimiter(burst, perSecond float64, maxKeys int, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{cap: burst, rate: perSecond, max: maxKeys, now: now, items: map[string]*bucket{}}
}

func (r *rateLimiter) refill(key string) *bucket {
	t := r.now()
	b := r.items[key]
	if b == nil {
		if len(r.items) >= r.max {
			r.items = map[string]*bucket{}
		}
		b = &bucket{tokens: r.cap, at: t}
		r.items[key] = b
		return b
	}
	b.tokens += t.Sub(b.at).Seconds() * r.rate
	if b.tokens > r.cap {
		b.tokens = r.cap
	}
	b.at = t
	return b
}

// allow consumes one token and reports whether one was available.
func (r *rateLimiter) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.refill(key)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// denyLimiter caps how many denial audit records one actor can generate per minute, so a
// misbehaving client cannot fill the audit log. The first suppressed denial in a window is
// itself recorded ("deny-throttled").
type denyLimiter struct {
	mu  sync.Mutex
	win map[string]*denyWindow
}

type denyWindow struct {
	start time.Time
	n     int
}

const denyAuditPerMinute = 30

// check returns (record, throttledNotice).
func (d *denyLimiter) check(actor string, now time.Time) (record, notice bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.win == nil || len(d.win) > 10000 {
		d.win = map[string]*denyWindow{}
	}
	w := d.win[actor]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &denyWindow{start: now}
		d.win[actor] = w
	}
	w.n++
	switch {
	case w.n <= denyAuditPerMinute:
		return true, false
	case w.n == denyAuditPerMinute+1:
		return false, true
	}
	return false, false
}
