package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRegisterFailureLocksAtomicallyAndExpires(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	now := time.Unix(10_000, 0)
	var locked bool
	for i := 1; i <= 3; i++ {
		a, l, err := db.RegisterFailure(ctx, "k", 3, time.Minute, time.Hour, now)
		if err != nil || a.Failures != i {
			t.Fatalf("%d: %+v %v", i, a, err)
		}
		locked = l
	}
	if !locked {
		t.Fatal("third failure did not report a new lock")
	}
	if _, l, _ := db.RegisterFailure(ctx, "k", 3, time.Minute, time.Hour, now.Add(time.Second)); l {
		t.Fatal("a failure during an existing lock must not re-announce it")
	}
	a, l, _ := db.RegisterFailure(ctx, "k", 3, time.Minute, time.Hour, now.Add(2*time.Hour))
	if a.Failures != 1 || l {
		t.Fatalf("after lock expiry: %+v %v", a, l)
	}
	_, _, _ = db.RegisterFailure(ctx, "w", 3, time.Minute, time.Hour, now)
	if a, _, _ := db.RegisterFailure(ctx, "w", 3, time.Minute, time.Hour, now.Add(2*time.Minute)); a.Failures != 1 {
		t.Fatalf("after window: %+v", a)
	}
}

func TestClearAndPruneLoginAttempts(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	now := time.Unix(10_000, 0)
	for _, k := range []string{"user:root|1.1.1.1", "user:root|2.2.2.2", "user:bob|1.1.1.1", "ip:1.1.1.1"} {
		_, _, _ = db.RegisterFailure(ctx, k, 5, time.Minute, time.Hour, now)
	}
	if n, _ := db.ClearLoginAttempts(ctx, "root"); n != 2 {
		t.Fatalf("cleared %d", n)
	}
	if n, _ := db.PruneLoginAttempts(ctx, now.Add(time.Hour), time.Minute); n != 2 {
		t.Fatalf("pruned %d", n)
	}
	_, _, _ = db.RegisterFailure(ctx, "x", 5, time.Minute, time.Hour, now)
	if n, _ := db.ClearLoginAttempts(ctx, ""); n != 1 {
		t.Fatalf("cleared %d", n)
	}
}

func TestSetAdminCredentialsKillsSessions(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	u, _ := db.CreateAdmin(ctx, "root", "old", "S1")
	now := time.Unix(1000, 0)
	_ = db.CreateSession(ctx, Session{IDHash: "a", UserID: u.ID, CSRF: "c", IP: "i", CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	_, _ = db.AdvanceTOTPStep(ctx, u.ID, 50)
	if err := db.SetAdminCredentials(ctx, u.ID, "new", "S2"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.AdminByID(ctx, u.ID)
	if got.PasswordHash != "new" || got.TOTPSecret != "S2" {
		t.Fatalf("%+v", got)
	}
	if _, err := db.SessionByHash(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatal("session survived credential reset")
	}
	if ok, _ := db.AdvanceTOTPStep(ctx, u.ID, 1); !ok {
		t.Fatal("replay guard not reset with a new seed")
	}
}
