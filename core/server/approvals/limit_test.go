package approvals

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// A module cannot open (or open and cancel) approvals without end, and every notice the engine sends
// is marked as the engine's own.
func TestAModuleOpensAtMostSoManyApprovals(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	open := func(key string) (Request, error) {
		r, _, err := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:emp", Amount: 1, IdempotencyKey: key})
		return r, err
	}
	for i := range MaxPerModuleHour {
		r, err := open("k" + strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := x.e.Cancel(ctx, r.ID, "module:"+exp, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := open("one-more"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("cancelled requests count too: %v", err)
	}
	if _, err := open("k0"); err != nil {
		t.Fatalf("a repeat of an earlier request is not a new one: %v", err)
	}
	if len(x.notes.notices) == 0 {
		t.Fatal("no notices")
	}
	for _, n := range x.notes.notices {
		if !n.Platform {
			t.Fatalf("not marked as the engine's: %+v", n)
		}
	}
	x.now = x.now.Add(time.Hour + time.Minute)
	if _, err := open("later"); err != nil {
		t.Fatalf("after an hour: %v", err)
	}
}
