package installer

import (
	"context"
	"slices"
	"testing"

	"github.com/dodwmd/companyinacan/core/forms"
)

// The module setting notify_email reaches every form of the rendered forms.yaml (E03d).
func TestFormsChartRendersNotifyEmail(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir = "../../../modules"
	spec.ID, spec.Namespace, spec.ServiceAccount = "forms", "mod-forms", "forms"
	spec.Manifest = loadManifest(t, "../../../modules/forms/module.yaml")
	spec.Settings = map[string]any{"notify_email": []any{"sales@acme.example", "owner@acme.example"}}
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	raw, _ := dig(api.Objects["ConfigMap/mod-forms/forms-config"], "data", "forms.yaml").(string)
	cfg, err := forms.ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if got := cfg.Forms[0].NotifyEmail; !slices.Equal(got, []string{"sales@acme.example", "owner@acme.example"}) {
		t.Fatalf("notify_email %v\n%s", got, raw)
	}
}
