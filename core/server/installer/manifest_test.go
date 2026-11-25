package installer

import (
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func loadManifest(t *testing.T, path string) *config.Module {
	t.Helper()
	doc, ps := config.Load(path)
	if doc == nil {
		t.Fatal(ps)
	}
	m, err := config.DecodeModule(doc)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
