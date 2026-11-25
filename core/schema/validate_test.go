package schema

import (
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func load(t *testing.T, src string) *config.Doc {
	t.Helper()
	d, ps := config.LoadBytes("t.yaml", []byte(src))
	if len(ps) != 0 {
		t.Fatalf("load: %v", ps)
	}
	return d
}

func TestCompanyMinimalValid(t *testing.T) {
	d := load(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\n")
	if ps := Validate(Company, d); len(ps) != 0 {
		t.Fatalf("unexpected: %v", ps)
	}
}

func TestMissingRequiredPointsAtParentWithLine(t *testing.T) {
	d := load(t, "schema: 1\nname: A\nprovider: {type: local}\n")
	ps := Validate(Company, d)
	if len(ps) != 1 || ps[0].Rule != "schema.required" || ps[0].Path != "/domain" || ps[0].Line != 1 {
		t.Fatalf("unexpected: %+v", ps)
	}
}

func TestUnknownFieldPointsAtKey(t *testing.T) {
	d := load(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nextra: 1\n")
	ps := Validate(Company, d)
	if len(ps) != 1 || ps[0].Rule != "schema.unknown-field" || ps[0].Line != 5 || ps[0].Column != 1 {
		t.Fatalf("unexpected: %+v", ps)
	}
}

func TestAllProblemsReportedTogether(t *testing.T) {
	d := load(t, "schema: 3\nname: \"\"\ndomain: BAD\nprovider: {type: nope}\nx: 1\n")
	ps := Validate(Company, d)
	if len(ps) < 5 {
		t.Fatalf("expected >=5 problems, got %d: %+v", len(ps), ps)
	}
}
