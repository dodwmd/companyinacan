package contract

// specs/040-module-routing: the module manifests, core/routing and the admission policy must agree.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/routing"
)

func routingManifests(t *testing.T) map[string]*config.Module {
	t.Helper()
	paths, err := filepath.Glob("../../modules/*/module.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatal(paths, err)
	}
	out := map[string]*config.Module{}
	for _, p := range paths {
		doc, ps := config.Load(p)
		if doc == nil {
			t.Fatal(ps)
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			t.Fatal(err)
		}
		out[m.ID] = m
	}
	return out
}

var uiRouteRE = regexp.MustCompile(`"(?:GET|POST|PUT|DELETE)(?: |", ")(/[^"]*)"`)

// Every page and action of a web UI is reachable through its route, and the list claims nothing the
// UI does not serve.
func TestPublicUIPathsMatchTheWebUIs(t *testing.T) {
	ms := routingManifests(t)
	for _, id := range []string{"work", "people", "jobs", "finance"} {
		p := ms[id].Runtime.PublicUI
		if p == nil || ms[id].Runtime.WebLogin == nil {
			t.Errorf("%s: a web UI with launch tickets needs runtime.public_ui", id)
			continue
		}
		if err := routing.CheckUIPaths(p.Exact, p.Prefixes); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		files, _ := filepath.Glob("../../core/" + id + "/ui*.go")
		served := map[string]bool{}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range uiRouteRE.FindAllStringSubmatch(string(src), -1) {
				first, _, _ := strings.Cut(strings.TrimPrefix(m[1], "/"), "/")
				if first == "{$}" {
					first = ""
				}
				served["/"+first] = true
			}
		}
		if len(served) == 0 {
			t.Fatalf("%s: no UI routes found in the source", id)
		}
		for s := range served {
			if !slices.Contains(p.Exact, s) && !slices.Contains(p.Prefixes, s) {
				t.Errorf("%s: the UI serves %s, which runtime.public_ui does not route", id, s)
			}
		}
		for _, s := range p.Prefixes {
			if !served[s] {
				t.Errorf("%s: runtime.public_ui routes %s, which the UI does not serve", id, s)
			}
		}
	}
}

func TestModuleHostsAndSitePathsAreClaimable(t *testing.T) {
	for id, m := range routingManifests(t) {
		if sd := m.Runtime.Subdomain; sd != "" && id != "example" && !routing.MayClaim(id, sd) {
			t.Errorf("%s may not claim its subdomain %s (core/routing.Aliases)", id, sd)
		}
		if r := m.Runtime.SiteRoute; r != nil && routing.SitePrefixes[id] != r.Path {
			t.Errorf("%s may not claim %s", id, r.Path)
		}
	}
	if m := routingManifests(t)["forms"]; m.Runtime.SiteRoute == nil {
		t.Error("forms declares no site route: /_forms would not be routed")
	}
}

func TestAdmissionPolicyMirrorsCoreRouting(t *testing.T) {
	b, err := os.ReadFile("../../deploy/base/admission-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy := string(b)
	var aliases, labels []string
	for id, label := range routing.Aliases {
		aliases = append(aliases, "'"+id+"': '"+label+"'")
		labels = append(labels, "'"+label+"'")
	}
	for _, a := range aliases {
		if !strings.Contains(policy, a) {
			t.Errorf("the policy lacks the alias %s", a)
		}
	}
	reserved := regexp.MustCompile(`variables\.id in \[([^\]]*)\] \? ''`).FindStringSubmatch(policy)
	if reserved == nil {
		t.Fatal("no reserved list in the policy")
	}
	got := strings.Split(reserved[1], ", ")
	want := append([]string{"''", "'admin'", "'www'"}, labels...)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("reserved ids in the policy %v, core/routing has %v", got, want)
	}
	for id, path := range routing.SitePrefixes {
		if !strings.Contains(policy, "variables.id == '"+id+"'") || !strings.Contains(policy, "PathPrefix(`"+path+"`)") {
			t.Errorf("the policy does not know that %s owns %s", id, path)
		}
	}
}
