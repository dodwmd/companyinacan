package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/problem"
)

func hasRule(ps []problem.Problem, rule string) bool {
	for _, p := range ps {
		if p.Rule == rule {
			return true
		}
	}
	return false
}

func writeCompany(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(f, []byte("schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// Company validation refuses to enable a module whose manifest says not-deployable.
func TestCompanyRefusesNotDeployableModules(t *testing.T) {
	for _, id := range []string{"crm", "support"} {
		ps := CompanyFile(writeCompany(t, "modules:\n  "+id+": {}\n"), "../../modules")
		if !hasRule(ps, "semantic.module-not-deployable") || !problem.HasErrors(ps) {
			t.Errorf("%s: enabling a not-deployable module was not refused: %v", id, ps)
		}
	}
	// disabled is fine: the manifest is not even loaded
	ps := CompanyFile(writeCompany(t, "modules:\n  crm: {enabled: false}\n"), "../../modules")
	if hasRule(ps, "semantic.module-not-deployable") {
		t.Errorf("a disabled module was refused: %v", ps)
	}
}

// A primary must be an enabled module that SERVES the capability; a planned provision does not.
func TestCompanyPrimaryAtPlannedProviderIsInvalid(t *testing.T) {
	ps := CompanyFile(writeCompany(t, "modules:\n  analytics: {}\nprimary:\n  analytics: analytics\n"), "../../modules")
	if !hasRule(ps, "semantic.primary-invalid") {
		t.Fatalf("primary pointing at a planned provider accepted: %v", ps)
	}
}

// A subscription with no service needs a SERVED provision to default to; planned ones do not count.
func TestSubscribeServiceIgnoresPlannedProvisions(t *testing.T) {
	src, err := os.ReadFile("../../modules/_example/module.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	noSvc := "{ event: lead.created.v1, webhook: /hooks/lead, optional: true }"
	s = strings.Replace(s, "{ event: lead.created.v1, service: adapter, port: 8081, webhook: /hooks/lead, optional: true }", noSvc, 1)
	if !strings.Contains(s, noSvc) {
		t.Fatal("fixture changed")
	}
	check := func(s string) []problem.Problem {
		dir := filepath.Join(t.TempDir(), "example")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, "module.yaml")
		if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return ModuleFile(f)
	}
	if hasRule(check(s), "semantic.subscribe-service") {
		t.Fatal("a module with a served provision may default the subscription service")
	}
	planned := strings.Replace(s, "service: adapter, port: 8081 }", "service: adapter, port: 8081, status: planned }", 1)
	if planned == s {
		t.Fatal("fixture changed")
	}
	if !hasRule(check(planned), "semantic.subscribe-service") {
		t.Error("a module whose only provision is planned must name the subscription service")
	}
}
