package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
)

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return nil
}

func (s *sleeps) max() (m time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.d {
		if d > m {
			m = d
		}
	}
	return
}

// The default deployment: CAN_TRUST_PROXY is off, so every client shares the ingress's address and
// there is no hard lock. An attacker hammering junk logins (even for the admin's own username)
// must slow the admin down at most, never lock the admin out.
func sharedIPEnv(t *testing.T) (*env, *sleeps) {
	e := newEnv(t)
	sl := &sleeps{}
	e.svc = NewService(e.db, audit.New(e.db), Options{
		Now: func() time.Time { return *e.now }, Policy: DefaultPolicy, // HardLock off
		MinLoginTime: time.Nanosecond, Sleep: sl.sleep,
	})
	return e, sl
}

func TestSharedIPAttackerCannotLockOutAdmin(t *testing.T) {
	e, sl := sharedIPEnv(t)
	ctx := context.Background()
	const ingress = "10.42.0.7" // every client looks like Traefik
	for i := 0; i < 500; i++ {
		if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: "guess-number-" + string(rune('a'+i%26)), Code: "000000", IP: ingress}); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("junk login %d: %v (must be an ordinary failure, never a refusal)", i, err)
		}
	}
	res, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: ingress})
	if err != nil || res.Token == "" {
		t.Fatalf("admin with valid credentials and TOTP was refused while under attack: %v", err)
	}
	if m := sl.max(); m <= 0 || m > DefaultPolicy.BackoffMax {
		t.Fatalf("expected a bounded back-off delay, got %v", m)
	}
}

func TestJunkLoginsNeverTouchArgon2(t *testing.T) {
	e, _ := sharedIPEnv(t)
	ctx := context.Background()
	e.svc.opt.HashWait = 5 * time.Millisecond
	release, err := e.svc.acquire(ctx) // the only slot is taken: any argon2 use would give ErrBusy
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for i := 0; i < 50; i++ {
		_, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: "000000", IP: "10.42.0.7"})
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("a login with a wrong TOTP code reached argon2 or was refused: %v", err)
		}
	}
	// and the attacker's cost does not delay an admin who has a valid code and a free slot
	release()
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "10.42.0.7"}); err != nil {
		t.Fatal(err)
	}
	release, _ = e.svc.acquire(ctx)
}

func TestWrongTOTPAndValidLoginTakeTheSameFloorTime(t *testing.T) {
	e, _ := sharedIPEnv(t)
	sl := &sleeps{}
	e.svc.opt.Sleep, e.svc.opt.MinLoginTime = sl.sleep, MinLoginTime
	ctx := context.Background()
	_, _ = e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: "000000", IP: "1.1.1.1"})
	_, _ = e.svc.Login(ctx, LoginRequest{Username: "nobody", Password: e.pw, Code: "000000", IP: "1.1.1.1"})
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	if len(sl.d) != 3 || sl.d[0] != MinLoginTime || sl.d[1] != MinLoginTime || sl.d[2] != MinLoginTime {
		t.Fatalf("login durations are distinguishable: %v", sl.d)
	}
}

func TestBackoffGrowsAndIsBounded(t *testing.T) {
	e, _ := sharedIPEnv(t)
	p := DefaultPolicy
	prev := time.Duration(0)
	for f := p.MaxUserFailures; f < p.MaxUserFailures+4; f++ {
		d := e.svc.backoff(f, p.MaxUserFailures)
		if d <= prev {
			t.Fatalf("failures=%d: %v not above %v", f, d, prev)
		}
		prev = d
	}
	if e.svc.backoff(p.MaxUserFailures-1, p.MaxUserFailures) != 0 || e.svc.backoff(10_000, p.MaxUserFailures) != p.BackoffMax {
		t.Fatal("below-threshold must be free and the cap must hold")
	}
}

func TestWaitingLoginsAreBounded(t *testing.T) {
	e, _ := sharedIPEnv(t)
	ctx := context.Background()
	gate := make(chan struct{})
	e.svc.opt.Sleep = func(ctx context.Context, d time.Duration) error {
		if d >= DefaultPolicy.BackoffBase {
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
		return nil
	}
	for i := 0; i < DefaultPolicy.MaxIPFailures+1; i++ { // push the address into back-off
		_, _, _ = e.db.RegisterFailure(ctx, "ip:9.9.9.9", DefaultPolicy.MaxIPFailures, time.Hour, time.Hour, *e.now)
	}
	var wg sync.WaitGroup
	busy := make(chan error, maxSleepers+50)
	for i := 0; i < maxSleepers+50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Login(ctx, LoginRequest{Username: "x", Password: "y", Code: "000000", IP: "9.9.9.9"})
			if errors.Is(err, ErrBusy) {
				busy <- err
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(busy) < 50 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(gate)
	wg.Wait()
	if len(busy) < 50 {
		t.Fatalf("only %d logins were turned away; waiting logins are unbounded", len(busy))
	}
}
