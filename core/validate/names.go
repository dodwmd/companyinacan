// Package validate runs schema and semantic validation for company and module files.
package validate

import (
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/schemas"
)

// Capability describes a known capability name.
type Capability struct {
	Name      string `yaml:"name"`
	Aggregate bool   `yaml:"aggregate"`
	Platform  bool   `yaml:"platform"`
}

var (
	namesOnce sync.Once
	names     map[string]Capability
)

// Capabilities returns the known capability names from schemas/names.yaml.
func Capabilities() map[string]Capability {
	namesOnce.Do(func() {
		var f struct {
			Capabilities []Capability `yaml:"capabilities"`
		}
		if err := yaml.Unmarshal(schemas.Names, &f); err != nil {
			panic("schemas/names.yaml: " + err.Error()) // embedded data, covered by tests
		}
		names = map[string]Capability{}
		for _, c := range f.Capabilities {
			names[c.Name] = c
		}
	})
	return names
}
