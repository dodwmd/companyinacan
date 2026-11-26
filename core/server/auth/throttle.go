package auth

import (
	"context"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/server/store"
)

// reservation is a failure counted before the credentials were compared (HardLock), so that
// attempts sent side by side get no more than the limit. It is given back on success, or when
// nothing was compared.
type reservation struct {
	key    string
	max    int
	locked bool // this reservation established the lock
}

// reserve counts an attempt against each key, refusing with LockedError when one is already locked.
// A refused attempt compares nothing and gives back all it counted.
func (s *Service) reserve(ctx context.Context, keys []reservation, now time.Time) ([]reservation, error) {
	for i := range keys {
		a, locked, err := s.db.RegisterFailure(ctx, keys[i].key, keys[i].max, s.opt.Policy.Window, s.opt.Policy.LockFor, now)
		if err != nil {
			s.release(ctx, keys[:i])
			return nil, err
		}
		if !locked && now.Before(a.LockedUntil) {
			s.release(ctx, keys[:i+1])
			return nil, &LockedError{RetryAfter: a.LockedUntil.Sub(now)}
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

// failedAlert is how many failed sign-ins on one account within store.LoginFailedWindow are
// reported.
const failedAlert = 20

// noteFailure counts a failed sign-in against the account and reports, once per window, that there
// have been many. It only detects: nothing is ever refused or slowed because of this count.
//
// username is the account's name, or "" when there is none: those share one counter that is never
// reported, so the state stays bounded by the number of accounts and a failure costs the same
// whether the name exists or not.
func (s *Service) noteFailure(ctx context.Context, username, ip string, now time.Time) {
	_, reached, err := s.db.RegisterFailure(ctx, store.LoginFailedKey+strings.ToLower(username), failedAlert, store.LoginFailedWindow, store.LoginFailedWindow, now)
	if err != nil {
		s.opt.Log.Error("counting failed sign-ins failed", "error", err)
		return
	}
	if reached && username != "" {
		s.opt.Log.Warn("many failed sign-ins on an account within a day; nothing is refused because of this. Check the audit log, and change the password if the attempts are not yours", "username", username, "failures", failedAlert)
		s.recordBestEffort(ctx, username, "admin.login.many_failures", ip)
	}
}
