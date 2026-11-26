package approvals

import (
	"context"
	"errors"
	"testing"
)

// A default chain entry `a|b` is a step for a with b as its fallback, and a manager named by the
// directory provider stands for manager_of_requester (specs/070-approvals-inbox).
func TestDefaultChainFallbackAndNamedManager(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	open := func(key, requester, manager string, chain ...string) (Detail, error) {
		r, _, err := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: requester, IdempotencyKey: key, DefaultChain: chain, Manager: manager})
		if err != nil {
			return Detail{}, err
		}
		return x.e.Get(ctx, r.ID)
	}
	d, err := open("k1", "user:emp", "", "finance|owner")
	if err != nil || len(d.Steps) != 1 || len(d.Steps[0].Roles) != 1 || d.Steps[0].Roles[0] != exp+":finance" {
		t.Fatalf("the primary role: %+v %v", d.Steps, err)
	}
	// Nobody holds the role "nobody": the owners are asked.
	if err := x.rb.Grant(ctx, "can-core:cli", "user:boss", "owner", ""); err != nil {
		t.Fatal(err)
	}
	d, err = open("k2", "user:emp", "", "nobody|owner")
	if err != nil || len(d.Steps[0].Roles) != 1 || d.Steps[0].Roles[0] != "owner" {
		t.Fatalf("the fallback: %+v %v", d.Steps, err)
	}
	if _, err := open("k3", "user:boss", "", "nobody|owner"); !errors.Is(err, ErrNoApprover) {
		t.Fatalf("the only owner cannot be their own approver: %v", err)
	}
	d, err = open("k4", "user:emp", "user:mgr", "manager|finance")
	if err != nil || len(d.Steps[0].Users) != 1 || d.Steps[0].Users[0] != "user:mgr" || len(d.Steps[0].Roles) != 0 {
		t.Fatalf("the named manager: %+v %v", d.Steps, err)
	}
	// A manager who is the requester does not count.
	d, err = open("k5", "user:emp", "user:emp", "manager|finance")
	if err != nil || len(d.Steps[0].Roles) != 1 || d.Steps[0].Roles[0] != exp+":finance" {
		t.Fatalf("own manager: %+v %v", d.Steps, err)
	}
	if _, err := open("k6", "user:emp", "not a subject", "manager"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad manager: %v", err)
	}
	// The same key with another manager is another request.
	if _, err := open("k4", "user:emp", "user:other", "manager|finance"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("idempotency: %v", err)
	}
}
