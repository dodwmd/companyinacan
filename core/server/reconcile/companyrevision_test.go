package reconcile

import (
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// A renamed company or a new domain must reach the modules that carry them; other modules must
// not be re-applied for it.
func TestCompanyFactsChangeTheSpecHash(t *testing.T) {
	man := &config.Module{Version: "1"}
	hash := func(name, domain, id string) string {
		return specHash(&config.Company{Name: name, Domain: domain, Modules: map[string]config.ModuleEntry{id: {}}}, id, man, "", "")
	}
	for _, id := range []string{"jobs", "files"} {
		if hash("A", "a.example", id) == hash("B", "a.example", id) || hash("A", "a.example", id) == hash("A", "b.example", id) {
			t.Errorf("%s: a changed company fact is not an upgrade", id)
		}
	}
	if hash("A", "a.example", "work") != hash("B", "b.example", "work") {
		t.Error("work does not carry company facts")
	}
}
