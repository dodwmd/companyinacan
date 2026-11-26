package ticket

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

func TestSummaryGrantIsSingleUseBoundToOneModuleAndShortLived(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.hold("user:alice", "work.read")

	tok, err := e.s.MintSummary(ctx, "user:alice", "work")
	if err != nil || !modclient.SummaryTokenShaped(tok) {
		t.Fatalf("mint: %q %v", tok, err)
	}
	// Another module can neither read nor spend it.
	if _, ok := e.s.RedeemSummary(ctx, "people", tok); ok {
		t.Fatal("another module redeemed the grant")
	}
	r, ok := e.s.RedeemSummary(ctx, "work", tok)
	if !ok || r.Subject != "user:alice" || !slices.Equal(r.Permissions, []string{"work.read", "work.write"}) {
		t.Fatalf("redeem: %+v %v", r, ok)
	}
	if _, ok := e.s.RedeemSummary(ctx, "work", tok); ok {
		t.Fatal("redeemed twice")
	}

	tok, _ = e.s.MintSummary(ctx, "user:alice", "work")
	e.now = e.now.Add(SummaryGrantTTL)
	if _, ok := e.s.RedeemSummary(ctx, "work", tok); ok {
		t.Fatal("an expired grant was redeemed")
	}
	tok, _ = e.s.MintSummary(ctx, "user:alice", "work")
	e.s.DropSummary(tok)
	if _, ok := e.s.RedeemSummary(ctx, "work", tok); ok {
		t.Fatal("a dropped grant was redeemed")
	}
	for _, junk := range []string{"", "cansum_short", "canpat_" + tok[len("cansum_"):], tok + "x"} {
		if _, ok := e.s.RedeemSummary(ctx, "work", junk); ok {
			t.Errorf("redeemed %q", junk)
		}
	}
}

func TestSummaryGrantFollowsStanding(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// No permission in the module, a disabled module, an unknown person: no grant at all.
	for _, c := range [][2]string{{"user:bob", "work"}, {"user:nobody", "work"}, {"user:alice", "jobs"}, {"user:alice", "ghost"}} {
		if _, err := e.s.MintSummary(ctx, c[0], c[1]); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%v: %v", c, err)
		}
	}
	// A right removed or an account disabled between mint and redeem answers nothing.
	e.hold("user:alice", "work.read")
	e.hold("user:bob", "work.read")
	a, _ := e.s.MintSummary(ctx, "user:alice", "work")
	b, _ := e.s.MintSummary(ctx, "user:bob", "work")
	e.drop("user:alice", "work.read")
	e.drop("user:alice", "work.write")
	e.disable(t, "bob")
	if _, ok := e.s.RedeemSummary(ctx, "work", a); ok {
		t.Error("redeemed after the permission was removed")
	}
	if _, ok := e.s.RedeemSummary(ctx, "work", b); ok {
		t.Error("redeemed after the account was disabled")
	}
}

func TestSummaryGrantsAreBounded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.hold("user:alice", "work.read")
	for range maxGrants {
		if _, err := e.s.MintSummary(ctx, "user:alice", "work"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.MintSummary(ctx, "user:alice", "work"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("over the bound: %v", err)
	}
	e.now = e.now.Add(SummaryGrantTTL + time.Second)
	if _, err := e.s.MintSummary(ctx, "user:alice", "work"); err != nil {
		t.Fatalf("expired grants must make room: %v", err)
	}
}
