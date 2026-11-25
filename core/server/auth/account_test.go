package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/audit"
)

func login(t *testing.T, e *env) LoginResult {
	t.Helper()
	res, err := e.svc.Login(context.Background(), LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// freshCode returns a code for the next TOTP step (login consumed the current one).
func freshCode(t *testing.T, e *env) string {
	t.Helper()
	*e.now = e.now.Add(31 * time.Second)
	return e.code(t)
}

func TestChangePasswordChecksCurrentEndsSessionsAndAudits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := login(t, e)
	_, u, _ := e.svc.Authenticate(ctx, res.Token)
	c := freshCode(t, e)

	if err := e.svc.ChangePassword(ctx, u, "wrong current pw", c, "another long password", "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current: %v", err)
	}
	if err := e.svc.ChangePassword(ctx, u, e.pw, "000000", "another long password", "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong code: %v", err)
	}
	if err := e.svc.ChangePassword(ctx, u, e.pw, "", "another long password", "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("missing code: %v", err)
	}
	if err := e.svc.ChangePassword(ctx, u, e.pw, c, "short", "10.0.0.1"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak: %v", err)
	}
	// the weak attempt above consumed the code; a replay of it is refused
	if err := e.svc.ChangePassword(ctx, u, e.pw, c, "another long password", "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("replayed code: %v", err)
	}
	c = freshCode(t, e)
	if err := e.svc.ChangePassword(ctx, u, e.pw, c, "another long password", "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Authenticate(ctx, res.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("session survived a password change: %v", err)
	}
	nu, _ := e.db.AdminByID(ctx, u.ID)
	if ok, _ := VerifyPassword("another long password", nu.PasswordHash); !ok {
		t.Fatal("new password not stored")
	}
	recs, _ := audit.New(e.db).List(ctx, 0, 50)
	found := false
	for _, r := range recs {
		found = found || r.Action == "admin.password.change"
	}
	if !found {
		t.Fatalf("no audit entry: %+v", recs)
	}
}

func TestChangePasswordIsThrottled(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := login(t, e)
	_, u, _ := e.svc.Authenticate(ctx, res.Token)
	var locked *LockedError
	for i := 0; i < 10 && !errors.As(e.svc.ChangePassword(ctx, u, "wrong current pw", "000000", "another long password", "1.1.1.1"), &locked); i++ {
	}
	if locked == nil {
		t.Fatal("repeated wrong attempts were never throttled")
	}
	// Even correct credentials are refused while locked.
	c := freshCode(t, e)
	if err := e.svc.ChangePassword(ctx, u, e.pw, c, "another long password", "1.1.1.1"); !errors.As(err, &locked) {
		t.Fatalf("%v", err)
	}
	*e.now = e.now.Add(time.Hour)
	c = freshCode(t, e)
	if err := e.svc.ChangePassword(ctx, u, e.pw, c, "another long password", "1.1.1.1"); err != nil {
		t.Fatalf("after lock expiry: %v", err)
	}
}

func TestAccountAttemptsAreCountedBeforeVerification(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := login(t, e)
	_, u, _ := e.svc.Authenticate(ctx, res.Token)
	_ = e.svc.ConfirmPassword(ctx, u, "wrong current pw")
	a, _ := e.db.GetLoginAttempt(ctx, "acct:root")
	if a.Failures != 1 {
		t.Fatalf("failures=%d", a.Failures)
	}
	if err := e.svc.ConfirmPassword(ctx, u, e.pw); err != nil {
		t.Fatal(err)
	}
	if a, _ = e.db.GetLoginAttempt(ctx, "acct:root"); a.Failures != 0 {
		t.Fatalf("success must clear the count, failures=%d", a.Failures)
	}
}

func TestRotateTOTPRequiresPasswordCurrentCodeAndProofOfNewSecret(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := login(t, e)
	_, u, _ := e.svc.Authenticate(ctx, res.Token)

	secret := NewTOTPSecret()
	key, _ := DecodeTOTPSecret(secret)
	c := freshCode(t, e)
	good := Code(key, SHA1, 6, 30, *e.now)
	if err := e.svc.RotateTOTP(ctx, u, "wrong current pw", c, secret, good, "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if err := e.svc.RotateTOTP(ctx, u, e.pw, "000000", secret, good, "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current code: %v", err)
	}
	if err := e.svc.RotateTOTP(ctx, u, e.pw, c, secret, "000000", "1.1.1.1"); !errors.Is(err, ErrBadTOTPCode) {
		t.Fatalf("wrong new code: %v", err)
	}
	c = freshCode(t, e)
	good = Code(key, SHA1, 6, 30, *e.now)
	if err := e.svc.RotateTOTP(ctx, u, e.pw, c, "not base32!", good, "1.1.1.1"); !errors.Is(err, ErrBadTOTPCode) {
		t.Fatalf("bad secret: %v", err)
	}
	c = freshCode(t, e)
	good = Code(key, SHA1, 6, 30, *e.now)
	if err := e.svc.RotateTOTP(ctx, u, e.pw, c, secret, good, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	nu, _ := e.db.AdminByID(ctx, u.ID)
	if nu.TOTPSecret != secret {
		t.Fatal("secret not replaced")
	}
	if _, _, err := e.svc.Authenticate(ctx, res.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("sessions must end when the second factor changes")
	}
}

func TestConfirmPasswordDoesNotChangeAnything(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := login(t, e)
	_, u, _ := e.svc.Authenticate(ctx, res.Token)
	if err := e.svc.ConfirmPassword(ctx, u, e.pw); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ConfirmPassword(ctx, u, "wrong current pw"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("%v", err)
	}
	if _, _, err := e.svc.Authenticate(ctx, res.Token); err != nil {
		t.Fatal("confirming a password must not end the session")
	}
}
