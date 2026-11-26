//go:build e2e

package e2e

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// (a) The public site answers, and a contact form post through the edge becomes a lead that the
// leads API (bearer token) returns.
func TestSiteAndLeadCapture(t *testing.T) {
	r := web.Do(t, harness.Req{Host: "localhost", Path: "/"}).Expect(t, "site /", 200)
	if !strings.Contains(r.String(), "Acme") {
		t.Fatalf("site / does not look like the example site (no \"Acme\" in the page): %.200s", r.String())
	}
	web.Do(t, harness.Req{Host: "localhost", Path: "/contact/"}).Expect(t, "site /contact/", 200)

	tok := web.Do(t, harness.Req{Host: "localhost", Path: "/_forms/contact/token"}).Expect(t, "forms timing token", 200).Str("token")
	if tok == "" {
		t.Fatalf("the forms gateway returned no timing token")
	}
	time.Sleep(3 * time.Second) // min_fill_seconds: a form posted instantly is a bot
	email := "lead-" + harness.Unique() + "@acme.example"
	web.Do(t, harness.Req{
		Method: "POST", Host: "localhost", Path: "/_forms/contact",
		Headers: map[string]string{"Origin": "http://localhost:" + env.Port},
		Body:    url.Values{"name": {"E2E Lead"}, "email": {email}, "message": {"hello from the e2e suite"}, "website": {""}, "_t": {tok}},
	}).Expect(t, "contact form POST", 200, 201, 303)

	leads := harness.API{C: web, Host: "forms.localhost", Token: tokenOf(t, "forms", "CAN_FORMS_API_TOKEN")}
	got := leads.Get(t, "/v1/leads?form=contact").Expect(t, "leads API", 200)
	if !strings.Contains(got.String(), email) {
		t.Fatalf("the lead %s is not in the leads API after a successful form post: %.300s", email, got.String())
	}

	// A form posted from a foreign origin must be refused, and a request without the token too.
	web.Do(t, harness.Req{
		Method: "POST", Host: "localhost", Path: "/_forms/contact",
		Headers: map[string]string{"Origin": "https://evil.example"},
		Body:    url.Values{"name": {"x"}, "email": {"x@evil.example"}, "message": {"x"}, "_t": {tok}},
	}).Expect(t, "foreign-origin form post is refused", 400, 403, 422)
	if strings.Contains(leads.Get(t, "/v1/leads?form=contact").String(), "x@evil.example") {
		t.Fatalf("a foreign-origin post was stored as a lead")
	}
}

// (b) Admin login with password and TOTP (the secret comes from `can-core admin reset-password
// --rotate-totp`, like dev/compose/bootstrap-admin.sh), then the dashboard, the module pages, the
// audit log, the API token page and the module catalogue.
func TestAdminLoginTOTPAndPages(t *testing.T) {
	s := harness.NewSession(env)
	const host = "admin.localhost"

	// Anonymous: sent to the login page.
	web.Do(t, harness.Req{Host: host, Path: "/"}).Expect(t, "admin / without a session", 303)
	login := s.Do(t, harness.Req{Host: host, Path: "/login"}).Expect(t, "login page", 200)
	csrf := harness.CSRF(login.String())
	if csrf == "" {
		t.Fatalf("no csrf field on the admin login page")
	}
	origin := map[string]string{"Origin": "http://" + host + ":" + env.Port}

	// A wrong password is refused.
	s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/login", Headers: origin,
		Body: url.Values{"csrf": {csrf}, "username": {"admin"}, "password": {"not-the-password"}, "totp": {"000000"}}}).
		Expect(t, "wrong password is refused", 401, 403)

	adminLogin(t, s)

	// Sign-in lands on the business overview; the technical dashboard is one link away.
	home := s.Do(t, harness.Req{Host: host, Path: "/"}).Expect(t, "the root after sign-in", 303)
	if home.Header.Get("Location") != "/overview" {
		t.Fatalf("sign-in lands on %q", home.Header.Get("Location"))
	}
	dash := s.Do(t, harness.Req{Host: host, Path: "/dashboard"}).Expect(t, "dashboard", 200)
	if !strings.Contains(dash.String(), "Dashboard") || !strings.Contains(dash.String(), "Acme Co") {
		t.Fatalf("dashboard does not show the company: %.300s", dash.String())
	}
	s.Do(t, harness.Req{Host: host, Path: "/modules"}).Expect(t, "modules page", 200)
	audit := s.Do(t, harness.Req{Host: host, Path: "/audit"}).Expect(t, "audit page", 200)
	if !strings.Contains(audit.String(), "admin.login") {
		t.Fatalf("the audit log does not show the login just made")
	}
	s.Do(t, harness.Req{Host: host, Path: "/leads"}).Expect(t, "leads page", 200)
	// Module management is on (the dev stack records the modules it runs): the catalogue renders and
	// the tokens page offers their permissions (devstack_test.go uses them).
	tokens := s.Do(t, harness.Req{Host: host, Path: "/tokens"}).Expect(t, "API tokens page", 200)
	if !strings.Contains(tokens.String(), `value="work.read"`) {
		t.Errorf("the API tokens page offers no work permission: %.300s", tokens.String())
	}
	cat := s.Do(t, harness.Req{Host: host, Path: "/catalogue"}).Expect(t, "module catalogue", 200)
	for _, id := range []string{"work", "people", "jobs", "finance", "equity"} {
		if !strings.Contains(cat.String(), "/catalogue/"+id) {
			t.Errorf("the catalogue does not list %s", id)
		}
	}

	// Log out ends the session.
	out := s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/logout", Headers: origin, Body: url.Values{"csrf": {harness.CSRF(dash.String())}}})
	out.Expect(t, "logout", 303)
	s.Do(t, harness.Req{Host: host, Path: "/"}).Expect(t, "session ended by logout", 303)
}
