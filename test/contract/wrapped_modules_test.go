package contract

// Epic E10a: wiki, CRM and status page wrapped modules (specs/019-wiki-crm-status). The generic
// manifest, chart, hook and schema checks run through the `starters` table in
// starter_modules_test.go; this file holds what is specific to these three modules.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestWikiIsTheLightChoice(t *testing.T) {
	m := readYAML(t, "modules/wiki/module.yaml")
	if asMap(m["provider"])["name"] != "DokuWiki" {
		t.Errorf("provider = %v", asMap(m["provider"]))
	}
	// SPEC 5 estimated the BookStack/Wiki.js pick at ~200 MB; the flat-file wiki must beat it.
	if asMap(m["resources"])["ram_mb"].(int) >= 200 {
		t.Error("the wiki module must be lighter than the ~200 MB catalogue estimate")
	}
	if asMap(m["requires"])["database"] != "none" {
		t.Error("a flat-file wiki needs no database")
	}
}

func TestCRMWiring(t *testing.T) {
	m := readYAML(t, "modules/crm/module.yaml")
	if !containsAny(asList(m["consumes"]), "leads") {
		t.Error("crm must consume leads")
	}
	subs := asList(m["subscribes"])
	if len(subs) != 2 {
		t.Fatalf("subscribes = %v", subs)
	}
	if erased := asMap(subs[1]); erased["event"] != "lead.erased.v1" || erased["optional"] != true || erased["status"] != "planned" {
		t.Errorf("subscribes = %v: the adapter that keeps copies of leads must hear of erasures", subs)
	}
	sub := asMap(subs[0])
	if sub["event"] != "lead.created.v1" || sub["optional"] != true || sub["webhook"] != "/hooks/lead" || sub["status"] != "planned" || sub["service"] != "crm-adapter" {
		t.Errorf("subscribes = %v: the lead adapter must be optional and planned", sub)
	}
	if !containsAny(asList(asMap(m["requires"])["capabilities"]), "email.send") {
		t.Error("crm sends mail through email.send")
	}
	for _, dc := range asList(m["data_classes"]) {
		if asMap(dc)["sensitivity"] != "standard" {
			t.Errorf("data class %v: a third-party CRM holds standard personal data only", dc)
		}
	}
	// personal data must come with documented erasure
	b, err := os.ReadFile(filepath.Join(root, "modules/crm/README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"erasure", "lead.created.v1", "personal data", "planned"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("modules/crm/README.md does not mention %q", w)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "specs/019-wiki-crm-status/contracts/lead-to-crm-adapter.md")); err != nil {
		t.Errorf("adapter design contract missing: %v", err)
	}
}

func TestPermissionNamesAreDistinctive(t *testing.T) {
	banned := []string{"platform.", "rbac.", "approvals.", "notifications."}
	owner := map[string]string{}
	for _, id := range []string{"wiki", "crm", "status", "support", "newsletter", "analytics", "work", "people", "jobs", "finance", "forms"} {
		if _, err := os.Stat(filepath.Join(root, "modules", id, "module.yaml")); err != nil {
			continue // `work` and others may live elsewhere
		}
		m := readYAML(t, "modules/"+id+"/module.yaml")
		for _, p := range asList(m["permissions"]) {
			ps := asStr(p)
			for _, b := range banned {
				if strings.HasPrefix(ps, b) {
					t.Errorf("%s: permission %s uses reserved prefix %s", id, ps, b)
				}
			}
			if prev, ok := owner[ps]; ok {
				t.Errorf("permission %s declared by both %s and %s", ps, prev, id)
			}
			owner[ps] = id
		}
	}
	for _, want := range []string{"wiki.read", "crm.read", "status.read"} {
		if owner[want] == "" {
			t.Errorf("expected permission %s", want)
		}
	}
}

func TestStatusRoutesPublicPathsOnly(t *testing.T) {
	docs, err := helmRender(t, "status", "ingress.host=status.example.com")
	if err != nil {
		t.Skipf("helm unavailable or failed: %v", err)
	}
	irs := ofKind(docs, "IngressRoute")
	if len(irs) != 1 {
		t.Fatalf("%d IngressRoutes", len(irs))
	}
	flat := mustJSON(irs[0].get("spec"))
	for _, forbidden := range []string{"/dashboard", "/socket.io", "/settings", "/setup"} {
		if strings.Contains(flat, forbidden) {
			t.Errorf("admin path %s must not be routed publicly: %s", forbidden, flat)
		}
	}
	if !strings.Contains(flat, "PathPrefix(`/status`)") {
		t.Errorf("the public status pages must be routed: %s", flat)
	}
	// the root page asks this unauthenticated endpoint which status page the host maps to
	if !strings.Contains(flat, "PathPrefix(`/api/entry-page`)") {
		t.Errorf("/api/entry-page must be routed so the root page can resolve: %s", flat)
	}
	for _, r := range asList(irs[0].get("spec", "routes")) {
		m := asStr(asMap(r)["match"])
		if !regexp.MustCompile("^Host\\(`status\\.example\\.com`\\) && (Path|PathPrefix)\\(").MatchString(m) {
			t.Errorf("route %q is not a path-restricted rule", m)
		}
	}
	// the operator can open the admin UI on the same host explicitly (local dev, first run)
	docs, err = helmRender(t, "status", "ingress.host=status.example.com", "ingress.publicOnly=false")
	if err != nil {
		t.Fatal(err)
	}
	if f := mustJSON(ofKind(docs, "IngressRoute")[0].get("spec")); !strings.Contains(f, "Host(`status.example.com`)") || strings.Contains(f, "PathPrefix") {
		t.Errorf("publicOnly=false must route the whole host: %s", f)
	}
}

func TestStatusPublicOnlyDefaultsInValues(t *testing.T) {
	v := readYAML(t, "modules/status/chart/values.yaml")
	ing := asMap(v["ingress"])
	if ing["publicOnly"] != true {
		t.Errorf("ingress.publicOnly must default to true: %v", ing["publicOnly"])
	}
	if len(asList(ing["publicPaths"])) == 0 {
		t.Error("ingress.publicPaths required")
	}
}

func TestWrappedModulesDocs(t *testing.T) {
	for _, id := range []string{"wiki", "crm", "status"} {
		b, err := os.ReadFile(filepath.Join(root, "modules", id, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ToLower(string(b))
		for _, w := range []string{"footprint", "licence", "sso", "unverified", "alternative"} {
			if !strings.Contains(s, w) {
				t.Errorf("modules/%s/README.md does not mention %q", id, w)
			}
		}
	}
}

func TestSpecArtifactsExist(t *testing.T) {
	for _, f := range []string{"spec.md", "plan.md", "research.md", "data-model.md", "tasks.md", "contracts/lead-to-crm-adapter.md"} {
		if _, err := os.Stat(filepath.Join(root, "specs/019-wiki-crm-status", f)); err != nil {
			t.Error(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, "specs/019-wiki-crm-status/research.md"))
	for _, w := range []string{"R-licences", "R-footprint", "DokuWiki", "EspoCRM", "Uptime Kuma"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("research.md lacks %q", w)
		}
	}
}

// docs/modules-catalogue.md lists every module directory (except `_` examples) with status, RAM and licence.
func TestModulesCatalogueCoversEveryModule(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "docs", "modules-catalogue.md"))
	if err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(filepath.Join(root, "modules"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		m := readYAML(t, "modules/"+e.Name()+"/module.yaml")
		row := ""
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "| `"+e.Name()+"` ") {
				row = l
			}
		}
		if row == "" {
			t.Errorf("modules-catalogue.md has no row for %s", e.Name())
			continue
		}
		if lic := asStr(asMap(m["provider"])["licence"]); lic != "" && !strings.Contains(row, lic) {
			t.Errorf("%s row lacks licence %s: %s", e.Name(), lic, row)
		}
		st := asStr(m["status"])
		if st == "" {
			st = "ready"
		}
		if !strings.Contains(row, st) {
			t.Errorf("%s row lacks status %s: %s", e.Name(), st, row)
		}
	}
}
