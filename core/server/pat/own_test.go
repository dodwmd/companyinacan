package pat

import (
	"context"
	"errors"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestRevokeAndListAreOwnUnlessAdmin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	iss := issue(t, e, "work", "work.read")
	// Another administrator without the manage right does not even learn that the token exists.
	if err := e.s.Revoke(ctx, "user:bob", iss.ID, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob revoked alice's token: %v", err)
	}
	if got, _ := e.s.ListFor(ctx, "user:bob", false); len(got) != 0 {
		t.Fatalf("bob sees %d of alice's tokens", len(got))
	}
	if got, _ := e.s.ListFor(ctx, "user:alice", false); len(got) != 1 {
		t.Fatalf("alice sees %d tokens", len(got))
	}
	if got, _ := e.s.ListFor(ctx, "user:bob", true); len(got) != 1 {
		t.Fatalf("a manager sees %d tokens", len(got))
	}
	if err := e.s.Revoke(ctx, "user:bob", iss.ID, true); err != nil {
		t.Fatal(err)
	}
}
