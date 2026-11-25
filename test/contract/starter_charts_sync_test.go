package contract

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The shared templates are copied into each chart (charts are embedded in the can-core image, so
// there is no library chart); this keeps the copies from drifting.
func TestSharedChartTemplatesIdentical(t *testing.T) {
	for _, f := range []string{"_helpers.tpl", "service.yaml", "ingressroute.yaml", "pvc.yaml", "networkpolicy.yaml", "adapter.yaml"} {
		ref, err := os.ReadFile(filepath.Join(root, "modules", "support", "chart", "templates", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range starters[1:] {
			got, err := os.ReadFile(filepath.Join(root, "modules", s.id, "chart", "templates", f))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(ref, got) {
				t.Errorf("modules/%s/chart/templates/%s differs from the support copy", s.id, f)
			}
		}
	}
}

// Every shipped module (not just the starters) must validate under module.v1.
func TestEveryModuleManifestValidates(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(root, "modules", "*", "module.yaml"))
	if len(files) < 5 {
		t.Fatalf("found only %d manifests", len(files))
	}
	for _, f := range files {
		if ps := onlyUnexpected(validateModule(f)); len(ps) != 0 {
			t.Errorf("%s:\n%s", f, join(ps))
		}
	}
}
