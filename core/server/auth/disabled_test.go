package auth

import (
	"context"
	"errors"
	"testing"
)

// A disabled account (the person left) can neither sign in nor keep a session, and a refused
// login consumes nothing: the TOTP step is still usable once the account is enabled again.
func TestDisabledAccountCannotLoginOrKeepASession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: e.code(t), IP: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := e.db.AdminByUsername(ctx, "root")
	if _, err := e.db.ExecContext(ctx, `UPDATE admin_users SET disabled_at=1 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Authenticate(ctx, res.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an existing session of a disabled account: %v", err)
	}
	*e.now = e.now.Add(60 * 1e9) // a fresh TOTP step
	code := e.code(t)
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: code, IP: "10.0.0.1"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login of a disabled account: %v", err)
	}
	var last int64
	_ = e.db.QueryRow(`SELECT totp_last_step FROM admin_users WHERE id=?`, u.ID).Scan(&last)
	if last >= e.now.Unix()/30 {
		t.Fatal("a refused login consumed the TOTP step")
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE admin_users SET disabled_at=0 WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Login(ctx, LoginRequest{Username: "root", Password: e.pw, Code: code, IP: "10.0.0.1"}); err != nil {
		t.Fatalf("after enabling: %v", err)
	}
}
