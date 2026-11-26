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

func guardEnv(t *testing.T, hardLock bool) *env {
	e := newEnv(t)
	pol := DefaultPolicy
	pol.HardLock, pol.AttemptsPerMin = hardLock, 100000
	e.svc = NewService(e.db, audit.New(e.db), Options{Now: func() time.Time { return *e.now }, Policy: pol,
		MinLoginTime: time.Nanosecond, Sleep: noSleep, HashSlots: 4, HashWait: time.Minute})
	return e
}

// Someone who knows the password and tries codes side by side, with no proxy to lock them out, must
// run out of tries: afterwards even the right code is refused, with the same answer.
func TestParallelCodeGuessesWithTheRightPasswordRunOut(t *testing.T) {
	e := guardEnv(t, false)
	ctx := context.Background()
	login := func(code string) error {
		_, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: code, IP: "10.42.0.7"})
		return err
	}
	bad := e.wrongCode(t)
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := login(bad); !errors.Is(err, ErrInvalidCredentials) {
				t.Errorf("guess: %v", err)
			}
		}()
	}
	wg.Wait()
	// Some of those were turned away without being looked at; how many depends on the scheduling.
	for range accountCodeFailures {
		_ = login(bad)
	}
	if err := login(e.code(t)); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("the guesser's right code was taken after 100 wrong ones: %v", err)
	}
	var locked int
	recs, _ := audit.New(e.db).List(ctx, 0, 1000)
	for _, r := range recs {
		if r.Action == "admin.login.code_locked" {
			locked++
		}
	}
	if locked != 1 {
		t.Fatalf("code lock audited %d times", locked)
	}
	// The lock ends by itself, and `can-core admin unlock --username` ends it at once.
	if _, err := e.db.ClearLoginAttempts(ctx, "root"); err != nil {
		t.Fatal(err)
	}
	if err := login(e.code(t)); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}

func TestCodeLockEndsByItself(t *testing.T) {
	e := guardEnv(t, false)
	ctx := context.Background()
	login := func(code string) error {
		_, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: code, IP: "10.42.0.7"})
		return err
	}
	// The lock starts just before the counting window ends: that must not lift it.
	for range accountFreeAttempts + 1 {
		_ = login(e.wrongCode(t))
	}
	*e.now = e.now.Add(store.LoginGuardWindow - time.Minute)
	for range accountCodeFailures - 1 {
		_ = login(e.wrongCode(t))
	}
	*e.now = e.now.Add(2 * time.Minute) // a new window; the lock still runs
	if err := login(e.code(t)); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("lock lifted with the window: %v", err)
	}
	*e.now = e.now.Add(DefaultPolicy.LockFor)
	if err := login(e.code(t)); err != nil {
		t.Fatalf("lock did not end: %v", err)
	}
}

// Nobody without the password can lock the owner out, however much they send and from wherever.
func TestWrongPasswordsNeverLockTheOwnerOut(t *testing.T) {
	for _, hard := range []bool{false, true} {
		t.Run(fmt.Sprint("hardLock=", hard), func(t *testing.T) {
			e := guardEnv(t, hard)
			ctx := context.Background()
			for i := range 100 {
				code := "000000"
				if i%2 == 0 {
					code = e.code(t) // even a right code, seen over a shoulder
				}
				ip := "10.42.0.7"
				if hard {
					ip = fmt.Sprintf("198.51.100.%d", i)
				}
				_, err := e.svc.Login(ctx, LoginRequest{Username: "ROOT", Password: "not the password", Code: code, IP: ip})
				if !errors.Is(err, ErrInvalidCredentials) {
					t.Fatalf("attempt %d: %v", i, err)
				}
			}
			if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "203.0.113.9"}); err != nil {
				t.Fatalf("owner refused: %v", err)
			}
			var n int
			_ = e.db.QueryRow(`SELECT COUNT(*) FROM login_attempts WHERE key IN ('login:root','login-code:root')`).Scan(&n)
			if n != 0 {
				t.Fatalf("a success left %d account counters", n)
			}
		})
	}
}

// Behind the proxy the attempt is counted before it is checked: requests sent together get no more
// than the limit.
func TestHardLockCountsBeforeChecking(t *testing.T) {
	e := guardEnv(t, true)
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

// A success gives its reservation back: the address's count is of failures only, and attempts that
// compared nothing (hashing busy) do not count either.
func TestHardLockSuccessAndBusyDoNotCount(t *testing.T) {
	e := guardEnv(t, true)
	ctx := context.Background()
	failures := func(key string) int {
		a, err := e.db.GetLoginAttempt(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return a.Failures
	}
	for range DefaultPolicy.MaxIPFailures + 5 {
		*e.now = e.now.Add(30 * time.Second) // a fresh code
		if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "192.0.2.2"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := failures("ip:192.0.2.2"); n != 0 {
		t.Fatalf("successes counted against the address: %d", n)
	}
	e.svc.opt.HashWait = time.Millisecond
	var releases []func()
	for range cap(e.svc.slots) {
		r, err := e.svc.acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, r)
	}
	*e.now = e.now.Add(30 * time.Second)
	for range 10 {
		if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "192.0.2.2"}); !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	}
	for _, r := range releases {
		r()
	}
	if u, ip := failures("user:root|192.0.2.2"), failures("ip:192.0.2.2"); u != 0 || ip != 0 {
		t.Fatalf("busy attempts counted: %d %d", u, ip)
	}
}
