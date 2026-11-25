package obs

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestHealthFirstCallersDisconnectDoesNotCacheDown(t *testing.T) {
	hl := NewHealth()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hl.Add("db", true, func(ctx context.Context) error {
		once.Do(func() { close(started) })
		<-release
		return ctx.Err() // a cancelled check context would report down
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hl.Check(ctx); close(done) }()
	<-started
	cancel() // the first caller disconnects while the check runs
	<-done
	close(release)
	// a later caller must see the real answer, not a "down" cached from the first caller's cancellation
	var rep Report
	for i := 0; i < 100; i++ {
		rep = hl.Check(context.Background())
		if rep.Status == StatusOK {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rep.Status != StatusOK {
		t.Fatalf("caller disconnect poisoned the cache: %+v", rep)
	}
}

func TestHealthConcurrentCallersShareOneRun(t *testing.T) {
	hl := NewHealth()
	hl.SetTTL(0)
	var mu sync.Mutex
	var running, peak, runs int
	hl.Add("db", true, func(context.Context) error {
		mu.Lock()
		running++
		runs++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		time.Sleep(200 * time.Millisecond) // long enough that every caller joins the one run, even on a loaded machine
		mu.Lock()
		running--
		mu.Unlock()
		return nil
	})
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); hl.Check(context.Background()) }()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if peak != 1 {
		t.Fatalf("checks overlapped: peak %d", peak)
	}
	if runs > 4 {
		t.Fatalf("20 concurrent probes ran the check %d times in %v; they should share a run", runs, time.Since(start))
	}
}

func TestHealthCallerWhoGivesUpGetsAnAnswerQuickly(t *testing.T) {
	hl := NewHealth()
	// The check blocks until the test releases it and the per-check timeout is an hour, so the only way
	// Check can return is by giving up on the caller's context: no timing threshold is involved.
	hl.SetTimeout(time.Hour)
	release := make(chan struct{})
	defer close(release)
	hl.Add("slow", true, func(ctx context.Context) error { <-release; return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	returned := make(chan struct{})
	go func() { hl.Check(ctx); close(returned) }()
	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("a cancelled caller kept waiting for the check")
	}
}
