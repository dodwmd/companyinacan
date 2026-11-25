// Package contract holds cross-package contract tests: every fixture and every
// shipped example must validate (or fail) exactly as expected.
package contract

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/validate"
)

const root = "../.."

type expectation struct {
	Problems []struct {
		Rule     string `yaml:"rule"`
		Path     string `yaml:"path"`
		Severity string `yaml:"severity"`
	} `yaml:"problems"`
}

func validateFile(t *testing.T, file string) []problem.Problem {
	t.Helper()
	modulesDir := filepath.Join(root, "testdata", "modules")
	switch {
	case strings.HasPrefix(filepath.Base(file), "company-"):
		return validate.CompanyFile(file, modulesDir)
	case strings.HasPrefix(filepath.Base(file), "module-"):
		return validate.ModuleFile(file)
	}
	t.Fatalf("fixture %s must start with company- or module-", file)
	return nil
}

func validateCompany(file, modulesDir string) []problem.Problem {
	return validate.CompanyFile(file, modulesDir)
}

func validateModule(file string) []problem.Problem { return validate.ModuleFile(file) }

func keys(ps []problem.Problem) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p.Severity)+" "+p.Rule+" "+p.Path)
	}
	sort.Strings(out)
	return out
}

func TestValidFixtures(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(root, "testdata", "valid", "*.yaml"))
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			if ps := validateFile(t, f); len(ps) != 0 {
				t.Fatalf("expected no problems, got:\n%s", join(ps))
			}
		})
	}
}

func TestInvalidFixtures(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(root, "testdata", "invalid", "*.yaml"))
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, ".expect.yaml") {
			continue
		}
		n++
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(strings.TrimSuffix(f, ".yaml") + ".expect.yaml")
			if err != nil {
				t.Fatalf("missing sidecar expectation: %v", err)
			}
			var exp expectation
			if err := yaml.Unmarshal(raw, &exp); err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, p := range exp.Problems {
				sev := p.Severity
				if sev == "" {
					sev = "error"
				}
				want = append(want, sev+" "+p.Rule+" "+p.Path)
			}
			sort.Strings(want)
			ps := validateFile(t, f)
			for _, p := range ps {
				if p.File == "" || p.Message == "" || p.Rule == "" {
					t.Errorf("problem lacks file/rule/message: %+v", p)
				}
			}
			got := keys(ps)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("problems differ\n got:\n  %s\nwant:\n  %s\nfull:\n%s", strings.Join(got, "\n  "), strings.Join(want, "\n  "), join(ps))
			}
		})
	}
	if n < 20 {
		t.Fatalf("expected at least 20 invalid fixtures, found %d", n)
	}
}

func join(ps []problem.Problem) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString("  " + p.String() + "\n")
	}
	return b.String()
}
