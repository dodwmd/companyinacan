// Package licence evaluates wrapped-app licences and generates THIRD_PARTY_LICENSES.
package licence

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/schemas"
)

// Accepted is the set of SPDX ids accepted for wrapped third-party apps.
type Accepted struct{ ids map[string]bool }

// Has reports whether the SPDX id is accepted (case-insensitive).
func (a Accepted) Has(id string) bool { return a.ids[strings.ToLower(id)] }

type acceptedFile struct {
	Schema   int `yaml:"schema"`
	Accepted []struct {
		ID string `yaml:"id"`
	} `yaml:"accepted"`
}

func parseAccepted(data []byte) (Accepted, error) {
	var f acceptedFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return Accepted{}, err
	}
	if f.Schema != 1 {
		return Accepted{}, fmt.Errorf("unsupported accepted-licences schema %d (want 1)", f.Schema)
	}
	a := Accepted{ids: map[string]bool{}}
	for _, e := range f.Accepted {
		a.ids[strings.ToLower(e.ID)] = true
	}
	return a, nil
}

var (
	defOnce sync.Once
	defAcc  Accepted
)

// DefaultAccepted returns the list embedded from schemas/accepted-licences.yaml.
func DefaultAccepted() Accepted {
	defOnce.Do(func() {
		a, err := parseAccepted(schemas.AcceptedLicences)
		if err != nil {
			panic("schemas/accepted-licences.yaml: " + err.Error()) // embedded data, covered by tests
		}
		defAcc = a
	})
	return defAcc
}

// LoadAccepted reads an alternative accepted-licence file.
func LoadAccepted(path string) (Accepted, error) {
	b, err := os.ReadFile(path) //nolint:gosec // user-provided path
	if err != nil {
		return Accepted{}, err
	}
	return parseAccepted(b)
}
