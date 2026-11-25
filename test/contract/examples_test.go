package contract

import (
	"path/filepath"
	"testing"
)

// The shipped examples must always validate, so they cannot drift from the schemas.
func TestShippedExamplesValidate(t *testing.T) {
	modules := filepath.Join(root, "modules")
	if ps := validateCompany(filepath.Join(root, "examples", "acme-co", "company.yaml"), modules); len(ps) != 0 {
		t.Fatalf("examples/acme-co: %s", join(ps))
	}
	if ps := validateModule(filepath.Join(root, "modules", "_example", "module.yaml")); len(ps) != 0 {
		t.Fatalf("modules/_example: %s", join(ps))
	}
}
