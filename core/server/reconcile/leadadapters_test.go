package reconcile_test

import (
	"slices"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/validate"
)

// An installation from before the lead adapters is not given leads by an unattended upgrade: the
// shipped manifests widen it, so the upgrade waits for approval (specs/069-lead-adapters).
func TestLeadAdaptersWaitForApproval(t *testing.T) {
	for _, id := range []string{"support", "newsletter"} {
		md, ps := validate.LoadManifest("../../../modules/" + id + "/module.yaml")
		if md == nil {
			t.Fatal(ps)
		}
		next, err := config.DecodeModule(md)
		if err != nil {
			t.Fatal(err)
		}
		before := *next
		before.Subscribes = nil
		got := reconcile.Widening(&before, next)
		for _, want := range []string{"receives event lead.created.v1", "is sent the names, addresses and messages of website leads by can-core"} {
			if !slices.Contains(got, want) {
				t.Errorf("%s: %q not in %v", id, want, got)
			}
		}
	}
}
