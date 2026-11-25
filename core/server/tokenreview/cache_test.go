package tokenreview

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

type counting struct {
	calls atomic.Int32
	next  func(token string) (Identity, error)
}

func (c *counting) Review(_ context.Context, token, _ string) (Identity, error) {
	c.calls.Add(1)
	return c.next(token)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newCached(next Reviewer, o CacheOptions) (*Cached, *clock) {
	ck := &clock{t: time.Unix(1_800_000_000, 0)}
	o.Now = ck.now
	return NewCached(next, o), ck
}

func good(token string) (Identity, error) {
	if token == "good" {
		return Identity{Namespace: "mod-a", ServiceAccount: "a"}, nil
	}
	return Identity{}, ErrInvalidToken
}

func TestPositiveCacheExpires(t *testing.T) {
	inner := &counting{next: good}
	c, ck := newCached(inner, CacheOptions{PositiveTTL: 30 * time.Second, NegativeTTL: 5 * time.Second})
	for i := 0; i < 5; i++ {
		if id, err := c.Review(context.Background(), "good", Audience); err != nil || id.Namespace != "mod-a" {
			t.Fatal(id, err)
		}
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("calls=%d", inner.calls.Load())
	}
	ck.advance(31 * time.Second)
	if _, err := c.Review(context.Background(), "good", Audience); err != nil || inner.calls.Load() != 2 {
		t.Fatalf("expired entry not re-reviewed: %d %v", inner.calls.Load(), err)
	}
}

func TestNegativeCacheShieldsTheAPIServer(t *testing.T) {
	inner := &counting{next: good}
	c, ck := newCached(inner, CacheOptions{NegativeTTL: 5 * time.Second})
	for i := 0; i < 10; i++ {
		if _, err := c.Review(context.Background(), "bad", Audience); !errors.Is(err, ErrInvalidToken) {
			t.Fatal(err)
		}
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("a repeated invalid token reached the API server %d times", inner.calls.Load())
	}
	ck.advance(6 * time.Second)
	_, _ = c.Review(context.Background(), "bad", Audience)
	if inner.calls.Load() != 2 {
		t.Fatal("negative entry never expired")
	}
}

func TestAudienceIsPartOfTheKey(t *testing.T) {
	inner := &counting{next: good}
	c, _ := newCached(inner, CacheOptions{})
	_, _ = c.Review(context.Background(), "good", Audience)
	_, _ = c.Review(context.Background(), "good", "other")
	if inner.calls.Load() != 2 {
		t.Fatal("different audiences shared a cache entry")
	}
}

func TestInfrastructureErrorsAreNotCached(t *testing.T) {
	boom := errors.New("api down")
	fail := true
	inner := &counting{next: func(string) (Identity, error) {
		if fail {
			return Identity{}, boom
		}
		return Identity{Namespace: "mod-a", ServiceAccount: "a"}, nil
	}}
	c, _ := newCached(inner, CacheOptions{})
	if _, err := c.Review(context.Background(), "t", Audience); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	fail = false
	if _, err := c.Review(context.Background(), "t", Audience); err != nil {
		t.Fatalf("outage was cached as a failure: %v", err)
	}
}

func TestPositiveTTLIsCappedByTokenExpiry(t *testing.T) {
	inner := &counting{next: good}
	c, ck := newCached(inner, CacheOptions{PositiveTTL: time.Hour})
	exp := ck.now().Add(10 * time.Second).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	tok := "h." + payload + ".s"
	inner.next = func(string) (Identity, error) { return Identity{Namespace: "mod-a", ServiceAccount: "a"}, nil }
	_, _ = c.Review(context.Background(), tok, Audience)
	ck.advance(11 * time.Second)
	_, _ = c.Review(context.Background(), tok, Audience)
	if inner.calls.Load() != 2 {
		t.Fatal("an expired token stayed valid through the cache")
	}
}

func TestGlobalRateLimit(t *testing.T) {
	inner := &counting{next: good}
	c, ck := newCached(inner, CacheOptions{Rate: 10, Burst: 3})
	var limited int
	for i := 0; i < 10; i++ {
		_, err := c.Review(context.Background(), fmt.Sprintf("tok-%d", i), Audience)
		if errors.Is(err, ErrRateLimited) {
			limited++
		}
	}
	if inner.calls.Load() != 3 || limited != 7 {
		t.Fatalf("calls=%d limited=%d", inner.calls.Load(), limited)
	}
	ck.advance(time.Second)
	if _, err := c.Review(context.Background(), "tok-new", Audience); errors.Is(err, ErrRateLimited) {
		t.Fatal("bucket did not refill")
	}
}

func TestPerSourceLimitStopsOneNoisyCallerStarvingOthers(t *testing.T) {
	inner := &counting{next: good}
	c, _ := newCached(inner, CacheOptions{Rate: 1000, Burst: 1000, SourceRate: 1, SourceBurst: 2})
	noisy := WithSource(context.Background(), "10.42.0.9")
	var limited int
	for i := 0; i < 10; i++ {
		if _, err := c.Review(noisy, fmt.Sprintf("junk-%d", i), Audience); errors.Is(err, ErrRateLimited) {
			limited++
		}
	}
	if limited != 8 {
		t.Fatalf("limited=%d", limited)
	}
	quiet := WithSource(context.Background(), "10.42.0.10")
	if _, err := c.Review(quiet, "good", Audience); err != nil {
		t.Fatalf("another source was starved: %v", err)
	}
}

func TestCacheHitsAreNotRateLimited(t *testing.T) {
	inner := &counting{next: good}
	c, _ := newCached(inner, CacheOptions{Rate: 1, Burst: 1})
	for i := 0; i < 50; i++ {
		if _, err := c.Review(context.Background(), "good", Audience); err != nil {
			t.Fatalf("hit %d: %v", i, err)
		}
	}
}

func TestCacheIsBounded(t *testing.T) {
	inner := &counting{next: good}
	c, _ := newCached(inner, CacheOptions{MaxEntries: 8})
	for i := 0; i < 100; i++ {
		_, _ = c.Review(context.Background(), fmt.Sprintf("junk-%d", i), Audience)
	}
	if n := c.size(); n > 8 {
		t.Fatalf("cache holds %d entries", n)
	}
}
