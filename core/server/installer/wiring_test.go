package installer

import (
	"context"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// A people module installed afresh has a new directory token: the fingerprint changes, which makes
// the reconciler apply provisioning again, and that copies the new one.
func TestProvisioningFollowsAReinstalledPeopleModule(t *testing.T) {
	var _ reconcile.WiringAware = (*Installer)(nil)
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir = "../../../modules"
	s := spec
	s.ID, s.Namespace, s.ServiceAccount = "provisioning", "mod-provisioning", "provisioning"
	s.Manifest = loadManifest(t, "../../../modules/provisioning/module.yaml")
	s.Manifest.Requires.Database = "none"
	print := func() string {
		t.Helper()
		f, err := in.WiringFingerprint(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if print() != "" {
		t.Fatal("a fingerprint without a people module")
	}
	setToken := func(v string) {
		if err := api.Apply(context.Background(), secret("mod-people", secretModule, "people", map[string]string{modsecrets.PeopleDirectoryKey: v})); err != nil {
			t.Fatal(err)
		}
	}
	setToken("first-token-first-token-first-token")
	first := print()
	if err := in.Enable(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	setToken("second-token-second-token-second-tok")
	if first == "" || print() == first {
		t.Fatal("a new token is not noticed")
	}
	if err := in.Enable(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := secretValue(t, api, s.Namespace, modsecrets.ProvisioningDirectoryKey); got != "second-token-second-token-second-tok" {
		t.Errorf("provisioning still holds %q", got)
	}
}
