package contract

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/jobs"
	"github.com/dodwmd/companyinacan/core/people"
	"github.com/dodwmd/companyinacan/core/server/adminapi"
	"github.com/dodwmd/companyinacan/core/server/adminui"
	"github.com/dodwmd/companyinacan/core/server/moduleapi"
	"github.com/dodwmd/companyinacan/core/work"
)

// specs/036-module-ui-sso/contracts/ui-routes.md lists every route of the module-UI single sign-on
// (E11c). Each must be served by the code that owns it, and each served ticket route must be listed.
func TestSSORoutesMatchTheContract(t *testing.T) {
	b, err := os.ReadFile("../../specs/036-module-ui-sso/contracts/ui-routes.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]bool{}
	for _, m := range regexp.MustCompile("`((?:GET|POST) /[^`]+)`").FindAllStringSubmatch(string(b), -1) {
		doc[m[1]] = true
	}
	var wp []string
	for _, r := range work.UIRoutes() {
		wp = append(wp, r.Method+" "+r.Path)
	}
	fin, _ := finance.UIRoutes()
	served := map[string][]string{
		"adminui":   adminui.Routes(),
		"adminapi":  adminapi.Routes(),
		"moduleapi": moduleapi.Routes(),
		"work":      wp,
		"people":    people.UIRoutes(),
		"jobs":      jobs.UIRoutes(),
		"finance":   fin,
	}
	want := map[string][]string{
		"adminui":   {"GET /apps", "POST /apps/{id}/launch"},
		"adminapi":  {"GET /api/v1/apps", "POST /api/v1/apps/{id}/launch"},
		"moduleapi": {"POST /v1/tickets/peek", "POST /v1/tickets/redeem", "POST /v1/tickets/revalidate"},
		"work":      {"GET /ui/login/ticket", "POST /ui/login/ticket"},
		"people":    {"GET /login/ticket", "POST /login/ticket"},
		"jobs":      {"GET /login/ticket", "POST /login/ticket"},
		"finance":   {"GET /login/ticket", "POST /login/ticket"},
	}
	for pkg, routes := range want {
		for _, r := range routes {
			if !slices.Contains(served[pkg], r) {
				t.Errorf("%s does not serve %q", pkg, r)
			}
			if !doc[r] {
				t.Errorf("%q is not in specs/036-module-ui-sso/contracts/ui-routes.md", r)
			}
		}
	}
	// Anything with a ticket or apps in its path that the code serves must be documented.
	re := regexp.MustCompile(`/(apps|tickets|login/ticket)`)
	for pkg, routes := range served {
		for _, r := range routes {
			if re.MatchString(r) && !doc[r] {
				t.Errorf("%s serves %q which the contract does not list", pkg, r)
			}
		}
	}
}
