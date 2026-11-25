package pat

import (
	"context"
	"testing"
)

// Defence in depth: a token whose issuer was disabled is dead even if nothing revoked it.
func TestDisabledIssuerTokensAreDead(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iss := issue(t, e, "work", "work.read")
	if _, ok := e.s.Introspect(ctx, "work", iss.Token); !ok {
		t.Fatal("fresh token refused")
	}
	if _, err := e.db.ExecContext(ctx, `UPDATE admin_users SET disabled_at=1 WHERE username='alice'`); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.s.Introspect(ctx, "work", iss.Token); ok {
		t.Fatal("a token of a disabled account introspected as active")
	}
}
