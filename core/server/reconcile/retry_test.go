package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// A failed upgrade may already have changed the cluster (routes pruned before the rollout, Secrets
// rewritten). Reverting company.yaml to the recorded state must then still be an upgrade, or those
// changes stay for ever.
func TestRevertingAfterAFailedUpgradeIsStillAnUpgrade(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	m := mf("work", "1", nil)
	withExposure := func(public bool) Desired {
		d := desired(nil, nil, m)
		e := d.Company.Modules["work"]
		e.Settings = map[string]any{"public": public} // stands for any change that changes the hash
		d.Company.Modules = map[string]config.ModuleEntry{"work": e}
		return d
	}
	apply := func(d Desired) Result {
		t.Helper()
		res, err := g.r.Apply(ctx, d, Options{})
		if err != nil && g.fk.FailEnable["work"] == nil {
			t.Fatal(err)
		}
		return res
	}
	apply(withExposure(true))
	g.fk.FailEnable["work"] = errors.New("rollout timed out")
	apply(withExposure(false))
	rec, err := g.db.GetModule(ctx, "work")
	if err != nil || rec.State != store.StateEnabled || rec.LastError == "" {
		t.Fatalf("a failed upgrade keeps the module enabled with the error: %+v %v", rec, err)
	}
	// reverted while the cluster is still broken: planned, fails, planned again
	for range 2 {
		if got := kinds(apply(withExposure(true)).Actions); got != "upgrade:work" {
			t.Fatalf("after reverting: %q, want upgrade:work", got)
		}
	}
	delete(g.fk.FailEnable, "work")
	if got := kinds(apply(withExposure(true)).Actions); got != "upgrade:work" {
		t.Fatalf("the retry: %q", got)
	}
	if got := kinds(apply(withExposure(true)).Actions); got != "" {
		t.Fatalf("after a successful retry nothing is left to do: %q", got)
	}
}
