package auth

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/store"
)

// The per-account guard on sign-in (specs/005-can-core-mvp/research.md R5).
//
// The code is normally checked before the password, so junk costs no argon2. That order also lets
// someone who knows the password try codes as fast as requests arrive: a wrong code is answered
// without looking at the password, so it cannot be told from junk. The guard therefore:
//
//  1. counts every attempt on an account, before anything is compared; a success clears the count.
//     Up to accountFreeAttempts (in flight or failed) per window the cheap order is used;
//  2. beyond that, checks the password first. A wrong password is an ordinary failure and counts
//     against nothing else, so nobody without the password can lock the owner out;
//  3. with the right password, counts the attempt before the code's outcome is used. After
//     accountCodeFailures of those the account takes no code for Policy.LockFor.
//
// Every refusal is ErrInvalidCredentials: a different answer would confirm the password.
const (
	accountFreeAttempts = 10
	accountCodeFailures = 5
)

type accountGuard struct {
	passwordFirst bool
	attemptsKey   string
	codeKey       string
}

// guardAccount records the attempt and says in which order to check it.
func (s *Service) guardAccount(ctx context.Context, username string, now time.Time) (accountGuard, error) {
	name := strings.ToLower(username)
	g := accountGuard{attemptsKey: store.LoginAccountKey + name, codeKey: store.LoginCodeKey + name}
	a, _, err := s.db.RegisterFailure(ctx, g.attemptsKey, math.MaxInt32, s.opt.Policy.Window, s.opt.Policy.LockFor, now)
	g.passwordFirst = a.Failures > accountFreeAttempts
	return g, err
}

// takeCodeAttempt is called once the password is known to be right and before the code's outcome is
// used. It reports whether the code may be taken, and whether this attempt is the one that locks.
func (s *Service) takeCodeAttempt(ctx context.Context, g accountGuard, now time.Time) (allowed, locking bool, err error) {
	if !g.passwordFirst {
		a, err := s.db.GetLoginAttempt(ctx, g.codeKey)
		return err == nil && !now.Before(a.LockedUntil), false, err
	}
	a, locked, err := s.db.RegisterFailure(ctx, g.codeKey, accountCodeFailures, s.opt.Policy.Window, s.opt.Policy.LockFor, now)
	return err == nil && (locked || !now.Before(a.LockedUntil)), locked, err
}

func (s *Service) clearGuard(ctx context.Context, g accountGuard) {
	_ = s.db.ClearLoginAttempt(ctx, g.attemptsKey)
	_ = s.db.ClearLoginAttempt(ctx, g.codeKey)
}

// reservation is a failure counted before the credentials were compared (HardLock), so that
// attempts sent side by side get no more than the limit. It is given back on success, or when
// nothing was compared.
type reservation struct {
	key    string
	max    int
	locked bool // this reservation established the lock
}

// reserve counts an attempt against each key, refusing with LockedError when one is already locked.
func (s *Service) reserve(ctx context.Context, keys []reservation, now time.Time) ([]reservation, error) {
	for i := range keys {
		a, locked, err := s.db.RegisterFailure(ctx, keys[i].key, keys[i].max, s.opt.Policy.Window, s.opt.Policy.LockFor, now)
		if err == nil && !locked && now.Before(a.LockedUntil) {
			err = &LockedError{RetryAfter: a.LockedUntil.Sub(now)}
		}
		if err != nil {
			s.release(ctx, keys[:i])
			return nil, err
		}
		keys[i].locked = locked
	}
	return keys, nil
}

func (s *Service) release(ctx context.Context, rs []reservation) {
	for _, r := range rs {
		if err := s.db.ReleaseFailure(context.WithoutCancel(ctx), r.key, r.max); err != nil {
			s.opt.Log.Error("releasing a login attempt failed", "error", err)
		}
	}
}
