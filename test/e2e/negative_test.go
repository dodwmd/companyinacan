//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// (h) Cross-module negative checks through the edge.

// A credential of one module is worthless at another: each service has its own token store.
func TestCrossModuleTokensAreRefused(t *testing.T) {
	hosts := map[string]string{
		"work.localhost":    "/v1/projects",
		"people.localhost":  "/v1/people",
		"jobs.localhost":    "/v1/jobs",
		"finance.localhost": "/v1/invoices",
		"equity.localhost":  "/v1/entities",
		"forms.localhost":   "/v1/leads",
	}
	tokens := map[string]string{
		"work.localhost":    tokenOf(t, "work", "CAN_WORK_ADMIN_TOKEN"),
		"people.localhost":  tokenOf(t, "people", "CAN_PEOPLE_API_TOKEN"),
		"jobs.localhost":    tokenOf(t, "jobs", "CAN_JOBS_ADMIN_TOKEN"),
		"finance.localhost": tokenOf(t, "finance", "CAN_FINANCE_ADMIN_TOKEN"),
		"equity.localhost":  tokenOf(t, "equity", "CAN_EQUITY_API_TOKEN"),
		"forms.localhost":   tokenOf(t, "forms", "CAN_FORMS_API_TOKEN"),
	}
	for host, path := range hosts {
		// the module's own token works ...
		web.Do(t, harness.Req{Host: host, Path: path, Token: tokens[host]}).Expect(t, host+" accepts its own token", 200)
		// ... no token and every other module's token do not
		web.Do(t, harness.Req{Host: host, Path: path}).Expect(t, host+" refuses a request without a token", 401)
		for other, tok := range tokens {
			if other == host {
				continue
			}
			web.Do(t, harness.Req{Host: host, Path: path, Token: tok}).Expect(t, host+" refuses the token of "+other, 401)
		}
	}
	// The one explicit case from the epic: a work token against the people API.
	web.Do(t, harness.Req{Host: "people.localhost", Path: "/v1/people", Token: tokens["work.localhost"]}).Expect(t, "work token on the people API", 401)
	// Introspection-only identities are not valid at modules that do not use them.
	web.Do(t, harness.Req{Host: "work.localhost", Path: "/v1/projects", Token: tokHR}).Expect(t, "an hr introspection token on the work API", 401)
}

// An unknown Host header must not reach any private virtual host, and must not get even the public
// site (a DNS-rebinding page could read it): the dev edge refuses it with 421. The epic text allowed
// "public site only"; the test accepts that too, but never an API, an admin page or a UI.
func TestHostileHostReachesNothingPrivate(t *testing.T) {
	for _, host := range []string{"evil.example", "admin.localhost.evil.example", "work.localhost.evil.example", "127.0.0.2", "work.localhost@evil.example"} {
		for _, p := range []string{"/", "/login", "/v1/people", "/v1/invoices", "/v1/projects", "/v1/entities", "/ui/", "/careers", "/healthz", "/metrics", "/_forms/contact/token"} {
			resp := web.Do(t, harness.Req{Host: "localhost", Path: p, Headers: map[string]string{"Host": host}})
			body := resp.String()
			if resp.Status == 421 {
				continue
			}
			// anything else must be the public example site, never a private surface
			if resp.Status == 200 && p == "/" && strings.Contains(body, "Acme") {
				continue
			}
			if resp.Status == 404 {
				continue
			}
			t.Errorf("Host %q GET %s answered HTTP %d: %.120s (want 421, or the public site only)", host, p, resp.Status, body)
		}
	}
	// And the named hosts still work, so the catch-all did not shadow them.
	web.Do(t, harness.Req{Host: "work.localhost", Path: "/healthz"}).Expect(t, "work.localhost still routed", 200)
	web.Do(t, harness.Req{Host: "localhost", Path: "/"}).Expect(t, "localhost still routed to the site", 200)
}

// Nothing exposes Prometheus-style metrics, or debug endpoints, through the edge.
func TestMetricsAndDebugAreNotExposed(t *testing.T) {
	for _, host := range []string{"localhost", "admin.localhost", "work.localhost", "people.localhost", "jobs.localhost", "finance.localhost", "equity.localhost", "forms.localhost"} {
		for _, p := range []string{"/metrics", "/debug/pprof/", "/debug/vars", "/_edge/metrics", "/actuator"} {
			r := web.Do(t, harness.Req{Host: host, Path: p})
			if r.Status == 200 && (strings.Contains(r.String(), "# HELP") || strings.Contains(r.String(), "goroutine") || strings.Contains(r.String(), "memstats")) {
				t.Errorf("http://%s%s exposes metrics or debug data (HTTP 200)", host, p)
			}
		}
	}
}

// The private admin and API surfaces refuse anonymous callers.
func TestAnonymousCannotReachPrivateSurfaces(t *testing.T) {
	web.Do(t, harness.Req{Host: "admin.localhost", Path: "/api/v1/modules"}).Expect(t, "admin API without a session", 401, 403)
	web.Do(t, harness.Req{Host: "admin.localhost", Path: "/audit"}).Expect(t, "admin audit page without a session", 303)
	web.Do(t, harness.Req{Host: "people.localhost", Path: "/"}).Expect(t, "people UI without a session", 303)
	web.Do(t, harness.Req{Host: "work.localhost", Path: "/ui/projects/X"}).Expect(t, "work UI without a session", 303)
	web.Do(t, harness.Req{Host: "jobs.localhost", Path: "/applications/app_x"}).Expect(t, "jobs UI without a session", 303)
	// Public by design.
	web.Do(t, harness.Req{Host: "jobs.localhost", Path: "/careers"}).Expect(t, "careers is public", 200)
	web.Do(t, harness.Req{Host: "finance.localhost", Path: "/public/invoices/not-a-link"}).Expect(t, "a bad public invoice link is 404", 404)
}
