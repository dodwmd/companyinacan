package pat

import (
	"context"
	"testing"
	"time"
)

// A token never outlives its issuer's rights: permissions are re-checked at introspection.
func TestTokenLosesPermissionsTheIssuerNoLongerHolds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	held := map[string]bool{"work.read": true, "work.write": true}
	e.s.Holds = func(_ context.Context, _, p string) (bool, error) { return held[p], nil }
	iss := issue(t, e, "work", "work.read", "work.write")
	r, ok := e.s.Introspect(ctx, "work", iss.Token)
	if !ok || len(r.Permissions) != 2 {
		t.Fatalf("%+v %v", r, ok)
	}
	held["work.write"] = false // role removed
	r, ok = e.s.Introspect(ctx, "work", iss.Token)
	if !ok || len(r.Permissions) != 1 || r.Permissions[0] != "work.read" {
		t.Fatalf("a removed permission survived: %+v %v", r, ok)
	}
	held["work.read"] = false
	if _, ok := e.s.Introspect(ctx, "work", iss.Token); ok {
		t.Fatal("a token with no permission the issuer still holds is active")
	}
	// an error from the rights check fails closed
	e.s.Holds = func(context.Context, string, string) (bool, error) { return false, context.DeadlineExceeded }
	if _, ok := e.s.Introspect(ctx, "work", iss.Token); ok {
		t.Fatal("answered active although the rights check failed")
	}
}

func TestTokenOfADeletedUserIsInactive(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iss := issue(t, e, "work", "work.read")
	if _, ok := e.s.Introspect(ctx, "work", iss.Token); !ok {
		t.Fatal("inactive while the user exists")
	}
	if _, err := e.db.Exec(`DELETE FROM admin_users WHERE username='alice'`); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.s.Introspect(ctx, "work", iss.Token); ok {
		t.Fatal("a deleted user's token is still active")
	}
}

func TestMaxTTLIsConfigurableAndDefaultsTo90Days(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	req := IssueRequest{Name: "x", Module: "work", Permissions: []string{"work.read"}, TTL: 120 * 24 * time.Hour}
	if _, err := e.s.Issue(ctx, "user:alice", req); err == nil {
		t.Fatal("120 days accepted under the 90-day default")
	}
	req.TTL = 90 * 24 * time.Hour
	if _, err := e.s.Issue(ctx, "user:alice", req); err != nil {
		t.Fatal(err)
	}
	e.s.MaxTTL = 30 * 24 * time.Hour
	if _, err := e.s.Issue(ctx, "user:alice", req); err == nil {
		t.Fatal("the configured maximum was ignored")
	}
}

func TestPerIssuerCapOnLiveTokens(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for i := 0; i < MaxLivePerIssuer; i++ {
		if _, err := e.s.Issue(ctx, "user:alice", IssueRequest{Name: "t", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour}); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := e.s.Issue(ctx, "user:alice", IssueRequest{Name: "t", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour}); err == nil {
		t.Fatal("one issuer filled the module's quota")
	}
	if _, err := e.s.Issue(ctx, "user:bob", IssueRequest{Name: "t", Module: "work", Permissions: []string{"work.read"}, TTL: time.Hour}); err != nil {
		t.Fatalf("another issuer must still be able to issue: %v", err)
	}
}

// Failed lookups from one module are slowed past a quota; a valid token is never delayed.
func TestInvalidLookupsArePenalisedAndValidOnesNever(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var slept time.Duration
	e.s.FailFree, e.s.FailPenalty = 5, 50*time.Millisecond
	e.s.Sleep = func(_ context.Context, d time.Duration) { slept += d }
	iss := issue(t, e, "work", "work.read")
	bad := TokenPrefix + "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	for i := 0; i < 20; i++ {
		e.s.Introspect(ctx, "work", bad)
	}
	if slept != 15*50*time.Millisecond {
		t.Fatalf("slept %v: expected the 15 lookups past the free quota to be delayed", slept)
	}
	slept = 0
	for i := 0; i < 50; i++ {
		if _, ok := e.s.Introspect(ctx, "work", iss.Token); !ok {
			t.Fatal("valid token refused")
		}
	}
	if slept != 0 {
		t.Fatalf("a valid token was delayed by %v", slept)
	}
	// another module's failures are counted separately
	e.s.Introspect(ctx, "people", bad)
	if slept != 0 {
		t.Fatal("one module's failures slowed another")
	}
}
