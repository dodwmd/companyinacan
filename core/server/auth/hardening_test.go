package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
)

// An anonymous attacker must not be able to lock the real admin out by failing logins for "root"
// from some other address (admin lockout DoS).
func TestLockoutOfAttackerDoesNotLockRealAdmin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < DefaultPolicy.MaxUserFailures+3; i++ {
		_, _ = e.svc.Login(ctx, LoginRequest{Username: "root", Password: "wrong password!", Code: "000000", IP: "66.66.66.66"})
	}
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: "wrong password!", Code: "000000", IP: "66.66.66.66"}); !errors.As(err, new(*LockedError)) {
		t.Fatalf("attacker pair should be locked: %v", err)
	}
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "10.0.0.5"}); err != nil {
		t.Fatalf("real admin locked out by a stranger: %v", err)
	}
}

func TestPerIPRateLimitAppliesBeforeAnyHashing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pol := DefaultPolicy
	pol.HardLock = true
	e.svc = NewService(e.db, audit.New(e.db), Options{MinLoginTime: time.Nanosecond, Sleep: noSleep, Now: func() time.Time { return *e.now }, Policy: pol})
	var locked bool
	for i := 0; i < pol.AttemptsPerMin+5; i++ {
		// valid credentials too: the limiter does not look at them
		_, err := e.svc.Login(ctx, LoginRequest{Username: "u" + string(rune('a'+i)), Password: "whatever-long-pass", Code: "000000", IP: "5.5.5.5"})
		var le *LockedError
		if errors.As(err, &le) {
			locked = true
			if i < pol.AttemptsPerMin {
				t.Fatalf("limited too early at attempt %d", i)
			}
		}
	}
	if !locked {
		t.Fatal("no rate limit")
	}
	*e.now = e.now.Add(61 * time.Second)
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "5.5.5.5"}); errors.As(err, new(*LockedError)) && !strings.Contains(err.Error(), "retry") {
		t.Fatal(err)
	}
}

func TestHashingConcurrencyIsCapped(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc = NewService(e.db, audit.New(e.db), Options{MinLoginTime: time.Nanosecond, Sleep: noSleep, Now: func() time.Time { return *e.now }, HashSlots: 1, HashWait: 20 * time.Millisecond,
		Policy: Policy{HardLock: true, MaxUserFailures: 5, MaxIPFailures: 50, AttemptsPerMin: 1000, Window: time.Minute, LockFor: time.Minute}})
	release, err := e.svc.acquire(ctx) // occupy the only slot
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "1.1.1.1"})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("login ran argon2 beyond the cap: %v", err)
	}
	release()
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "1.1.1.1"}); err != nil {
		t.Fatalf("slot not released: %v", err)
	}
}

func TestParallelLoginsNeverExceedSlots(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.svc = NewService(e.db, audit.New(e.db), Options{MinLoginTime: time.Nanosecond, Sleep: noSleep, Now: func() time.Time { return *e.now }, HashSlots: 2, HashWait: 5 * time.Second,
		Policy: Policy{HardLock: true, MaxUserFailures: 500, MaxIPFailures: 500, AttemptsPerMin: 1000, Window: time.Minute, LockFor: time.Minute}})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.svc.Login(ctx, LoginRequest{Username: "root", Password: "wrong password!!", Code: "000000", IP: "2.2.2.2"})
		}()
	}
	wg.Wait()
	if len(e.svc.slots) != 0 || cap(e.svc.slots) != 2 {
		t.Fatalf("slots leaked: len=%d cap=%d", len(e.svc.slots), cap(e.svc.slots))
	}
}

func TestOversizedInputNeverReachesArgon2OrCreatesPerNameState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	release, _ := e.svc.acquire(ctx) // if argon2 were reached we'd get ErrBusy after HashWait
	defer release()
	e.svc.opt.HashWait = 10 * time.Millisecond
	for _, r := range []LoginRequest{
		{Username: strings.Repeat("a", MaxUsernameLen+1), Password: e.pw, Code: "000000", IP: "3.3.3.3"},
		{Username: "root", Password: strings.Repeat("p", MaxPasswordLen+1), Code: "000000", IP: "3.3.3.3"},
		{Username: "", Password: e.pw, Code: "000000", IP: "3.3.3.3"},
	} {
		if _, err := e.svc.Login(ctx, r); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("%.10q: %v", r.Username, err)
		}
	}
	var n int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM login_attempts WHERE key LIKE 'user:%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d per-name rows created by junk usernames", n)
	}
}

func TestLockIsAuditedOncePerLock(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < DefaultPolicy.MaxUserFailures+10; i++ {
		_, _ = e.svc.Login(ctx, LoginRequest{Username: "root", Password: "wrong password!", Code: "000000", IP: "4.4.4.4"})
	}
	recs, _ := audit.New(e.db).List(ctx, 0, 100)
	locks := 0
	for _, r := range recs {
		if r.Action == "admin.login.locked" {
			locks++
		}
	}
	if locks != 1 {
		t.Fatalf("lock audited %d times, want once; %d records", locks, len(recs))
	}
}

type failingAudit struct{}

func (failingAudit) Append(context.Context, audit.Entry) (audit.Record, error) {
	return audit.Record{}, errors.New("disk full")
}

func TestLoginFailsClosedWhenAuditCannotBeWritten(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var logs strings.Builder
	svc := NewService(e.db, failingAudit{}, Options{MinLoginTime: time.Nanosecond, Sleep: noSleep, Now: func() time.Time { return *e.now }, Log: slog.New(slog.NewJSONHandler(&logs, nil)),
		Policy: Policy{HardLock: true, MaxUserFailures: 5, MaxIPFailures: 20, AttemptsPerMin: 1000, Window: time.Minute, LockFor: time.Minute}})
	_, err := svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "1.1.1.1"})
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login succeeded or masked without an audit trail: %v", err)
	}
	var n int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Fatal("a session survived the failed audit")
	}
	// failure paths still answer normally but the error is logged, not swallowed
	_, _ = svc.Login(ctx, LoginRequest{Username: "root", Password: "wrong password!", Code: "000000", IP: "1.1.1.1"})
	if !strings.Contains(logs.String(), "audit append failed") {
		t.Fatalf("audit failure not logged: %q", logs.String())
	}
}
