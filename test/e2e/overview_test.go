//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// The business overview (specs/053-business-overview): can-core asks finance, people and work for
// their aggregates over the compose network with single-use grants, and each answers after asking
// can-core who the grant is for. The admin is an owner, so holds every permission.
func TestBusinessOverview(t *testing.T) {
	s := adminSession(t)
	page := s.Do(t, harness.Req{Host: "admin.localhost", Path: "/overview"}).Expect(t, "overview page", 200).String()
	for _, want := range []string{"Business overview", "Needs attention", "Approvals waiting for you",
		"Receivables (unpaid invoices)", "Overdue invoices", "Headcount you can see", "Open P0 and P1 issues"} {
		if !strings.Contains(page, want) {
			t.Errorf("the overview lacks %q", want)
		}
	}
	if strings.Contains(page, "is unavailable") {
		t.Errorf("a module did not answer:\n%s", page)
	}
	// A summary grant is not something a browser or a script can make up.
	for _, host := range []string{"work.localhost", "finance.localhost", "people.localhost"} {
		harness.API{C: web, Host: host, Token: "cansum_" + strings.Repeat("A", 43)}.Get(t, "/v1/summary").Expect(t, host+": a made-up grant", 401)
	}
}
