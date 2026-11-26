package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/store"
)

// flood sends n wrong-password attempts for name side by side and returns their results.
func flood(e *env, name string, n int) <-chan error {
	out := make(chan error, n)
	for range n {
		go func() {
			_, err := e.svc.Login(context.Background(), LoginRequest{Username: name, Password: "junk junk junk", Code: "000000", IP: "10.42.0.7"})
			out <- err
		}()
	}
	return out
}

// await reads n results; they must arrive although the hashing slot is taken.
func await(t *testing.T, results <-chan error, n int) {
	t.Helper()
	for i := range n {
		select {
		case err := <-results:
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("junk attempt: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%d of %d junk attempts are waiting for the hashing slot", n-i, n)
		}
	}
}

func floodEnv(t *testing.T) *env {
	e := guardEnv(t, false)
	e.svc.opt.HashWait = time.Minute
	e.svc.slots = make(chan struct{}, 1) // as in production
	return e
}

// overBudget pushes a name past its code-first attempts.
func overBudget(t *testing.T, e *env, name string) {
	t.Helper()
	for range accountFreeAttempts {
		_, _ = e.svc.Login(context.Background(), LoginRequest{Username: name, Password: "junk junk junk", Code: "000000", IP: "10.42.0.7"})
	}
}

// A flood on any name, known or not, puts at most one hash in front of everybody else.
func TestAJunkFloodDoesNotQueueForTheHashingSlot(t *testing.T) {
	e := floodEnv(t)
	ctx := context.Background()
	overBudget(t, e, "nobody")
	release, err := e.svc.acquire(ctx) // a hash is running
	if err != nil {
		t.Fatal(err)
	}
	const n = 60
	results := flood(e, "nobody", n)
	await(t, results, n-1) // all but one are turned away; that one waits for the slot
	if len(e.svc.pf.hashGate) != 1 {
		t.Fatal("no password-first check is waiting")
	}
	owner := make(chan error, 1)
	go func() {
		_, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "10.42.0.7"})
		owner <- err
	}()
	release()
	if err := <-owner; err != nil {
		t.Fatalf("owner refused during a flood on another name: %v", err)
	}
	await(t, results, 1)
}

// A flood on the owner's own name costs no hashing once the password has been seen, so the owner
// gets in, and step-up, password change and set-up links are not held up either.
func TestAJunkFloodOnTheOwnersNameDoesNotKeepTheOwnerOut(t *testing.T) {
	e := floodEnv(t)
	ctx := context.Background()
	first, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "10.42.0.7"})
	if err != nil {
		t.Fatal(err)
	}
	overBudget(t, e, "root")
	release, err := e.svc.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	await(t, flood(e, "root", 60), 60) // none of it needs the slot
	release()

	overBudget(t, e, "nobody")
	*e.now = e.now.Add(30 * time.Second) // before the flood starts reading the clock
	junk := flood(e, "nobody", 60)       // and a flood on another name at the same time
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "10.42.0.7"}); err != nil {
		t.Fatalf("owner refused during a flood on their name: %v", err)
	}
	u, err := e.db.AdminByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	await(t, junk, 60)
	junk = flood(e, "nobody", 60)
	// The login above used this time step's code; the step before is still accepted but older, so
	// the step-up takes the next one.
	next, _ := DecodeTOTPSecret(e.sec)
	if err := e.svc.Reauthenticate(ctx, first.Session, u, e.pw, Code(next, SHA1, 6, 30, e.now.Add(30*time.Second)), "10.0.0.1"); err != nil {
		t.Fatalf("step-up: %v", err)
	}
	if err := e.svc.ConfirmPassword(ctx, u, e.pw); err != nil {
		t.Fatalf("password change: %v", err)
	}
	if _, err := e.svc.NewPasswordHash(ctx, "a brand new password"); err != nil {
		t.Fatalf("set-up link: %v", err)
	}
	await(t, junk, 60)
}

// Behind the proxy an attempt that was turned away compared nothing and does not count.
func TestTurnedAwayAttemptsDoNotCountBehindTheProxy(t *testing.T) {
	e := guardEnv(t, true)
	ctx := context.Background()
	for i := range accountFreeAttempts {
		_, _ = e.svc.Login(ctx, LoginRequest{Username: "root", Password: "junk junk junk", Code: "000000", IP: fmt.Sprintf("198.51.100.%d", i)})
	}
	e.svc.pf.hashGate <- struct{}{} // another check is under way
	for range 10 {
		if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "192.0.2.3"}); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatal(err)
		}
	}
	<-e.svc.pf.hashGate
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "192.0.2.3"}); err != nil {
		t.Fatalf("turned-away attempts locked the address: %v", err)
	}
}

// A changed password is not recognised by what was remembered of the old one.
func TestRememberedPasswordsFollowTheStoredHash(t *testing.T) {
	p := newPasswordFirst()
	p.remember("hash-1", "old password")
	if match, known := p.recall("hash-1", "old password"); !match || !known {
		t.Fatal("not recalled")
	}
	if match, known := p.recall("hash-1", "other"); match || !known {
		t.Fatal("another password matched")
	}
	if _, known := p.recall("hash-2", "old password"); known {
		t.Fatal("known under another hash")
	}
}

// Someone who knows the password and goes slowly must still run into the code lock.
func TestSlowCodeGuessingStillTripsTheLock(t *testing.T) {
	e := guardEnv(t, false)
	ctx := context.Background()
	guess := func() {
		_, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.wrongCode(t), IP: "10.42.0.7"})
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatal(err)
		}
	}
	// One guess every 100 minutes: never two in one 15-minute window.
	for range accountFreeAttempts + accountCodeFailures {
		guess()
		*e.now = e.now.Add(95 * time.Minute)
		if _, err := e.db.PruneLoginAttempts(ctx, *e.now, DefaultPolicy.Window); err != nil { // hourly housekeeping
			t.Fatal(err)
		}
		if a, _ := e.db.GetLoginAttempt(ctx, store.LoginCodeKey+"root"); !a.LockedUntil.IsZero() && a.Failures >= accountCodeFailures {
			return
		}
		*e.now = e.now.Add(5 * time.Minute)
	}
	t.Fatal("15 slow guesses with the right password never locked the codes")
}

func TestBusyGivesTheStepUpAndAccountAttemptsBack(t *testing.T) {
	e := guardEnv(t, false)
	ctx := context.Background()
	res, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := e.db.AdminByUsername(ctx, "root")
	*e.now = e.now.Add(30 * time.Second)
	e.svc.opt.HashWait, e.svc.slots = time.Millisecond, make(chan struct{}, 1)
	release, err := e.svc.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range stepUpAttempts + accountMaxFailures {
		if err := e.svc.Reauthenticate(ctx, res.Session, u, e.pw, e.code(t), "10.0.0.1"); !errors.Is(err, ErrBusy) {
			t.Fatalf("step-up: %v", err)
		}
		if err := e.svc.ConfirmPassword(ctx, u, e.pw); !errors.Is(err, ErrBusy) {
			t.Fatalf("account: %v", err)
		}
	}
	release()
	e.svc.opt.HashWait = time.Minute
	if err := e.svc.Reauthenticate(ctx, res.Session, u, e.pw, e.code(t), "10.0.0.1"); err != nil {
		t.Fatalf("busy answers used up the step-up attempts: %v", err)
	}
	if err := e.svc.ConfirmPassword(ctx, u, e.pw); err != nil {
		t.Fatalf("busy answers locked account changes: %v", err)
	}
}
