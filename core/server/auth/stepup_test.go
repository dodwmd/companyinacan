package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// Confirmations sent side by side must not get more guesses than ones sent one after another.
func TestStepUpLimitHoldsUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := login(t, e)
	sess, u, err := e.svc.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = e.svc.Reauthenticate(ctx, sess, u, "wrong password!!", "000000", "10.0.0.1")
		}()
	}
	close(start)
	wg.Wait()
	verified := 0
	for _, err := range errs {
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			verified++
		case !errors.Is(err, ErrUnauthenticated):
			t.Fatalf("unexpected: %v", err)
		}
	}
	if verified != stepUpAttempts {
		t.Fatalf("%d attempts were checked, want %d", verified, stepUpAttempts)
	}
	// Past the limit even the right credentials are not looked at, and the session is over.
	if err := e.svc.Reauthenticate(ctx, sess, u, e.pw, freshCode(t, e), "10.0.0.1"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("after the limit: %v", err)
	}
	if _, _, err := e.svc.Authenticate(ctx, res.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("the session survived: %v", err)
	}
}

// The older throttle of account changes counts before it verifies, so it holds the same way.
func TestAccountThrottleHoldsUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := login(t, e)
	_, u, err := e.svc.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	const n = 40
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = e.svc.ConfirmPassword(ctx, u, "wrong password!!")
		}()
	}
	close(start)
	wg.Wait()
	checked := 0
	for _, err := range errs {
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			checked++
		case !errors.As(err, new(*LockedError)):
			t.Fatalf("unexpected: %v", err)
		}
	}
	if checked != accountMaxFailures {
		t.Fatalf("%d attempts were checked, want %d", checked, accountMaxFailures)
	}
}
