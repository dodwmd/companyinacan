package sso

import (
	"context"
	"errors"
	"testing"
)

// A disabled account cannot sign in through the identity provider either.
func TestDisabledAccountCannotSignInWithSSO(t *testing.T) {
	e := newEnv(t, nil)
	r := e.mustLogin(alice())
	_, user, err := e.au.Authenticate(context.Background(), r.Login.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ExecContext(context.Background(), `UPDATE admin_users SET disabled_at=1 WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.login(alice())
	var d *Denied
	if !errors.As(err, &d) || d.Code != "disabled" {
		t.Fatalf("sign-in of a disabled account: %v", err)
	}
}
