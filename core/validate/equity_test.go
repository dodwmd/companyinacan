package validate

import (
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/problem"
)

// equity's directory mode is asked for in company.yaml, never assumed, and never left silently.
func TestEquityIdentitySettings(t *testing.T) {
	company := func(modules string) []problem.Problem {
		return CompanyBytes("company.yaml", []byte("schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nmodules:\n"+modules), "../../modules")
	}
	find := func(ps []problem.Problem, rule string) (problem.Problem, bool) {
		for _, p := range ps {
			if p.Rule == rule {
				return p, true
			}
		}
		return problem.Problem{}, false
	}
	soon := time.Now().AddDate(0, 0, 2).UTC().Format("2006-01-02")
	late := time.Now().AddDate(0, 0, 30).UTC().Format("2006-01-02")
	for name, c := range map[string]struct {
		modules, rule string
		severity      problem.Severity
	}{
		"people disabled":         {"  equity: {settings: {identity: directory}}\n  people: {enabled: false}\n", "semantic.equity-directory", problem.Error},
		"people absent":           {"  equity: {settings: {identity: directory}}\n", "semantic.equity-directory", problem.Error},
		"mistyped identity":       {"  equity: {settings: {identity: directroy}}\n  people: {}\n", "semantic.equity-settings", problem.Error},
		"suggestion":              {"  equity: {}\n  people: {}\n", "semantic.equity-identity", problem.Warning},
		"break-glass too far":     {"  equity: {settings: {break_glass_until: \"" + late + "\"}}\n", "semantic.equity-break-glass", problem.Error},
		"break-glass not a date":  {"  equity: {settings: {break_glass_until: always}}\n", "semantic.equity-break-glass", problem.Error},
		"directory with people":   {"  equity: {settings: {identity: directory}}\n  people: {}\n", "", ""},
		"attestation alone":       {"  equity: {}\n", "", ""},
		"unquoted date":           {"  equity: {settings: {break_glass_until: " + soon + "}}\n", "", ""},
		"a past window":           {"  equity: {settings: {break_glass_until: \"2020-01-01\"}}\n", "", ""},
		"equity disabled with it": {"  equity: {enabled: false, settings: {identity: directory}}\n", "", ""},
	} {
		ps := company(c.modules)
		if c.rule == "" {
			for _, p := range ps {
				if strings.HasPrefix(p.Rule, "semantic.equity") {
					t.Errorf("%s: %s", name, p.String())
				}
			}
			continue
		}
		if p, ok := find(ps, c.rule); !ok || p.Severity != c.severity {
			t.Errorf("%s: want %s %s, got %v", name, c.severity, c.rule, ps)
		}
	}
}
