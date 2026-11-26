package contract

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/licence"
)

// A version bump of one of these images must reach THIRD_PARTY_LICENSES too.
func TestThirdPartyImagesAreRecordedAsDeployed(t *testing.T) {
	v := readYAML(t, "modules/support/chart/values.yaml")
	img := asMap(v["image"])
	deployed := []string{
		asStr(img["repository"]) + ":" + asStr(img["tag"]) + "@" + asStr(img["digest"]),
	}
	pg := readFile(t, "deploy/templates/postgres.yaml")
	for _, line := range strings.Split(pg, "\n") {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(line), "image: "); ok {
			deployed = append(deployed, ref)
		}
	}
	if len(deployed) != 2 || len(licence.Images) != len(deployed) {
		t.Fatalf("deployed %v, recorded %d", deployed, len(licence.Images))
	}
	notice := readFile(t, "THIRD_PARTY_LICENSES")
	for i, ref := range deployed {
		if licence.Images[i].Ref != ref {
			t.Errorf("core/licence/images.go records %s, deployed is %s", licence.Images[i].Ref, ref)
		}
		if !strings.Contains(notice, ref) {
			t.Errorf("THIRD_PARTY_LICENSES lacks %s: run `make licences`", ref)
		}
	}
}
