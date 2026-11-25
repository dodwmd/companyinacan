package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func openMem(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAPITokenLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	now := time.Unix(1_000_000, 0)
	db.Now = func() time.Time { return now }
	tok := APIToken{ID: "pat_1", Name: "ci", ModuleID: "work", Subject: "user:alice", Permissions: []string{"work.read"}, TokenHash: "h1", CreatedBy: "user:alice", ExpiresAt: now.Add(time.Hour)}
	if err := db.InsertAPIToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := db.APITokenByHash(ctx, "h1")
	if err != nil || got.ModuleID != "work" || len(got.Permissions) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := db.APITokenByHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hash: %v", err)
	}
	if list, err := db.ListAPITokens(ctx); err != nil || len(list) != 1 {
		t.Fatalf("%v %v", list, err)
	}
	if err := db.RevokeAPIToken(ctx, "pat_1"); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.APITokenByHash(ctx, "h1"); got.RevokedAt.IsZero() {
		t.Fatal("not revoked")
	}
	if err := db.RevokeAPIToken(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking an unknown token: %v", err)
	}
}

func TestInsertEventOnceDeduplicates(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	e := Event{ID: "evt_1", Type: "a.b.v1", Time: time.Unix(10, 0), Source: "forms", DataJSON: `{}`}
	id, dup, err := db.InsertEventOnce(ctx, e, "src-1", nil)
	if err != nil || dup || id != "evt_1" {
		t.Fatalf("%s %v %v", id, dup, err)
	}
	e2 := e
	e2.ID = "evt_2"
	id, dup, err = db.InsertEventOnce(ctx, e2, "src-1", nil)
	if err != nil || !dup || id != "evt_1" {
		t.Fatalf("second publish with the same source id: %s %v %v", id, dup, err)
	}
	// the same source id from another module is a different key
	e3 := e2
	e3.Source = "jobs"
	if id, dup, err = db.InsertEventOnce(ctx, e3, "src-1", nil); err != nil || dup || id != "evt_2" {
		t.Fatalf("%s %v %v", id, dup, err)
	}
}

func TestSecretRotationsAreConsumedOnce(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	if err := db.RequestRotation(ctx, "people", "keyring", "user:alice"); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestRotation(ctx, "people", "keyring", "user:alice"); err != nil {
		t.Fatal("asking twice is idempotent: ", err)
	}
	keys, err := db.PendingRotations(ctx, "people")
	if err != nil || len(keys) != 1 || keys[0] != "keyring" {
		t.Fatalf("%v %v", keys, err)
	}
	if err := db.ClearRotations(ctx, "people", keys); err != nil {
		t.Fatal(err)
	}
	if keys, _ = db.PendingRotations(ctx, "people"); len(keys) != 0 {
		t.Fatalf("%v", keys)
	}
}
