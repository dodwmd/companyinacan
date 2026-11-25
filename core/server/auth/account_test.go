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

func TestChangePasswordChecksCurrentEndsSessionsAndAudits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := login(t, e)
	_, u, _ := e.svc.Authenticate(ctx, res.Token)

	if err := e.svc.ChangePassword(ctx, u, "wrong current pw", "another long password", "10.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current: %v", err)
	}
	if err := e.svc.ChangePassword(ctx, u, e.pw, "short", "10.0.0.1"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak: %v", err)
	}
	if err := e.svc.ChangePassword(ctx, u, e.pw, "another long password", "10.0.0.1"); err != nil {
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
	for i := 0; i < 10 && !errors.As(e.svc.ChangePassword(ctx, u, "wrong current pw", "another long password", "1.1.1.1"), &locked); i++ {
	}
	if locked == nil {
		t.Fatal("repeated wrong current passwords were never throttled")
	}
	// Even the right password is refused while locked.
	if err := e.svc.ChangePassword(ctx, u, e.pw, "another long password", "1.1.1.1"); !errors.As(err, &locked) {
		t.Fatalf("%v", err)
	}
	*e.now = e.now.Add(time.Hour)
	if err := e.svc.ChangePassword(ctx, u, e.pw, "another long password", "1.1.1.1"); err != nil {
		t.Fatalf("after lock expiry: %v", err)
	}
}

func TestRotateTOTPRequiresPasswordAndProofOfNewSecret(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := login(t, e)
	_, u, _ := e.svc.Authenticate(ctx, res.Token)

	secret := NewTOTPSecret()
	key, _ := DecodeTOTPSecret(secret)
	good := Code(key, SHA1, 6, 30, *e.now)

	if err := e.svc.RotateTOTP(ctx, u, "wrong current pw", secret, good, "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if err := e.svc.RotateTOTP(ctx, u, e.pw, secret, "000000", "1.1.1.1"); !errors.Is(err, ErrBadTOTPCode) {
		t.Fatalf("wrong code: %v", err)
	}
	if err := e.svc.RotateTOTP(ctx, u, e.pw, "not base32!", good, "1.1.1.1"); !errors.Is(err, ErrBadTOTPCode) {
		t.Fatalf("bad secret: %v", err)
	}
	if err := e.svc.RotateTOTP(ctx, u, e.pw, secret, good, "1.1.1.1"); err != nil {
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

func TestConfirmPasswordThrottledAndDoesNotChangeAnything(t *testing.T) {
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
