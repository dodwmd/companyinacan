package licence

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func mod(id, typ, lic, selected, class string) *config.Module {
	m := &config.Module{ID: id}
	m.Provider.Type = typ
	m.Provider.Name = "App " + id
	m.Provider.Licence = lic
	m.Provider.LicenceSelected = selected
	m.Provider.LicenceClass = class
	m.Provider.Upstream = "https://example.com/" + id
	return m
}

func TestEvaluate(t *testing.T) {
	acc := DefaultAccepted()
	tests := []struct {
		name     string
		m        *config.Module
		wantRule string // "" = pass
		wantSA   bool
	}{
		{"accepted id", mod("a", "oss-wrapper", "MIT", "", ""), "", false},
		{"accepted agpl", mod("a", "oss-wrapper", "AGPL-3.0-only", "", ""), "", false},
		{"case-insensitive", mod("a", "oss-wrapper", "apache-2.0", "", ""), "", false},
		{"missing", mod("a", "oss-wrapper", "", "", ""), "licence.missing", false},
		{"unknown id", mod("a", "oss-wrapper", "Elastic-2.0", "", ""), "licence.not-accepted", false},
		{"source-available marked", mod("a", "oss-wrapper", "Elastic-2.0", "", "source-available"), "", true},
		{"or both accepted", mod("a", "oss-wrapper", "MIT OR Apache-2.0", "", ""), "", false},
		{"or one accepted no selection", mod("a", "oss-wrapper", "Apache-2.0 OR Elastic-2.0", "", ""), "licence.expression-ambiguous", false},
		{"or one accepted selected", mod("a", "oss-wrapper", "Apache-2.0 OR Elastic-2.0", "Apache-2.0", ""), "", false},
		{"or selected not accepted", mod("a", "oss-wrapper", "Apache-2.0 OR Elastic-2.0", "Elastic-2.0", ""), "licence.not-accepted", false},
		{"or selected not in expression", mod("a", "oss-wrapper", "Apache-2.0 OR Elastic-2.0", "MIT", ""), "licence.expression-ambiguous", false},
		{"or none accepted", mod("a", "oss-wrapper", "Elastic-2.0 OR BUSL-1.1", "", ""), "licence.not-accepted", false},
		{"and both accepted", mod("a", "oss-wrapper", "MIT AND BSD-3-Clause", "", ""), "", false},
		{"and one not accepted", mod("a", "oss-wrapper", "MIT AND Elastic-2.0", "", ""), "licence.not-accepted", false},
		{"parens", mod("a", "oss-wrapper", "(MIT OR Apache-2.0) AND ISC", "", ""), "", false},
		{"with exception", mod("a", "oss-wrapper", "GPL-2.0-only WITH Classpath-exception-2.0", "", ""), "", false},
		{"garbage expression", mod("a", "oss-wrapper", "MIT OR", "", ""), "licence.invalid-expression", false},
		{"inhouse skipped", mod("a", "inhouse", "", "", ""), "", false},
		{"external needs licence", mod("a", "external", "", "", ""), "licence.missing", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := Evaluate(tt.m, acc, "m.yaml")
			if tt.wantRule == "" {
				if len(res.Problems) != 0 {
					t.Fatalf("want pass, got %v", res.Problems)
				}
				if res.SourceAvailable != tt.wantSA {
					t.Fatalf("SourceAvailable=%v want %v", res.SourceAvailable, tt.wantSA)
				}
				return
			}
			if len(res.Problems) != 1 || res.Problems[0].Rule != tt.wantRule {
				t.Fatalf("want %s, got %v", tt.wantRule, res.Problems)
			}
			if !strings.Contains(res.Problems[0].Message, "a") {
				t.Errorf("message should name the module: %q", res.Problems[0].Message)
			}
		})
	}
}

func TestAcceptedListIsReviewableData(t *testing.T) {
	acc := DefaultAccepted()
	for _, id := range []string{"Apache-2.0", "MIT", "AGPL-3.0-only", "MPL-2.0"} {
		if !acc.Has(id) {
			t.Errorf("%s should be accepted", id)
		}
	}
	if acc.Has("Elastic-2.0") || acc.Has("BUSL-1.1") {
		t.Error("source-available licences must not be on the accepted list")
	}
}
