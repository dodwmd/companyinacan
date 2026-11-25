package tokenreview

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSingleflightCoalescesConcurrentReviews(t *testing.T) {
	release := make(chan struct{})
	inner := &counting{next: func(string) (Identity, error) {
		<-release
		return Identity{Namespace: "mod-a", ServiceAccount: "a"}, nil
	}}
	c, _ := newCached(inner, CacheOptions{Rate: 1000, Burst: 1000})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Review(context.Background(), "same", Audience); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := inner.calls.Load(); n != 1 {
		t.Fatalf("%d reviews reached the API server for one token", n)
	}
}

func TestLRUKeepsTheHotEntry(t *testing.T) {
	inner := &counting{next: func(tok string) (Identity, error) {
		if tok == "hot" {
			return Identity{Namespace: "mod-a", ServiceAccount: "a"}, nil
		}
		return Identity{}, ErrInvalidToken
	}}
	c, _ := newCached(inner, CacheOptions{MaxEntries: 4, Rate: 1e6, Burst: 1000000})
	_, _ = c.Review(context.Background(), "hot", Audience)
	for i := 0; i < 50; i++ {
		_, _ = c.Review(context.Background(), fmt.Sprintf("junk-%d", i), Audience)
		_, _ = c.Review(context.Background(), "hot", Audience)
	}
	before := inner.calls.Load()
	_, _ = c.Review(context.Background(), "hot", Audience)
	if inner.calls.Load() != before {
		t.Fatal("the frequently used entry was evicted by junk tokens")
	}
	if c.size() > 4 {
		t.Fatal(c.size())
	}
}
