package auth

import (
	"context"
	"errors"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Account changes need the current password. Wrong attempts are counted per user (not per
// address: the caller already holds a session) so a stolen session cannot be used to guess the
// password offline-fast: 5 failures lock account changes for 15 minutes.
const (
	accountMaxFailures = 5
	accountLockFor     = 15 * time.Minute
)

// ErrBadTOTPCode is returned by RotateTOTP when the current password was right but the code does
// not match the proposed secret (so the caller can let the user retry without enumerating anything).
var ErrBadTOTPCode = errors.New("code does not match the new secret")

func accountKey(u store.AdminUser) string { return "acct:" + u.Username }

// checkCurrent verifies the user's current password under the throttle.
func (s *Service) checkCurrent(ctx context.Context, u store.AdminUser, current string) error {
	now := s.opt.Now()
	key := accountKey(u)
	a, err := s.db.GetLoginAttempt(ctx, key)
	if err != nil {
		return err
	}
	if now.Before(a.LockedUntil) {
		return &LockedError{RetryAfter: a.LockedUntil.Sub(now)}
	}
	if len(current) > MaxPasswordLen {
		return ErrInvalidCredentials
	}
	ok, err := s.verify(ctx, current, u.PasswordHash)
	if err != nil {
		return err
	}
	if !ok {
		if _, locked, err := s.db.RegisterFailure(ctx, key, accountMaxFailures, s.opt.Policy.Window, accountLockFor, now); err != nil {
			return err
		} else if locked {
			s.recordBestEffort(ctx, u.Username, "admin.account.locked", "")
		}
		return ErrInvalidCredentials
	}
	_ = s.db.ClearLoginAttempt(ctx, key)
	return nil
}

func (s *Service) auditAccount(ctx context.Context, u store.AdminUser, action, ip string) error {
	_, err := s.audit.Append(ctx, audit.Entry{Actor: u.Username, Action: action, Target: "admin", Detail: map[string]any{"ip": ip}})
	return err
}

// ConfirmPassword verifies the current password under the account throttle, changing nothing.
func (s *Service) ConfirmPassword(ctx context.Context, u store.AdminUser, current string) error {
	return s.checkCurrent(ctx, u, current)
}

// ChangePassword replaces the user's password after verifying the current one. All of the user's
// sessions end (the caller must log in again).
func (s *Service) ChangePassword(ctx context.Context, u store.AdminUser, current, next, ip string) error {
	if err := s.checkCurrent(ctx, u, current); err != nil {
		return err
	}
	if len(next) > MaxPasswordLen {
		return ErrWeakPassword
	}
	h, err := s.hash(ctx, next) // also enforces the minimum length
	if err != nil {
		return err
	}
	if err := s.db.SetAdminCredentials(ctx, u.ID, h, ""); err != nil {
		return err
	}
	return s.auditAccount(ctx, u, "admin.password.change", ip)
}

// RotateTOTP replaces the TOTP secret after verifying the current password and that the caller
// can produce a valid code from the new secret (so a mistyped or unsaved secret cannot lock the
// admin out). All of the user's sessions end.
func (s *Service) RotateTOTP(ctx context.Context, u store.AdminUser, current, newSecret, code, ip string) error {
	if err := s.checkCurrent(ctx, u, current); err != nil {
		return err
	}
	if _, ok := ValidateTOTP(newSecret, code, s.opt.Now()); !ok {
		return ErrBadTOTPCode
	}
	if err := s.db.SetAdminCredentials(ctx, u.ID, u.PasswordHash, newSecret); err != nil {
		return err
	}
	return s.auditAccount(ctx, u, "admin.totp.rotate", ip)
}
