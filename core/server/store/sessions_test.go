package store

import (
	"context"
	"testing"
	"time"
)

func TestListAndRevokeSessionsByPublicID(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	u1, _ := db.CreateAdmin(ctx, "a", "h", "s")
	u2, _ := db.CreateAdmin(ctx, "b", "h", "s")
	now := time.Unix(1_700_000_000, 0)
	mk := func(id string, uid int64, ttl time.Duration) {
		if err := db.CreateSession(ctx, Session{IDHash: id, UserID: uid, CSRF: "c", IP: "1.2.3.4", CreatedAt: now, ExpiresAt: now.Add(ttl)}); err != nil {
			t.Fatal(err)
		}
	}
	mk("aaaaaaaaaaaaaaaa1111", u1.ID, time.Hour)
	mk("bbbbbbbbbbbbbbbb2222", u1.ID, time.Hour)
	mk("cccccccccccccccc3333", u1.ID, -time.Hour) // expired: not listed
	mk("dddddddddddddddd4444", u2.ID, time.Hour)

	ss, err := db.ListSessions(ctx, u1.ID, now)
	if err != nil || len(ss) != 2 {
		t.Fatalf("%v %+v", err, ss)
	}
	if PublicSessionID(ss[0]) != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("public id %q", PublicSessionID(ss[0]))
	}
	// Another user's session cannot be revoked through this user's id.
	if ok, err := db.RevokeSession(ctx, u1.ID, "dddddddddddddddd"); err != nil || ok {
		t.Fatalf("cross-user revoke: %v %v", ok, err)
	}
	if ok, err := db.RevokeSession(ctx, u1.ID, "aaaaaaaaaaaaaaaa"); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if ss, _ = db.ListSessions(ctx, u1.ID, now); len(ss) != 1 {
		t.Fatalf("%+v", ss)
	}
	// Prefixes that are too short never match.
	if ok, _ := db.RevokeSession(ctx, u1.ID, "b"); ok {
		t.Fatal("short prefix revoked a session")
	}
}
