package cli

// Public access (specs/045-public-access) in `can up` and `can status`, with fakes: no server, no
// DNS lookup and no provider call leaves the process.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/provider"
)

const exposedCompany = `schema: 1
name: Acme Co
domain: acme.example
provider:
  type: ssh
  options:
    host: 203.0.113.7
modules:
  work:
    expose: {ui: true}
  jobs: {}
  finance:
    expose: {public: true}
  people: {}
  forms: {}
`

func exposedSetup(t *testing.T, resolver fixedResolver) string {
	t.Helper()
	dir := writeCompany(t, exposedCompany)
	r := newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, resolver)
	testImagesDir2 = testImagesDir(t)
	return dir
}

func TestUpListsTheModuleHosts(t *testing.T) {
	dir := exposedSetup(t, goodResolver().(fixedResolver))
	useStack(t, false, stackResult(), nil)
	code, out, errs := run(t, stackArgs(dir)...)
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	for _, want := range []string{"work.acme.example", "careers.acme.example", "finance.acme.example", "work ui", "jobs pages", "finance invoice-link"} {
		if !strings.Contains(out, want) {
			t.Errorf("records lack %q:\n%s", want, out)
		}
	}
	// nothing exposed, nothing to publish
	if strings.Contains(out, "people.acme.example") || strings.Contains(out, "forms.acme.example") {
		t.Errorf("a record for a module that is not public:\n%s", out)
	}

	code, out, _ = run(t, "up", dir, "--plan", "--modules-dir", "../../modules")
	if code != ExitOK || !strings.Contains(out, "work.acme.example") {
		t.Errorf("--plan: %d %s", code, out)
	}
}

func TestStatusShowsWhereTheNamesPoint(t *testing.T) {
	dir := exposedSetup(t, fixedResolver{
		"203-0-113-7.sslip.io": {"203.0.113.7"},
		"acme.example":         {"203.0.113.7"},
		"work.acme.example":    {"198.51.100.4"},
	})
	useStack(t, false, stackResult(), nil)
	if code, out, errs := run(t, stackArgs(dir)...); code != ExitOK {
		t.Fatalf("up: %d %s %s", code, out, errs)
	}
	_, out, _ := run(t, "status", dir, "--modules-dir", "../../modules")
	for _, want := range []string{
		"acme.example -> 203.0.113.7  (site): ok",
		"work.acme.example -> 203.0.113.7  (work ui): points to 198.51.100.4",
		"careers.acme.example -> 203.0.113.7  (jobs pages): missing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	_, out, _ = run(t, "status", dir, "--modules-dir", "../../modules", "--output", "json")
	var v struct {
		Records []struct{ Name, State string } `json:"dns_records"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || len(v.Records) < 5 {
		t.Fatalf("json: %v %s", err, out)
	}
}

func TestProxiedDNSAndTheFlagSwitchEdgeLimitsOff(t *testing.T) {
	dir := exposedSetup(t, goodResolver().(fixedResolver))
	spy := useStack(t, false, stackResult(), nil)
	if code, out, errs := run(t, stackArgs(dir)...); code != ExitOK || spy.opts.Routes.NoEdgeLimit {
		t.Fatalf("limits are on by default: %d %s %s", code, out, errs)
	}
	if code, out, errs := run(t, stackArgs(dir, "--no-edge-limits")...); code != ExitOK || !spy.opts.Routes.NoEdgeLimit {
		t.Fatalf("--no-edge-limits: %d %s %s", code, out, errs)
	}
}
