package installer

import (
	"strings"
	"testing"
)

// A Secret used as environment is read when the process starts, so a changed Secret must roll the
// pods; an unchanged one must not restart anything.
func TestChangedSecretRollsThePodsAndAnUnchangedOneDoesNot(t *testing.T) {
	enable, _, _, api := secretFixtureAPI(t)
	if err := enable(); err != nil {
		t.Fatal(err)
	}
	stamp := func() string {
		d := get(t, api, "Deployment", "mod-stub", "stub")
		a, _ := dig(d, "spec", "template", "metadata", "annotations").(map[string]any)
		s, _ := a["can.dev/secret-version"].(string)
		return s
	}
	first := stamp()
	if first == "" {
		t.Fatal("the pod template carries no secret version")
	}
	if err := enable(); err != nil {
		t.Fatal(err)
	}
	if got := stamp(); got != first {
		t.Fatal("re-enabling with unchanged secrets changed the pod template: it would restart the module for nothing")
	}
	if err := enable("CAN_STUB_TOKEN"); err != nil {
		t.Fatal(err)
	}
	second := stamp()
	if second == first || second == "" {
		t.Fatal("a rotated secret did not roll the pods")
	}
	if strings.Contains(second, "CAN_STUB") || len(second) > 40 {
		t.Fatalf("the version looks like secret material: %q", second)
	}
}
