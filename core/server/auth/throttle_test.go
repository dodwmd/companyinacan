package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func (e *env) wrongCode(t *testing.T) string {
	for i := 0; ; i++ {
		c := fmt.Sprintf("%06d", i)
		if _, ok := ValidateTOTP(e.sec, c, *e.now); !ok {
			return c
		}
	}
}

func throttleEnv(t *testing.T, hardLock bool) *env {
	e := newEnv(t)
	pol := DefaultPolicy
	pol.HardLock, pol.AttemptsPerMin = hardLock, 100000
	e.svc = NewService(e.db, audit.New(e.db), Options{Now: func() time.Time { return *e.now }, Policy: pol,
		MinLoginTime: time.Nanosecond, Sleep: noSleep, HashSlots: 4, HashWait: time.Minute})
	return e
}

// takeSlots occupies every hashing slot: any password check from now on answers ErrBusy.
func takeSlots(t *testing.T, e *env) (release func()) {
	t.Helper()
	e.svc.opt.HashWait = time.Millisecond
	var rs []func()
	for range cap(e.svc.slots) {
		r, err := e.svc.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	return func() {
		for _, r := range rs {
			r()
		}
		e.svc.opt.HashWait = time.Minute
	}
}

// Without a valid code no password is ever checked, however often an account has failed, known or
// not: otherwise junk could take the hashing slot from everyone, or a password could be confirmed
// without the code.
func TestNoPasswordCheckWithoutAValidCode(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(fmt.Sprint("hardLock=", hard), func(t *testing.T) {
			e := throttleEnv(t, hard)
			ctx := context.Background()
			release := takeSlots(t, e)
			bad := e.wrongCode(t)
			for i := range 3 * failedAlert {
				for _, name := range []string{"root", "nobody"} {
					for _, pw := range []string{e.pw, "not the password"} {
						_, err := e.svc.Login(ctx, LoginRequest{Username: name, Password: pw, Code: bad, IP: fmt.Sprintf("198.51.%d.%d", i, len(pw))})
						if !errors.Is(err, ErrInvalidCredentials) {
							t.Fatalf("attempt %d for %s reached the password check: %v", i, name, err)
						}
					}
				}
			}
			release()
			// Many failures refuse nothing: the owner signs in.
			if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "203.0.113.9"}); err != nil {
				t.Fatalf("owner refused: %v", err)
			}
		})
	}
}

