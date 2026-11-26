package contract

import (
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// Least privilege (epic E06d): the modules that may connect to the internet are exactly these. A
// change here needs a reason in the module's manifest and in docs/security-model.md.
func TestOnlyTheseModulesReachTheInternet(t *testing.T) {
	var got []string
	for _, f := range glob(t, root, "modules/*/module.yaml") {
		doc, ps := config.Load(filepath.Join(root, f))
		if doc == nil {
			t.Fatal(ps)
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if m.Runtime.InternetEgress {
			got = append(got, m.ID)
		}
	}
	sort.Strings(got)
	want := []string{"crm", "newsletter", "provisioning", "status", "support"}
	if !slices.Equal(got, want) {
		t.Fatalf("internet_egress: %v, want %v", got, want)
	}
}
