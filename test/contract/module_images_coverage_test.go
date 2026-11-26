package contract

import (
	"path/filepath"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modimages"
)

// Every in-house module installs from an image `make images` builds.
func TestMakeImagesCoversEveryInHouseModule(t *testing.T) {
	built := map[string]bool{}
	for _, n := range modimages.Names() {
		built[modimages.Ref(n)] = true
	}
	seen := 0
	for _, f := range glob(t, root, "modules/[a-z]*/module.yaml") {
		doc, ps := config.Load(filepath.Join(root, f))
		if doc == nil {
			t.Fatal(ps)
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if m.Provider.Type != "inhouse" {
			continue
		}
		seen++
		refs, err := modimages.ForModule(filepath.Join(root, "modules"), m.ID)
		if err != nil || len(refs) == 0 {
			t.Errorf("%s: no images (%v): is its chart missing?", m.ID, err)
			continue
		}
		for _, r := range refs {
			if !built[r] {
				t.Errorf("%s pulls %s, which make images does not build", m.ID, r)
			}
		}
	}
	if seen < 10 {
		t.Fatalf("only %d in-house modules", seen)
	}
}
