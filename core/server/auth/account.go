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

// checkCurrent verifies the user's current password and, when needCode is set, a current TOTP code
// (single use, like at login). The attempt is counted before any verification starts, in one
// transaction (RegisterFailure), so parallel guesses cannot slip past the limit; a success clears
// the count. Cheapest check first: a wrong code never reaches argon2.
func (s *Service) checkCurrent(ctx context.Context, u store.AdminUser, current, code string, needCode bool) error {
	return s.checkCurrentOpt(ctx, u, current, code, needCode, true)
}

// checkCurrentOpt is checkCurrent with control over clearing the failure count on success.
func (s *Service) checkCurrentOpt(ctx context.Context, u store.AdminUser, current, code string, needCode, clear bool) error {
	if u.AuthSource != store.SourceLocal {
		return ErrInvalidCredentials // single sign-on users have no password to confirm
	}
	now := s.opt.Now()
	key := accountKey(u)
	a, locked, err := s.db.RegisterFailure(ctx, key, accountMaxFailures, s.opt.Policy.Window, accountLockFor, now)
	if err != nil {
		return err
	}
	if locked {
		s.recordBestEffort(ctx, u.Username, "admin.account.locked", "")
	} else if now.Before(a.LockedUntil) {
		return &LockedError{RetryAfter: a.LockedUntil.Sub(now)} // was already locked
	}
	if len(current) > MaxPasswordLen {
		return ErrInvalidCredentials
	}
	var step int64
	if needCode {
		var ok bool
		if step, ok = ValidateTOTP(u.TOTPSecret, code, now); !ok {
			return ErrInvalidCredentials
		}
	}
	ok, err := s.verify(ctx, current, u.PasswordHash)
	if err != nil { // nothing was compared
		s.release(ctx, []reservation{{key: key, max: accountMaxFailures}})
		return err
	}
	if !ok {
		return ErrInvalidCredentials
	}
	if needCode {
		if fresh, err := s.db.AdvanceTOTPStep(ctx, u.ID, step); err != nil {
			return err
		} else if !fresh {
			return ErrInvalidCredentials // replayed code
		}
	}
	if clear {
		_ = s.db.ClearLoginAttempt(ctx, key)
	}
	return nil
}

// ConfirmPasswordAndCode re-authenticates for a destructive action: the current password and a fresh
// single-use authentication code. A success does not clear the shared account failure count, so this
// cannot be used to reset the throttle that guards password changes.
func (s *Service) ConfirmPasswordAndCode(ctx context.Context, u store.AdminUser, current, code string) error {
	return s.checkCurrentOpt(ctx, u, current, code, true, false)
}

func (s *Service) auditAccount(ctx context.Context, u store.AdminUser, action, ip string) error {
	_, err := s.audit.Append(ctx, audit.Entry{Actor: u.Username, Action: action, Target: "admin", Detail: map[string]any{"ip": ip}})
	return err
}

// ConfirmPassword verifies the current password under the account throttle, changing nothing.
func (s *Service) ConfirmPassword(ctx context.Context, u store.AdminUser, current string) error {
	return s.checkCurrent(ctx, u, current, "", false)
}

// ChangePassword replaces the user's password after verifying the current one. All of the user's
// sessions end (the caller must log in again).
func (s *Service) ChangePassword(ctx context.Context, u store.AdminUser, current, code, next, ip string) error {
	if err := s.checkCurrent(ctx, u, current, code, true); err != nil {
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
func (s *Service) RotateTOTP(ctx context.Context, u store.AdminUser, current, code, newSecret, newCode, ip string) error {
	if err := s.checkCurrent(ctx, u, current, code, true); err != nil {
		return err
	}
	if _, ok := ValidateTOTP(newSecret, newCode, s.opt.Now()); !ok {
		return ErrBadTOTPCode
	}
	if err := s.db.SetAdminCredentials(ctx, u.ID, u.PasswordHash, newSecret); err != nil {
		return err
	}
	return s.auditAccount(ctx, u, "admin.totp.rotate", ip)
}