func TestManyFailuresAreReportedOnceADayAndStateIsBounded(t *testing.T) {
	e := throttleEnv(t, false)
	ctx := context.Background()
	alerts := func() (n int) {
		recs, err := audit.New(e.db).List(ctx, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if r.Action == "admin.login.many_failures" {
				n++
				if r.Actor != "root" {
					t.Errorf("alert for %q", r.Actor)
				}
			}
		}
		return n
	}
	fail := func(name string, times int) {
		for range times {
			_, err := e.svc.Login(ctx, LoginRequest{Username: name, Password: "not the password", Code: e.wrongCode(t), IP: "10.42.0.7"})
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatal(err)
			}
			*e.now = e.now.Add(time.Minute)
			if _, err := e.db.PruneLoginAttempts(ctx, *e.now, DefaultPolicy.Window); err != nil { // housekeeping
				t.Fatal(err)
			}
		}
	}
	fail("root", failedAlert-1)
	*e.now = e.now.Add(time.Hour) // far past the address window: the count is for a day
	if _, err := e.db.PruneLoginAttempts(ctx, *e.now, DefaultPolicy.Window); err != nil {
		t.Fatal(err)
	}
	if alerts() != 0 {
		t.Fatal("reported too early")
	}
	fail("ROOT", 1) // another spelling is no other name to guess against
	fail("root", 1)
	if n := alerts(); n != 1 {
		t.Fatalf("%d alerts, want 1", n)
	}
	fail("root", 2*failedAlert)
	if n := alerts(); n != 1 {
		t.Fatalf("%d alerts within a day, want 1", n)
	}
	for i := range 2 * failedAlert {
		fail(fmt.Sprintf("nobody%d", i), 1)
	}
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM login_attempts WHERE key LIKE ?`, store.LoginFailedKey+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n > 2 || alerts() != 1 { // root and the one shared by names that do not exist
		t.Fatalf("%d counters, %d alerts", n, alerts())
	}
	*e.now = e.now.Add(store.LoginFailedWindow + time.Hour)
	fail("root", failedAlert)
	if n := alerts(); n != 2 {
		t.Fatalf("%d alerts after a day, want 2", n)
	}
}

// Behind the proxy the attempt is counted before it is checked: requests sent together get no more
// than the limit.
func TestHardLockCountsBeforeChecking(t *testing.T) {
	e := throttleEnv(t, true)
	ctx := context.Background()
	var mu sync.Mutex
	evaluated := 0
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: "not the password", Code: e.code(t), IP: "192.0.2.1"})
			if errors.Is(err, ErrInvalidCredentials) {
				mu.Lock()
				evaluated++
				mu.Unlock()
			} else if !errors.As(err, new(*LockedError)) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if evaluated != DefaultPolicy.MaxUserFailures {
		t.Fatalf("%d attempts evaluated, limit %d", evaluated, DefaultPolicy.MaxUserFailures)
	}
}

// A success gives its reservation back, and so do attempts that compared nothing: hashing busy, or
// refused because another key is locked.
func TestHardLockOnlyFailuresCount(t *testing.T) {
	e := throttleEnv(t, true)
	ctx := context.Background()
	failures := func(key string) int {
		a, err := e.db.GetLoginAttempt(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return a.Failures
	}
	login := func(name, ip string) error {
		_, err := e.svc.Login(ctx, LoginRequest{Username: name, Password: e.pw, Code: e.code(t), IP: ip})
		return err
	}
	for range DefaultPolicy.MaxIPFailures + 5 {
		*e.now = e.now.Add(30 * time.Second) // a fresh code
		if err := login("root", "192.0.2.2"); err != nil {
			t.Fatal(err)
		}
	}
	if n := failures("ip:192.0.2.2"); n != 0 {
		t.Fatalf("successes counted against the address: %d", n)
	}
	release := takeSlots(t, e)
	*e.now = e.now.Add(30 * time.Second)
	for range 10 {
		if err := login("root", "192.0.2.2"); !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	}
	release()
	if u, ip := failures("user:root|192.0.2.2"), failures("ip:192.0.2.2"); u != 0 || ip != 0 {
		t.Fatalf("busy attempts counted: %d %d", u, ip)
	}
	// A locked pair: the refused attempts leave the counters as they were.
	for range DefaultPolicy.MaxUserFailures {
		_, _ = e.svc.Login(ctx, LoginRequest{Username: "other", Password: "x", Code: "000000", IP: "192.0.2.4"})
	}
	for range 10 {
		if err := login("other", "192.0.2.4"); !errors.As(err, new(*LockedError)) {
			t.Fatal(err)
		}
	}
	if u, ip := failures("user:other|192.0.2.4"), failures("ip:192.0.2.4"); u != DefaultPolicy.MaxUserFailures || ip != DefaultPolicy.MaxUserFailures {
		t.Fatalf("refused attempts counted: %d %d", u, ip)
	}
}

func TestBusyGivesTheStepUpAndAccountAttemptsBack(t *testing.T) {
	e := throttleEnv(t, false)
	ctx := context.Background()
	res, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := e.db.AdminByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	*e.now = e.now.Add(30 * time.Second)
	release := takeSlots(t, e)
	for range stepUpAttempts + accountMaxFailures {
		if err := e.svc.Reauthenticate(ctx, res.Session, u, e.pw, e.code(t), "10.0.0.1"); !errors.Is(err, ErrBusy) {
			t.Fatalf("step-up: %v", err)
		}
		if err := e.svc.ConfirmPassword(ctx, u, e.pw); !errors.Is(err, ErrBusy) {
			t.Fatalf("account: %v", err)
		}
	}
	release()
	if err := e.svc.Reauthenticate(ctx, res.Session, u, e.pw, e.code(t), "10.0.0.1"); err != nil {
		t.Fatalf("busy answers used up the step-up attempts: %v", err)
	}
	if err := e.svc.ConfirmPassword(ctx, u, e.pw); err != nil {
		t.Fatalf("busy answers locked account changes: %v", err)
	}
}
