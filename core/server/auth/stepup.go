package auth

import (
	"context"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// stepUpAttempts is how many wrong confirmations one session may make before it is ended.
const stepUpAttempts = 5

// Reauthenticate confirms the password and a fresh code of a signed-in local user and records it on
// the session (the step-up before sensitive changes).
//
// Wrong attempts are counted per session, never per account or address: after stepUpAttempts the
// session ends (ErrUnauthenticated) and the person signs in again, so nobody holding a stolen
// session can lock the real user out of anything.
func (s *Service) Reauthenticate(ctx context.Context, sess store.Session, u store.AdminUser, password, code, ip string) error {
	if u.AuthSource != store.SourceLocal || sess.Source == store.SourceSSO {
		return ErrInvalidCredentials // single sign-on users confirm at their identity provider
	}
	now := s.opt.Now()
	key := "reauth:" + store.PublicSessionID(sess)
	// The attempt is counted, atomically, before anything is verified. The count decides, not the
	// moment the lock was set, so requests running side by side cannot outrun the limit.
	att, _, err := s.db.RegisterFailure(ctx, key, stepUpAttempts+1, s.opt.Policy.Window, s.opt.Policy.LockFor, now)
	if err != nil {
		return err
	}
	if att.Failures > stepUpAttempts {
		if err := s.db.DeleteSession(ctx, sess.IDHash); err != nil {
			return err
		}
		s.recordBestEffort(ctx, u.Username, "admin.reauth.session_ended", ip)
		return ErrUnauthenticated
	}
	step, ok := ValidateTOTP(u.TOTPSecret, code, now)
	if !ok || len(password) > MaxPasswordLen {
		s.recordBestEffort(ctx, u.Username, "admin.reauth.failed", ip)
		return ErrInvalidCredentials
	}
	if ok, err = s.verify(ctx, password, u.PasswordHash); err != nil {
		return err
	}
	if ok {
		if ok, err = s.db.AdvanceTOTPStep(ctx, u.ID, step); err != nil { // single use
			return err
		}
	}
	if !ok {
		s.recordBestEffort(ctx, u.Username, "admin.reauth.failed", ip)
		return ErrInvalidCredentials
	}
	_ = s.db.ClearLoginAttempt(ctx, key)
	if _, err := s.audit.Append(ctx, audit.Entry{Actor: u.Username, Action: "admin.reauth", Target: "admin", Detail: map[string]any{"ip": ip}}); err != nil {
		return err // an unrecorded step-up is not granted
	}
	return s.db.SetSessionReauth(ctx, sess.IDHash, now)
}

// NewPasswordHash hashes a new password under the same concurrency bound as sign-in.
func (s *Service) NewPasswordHash(ctx context.Context, password string) (string, error) {
	if len(password) > MaxPasswordLen {
		return "", ErrWeakPassword
	}
	return s.hash(ctx, password)
}

// CheckPassword verifies a password against a stored hash under the same concurrency bound.
func (s *Service) CheckPassword(ctx context.Context, password, hash string) (bool, error) {
	if len(password) > MaxPasswordLen {
		return false, nil
	}
	return s.verify(ctx, password, hash)
}
