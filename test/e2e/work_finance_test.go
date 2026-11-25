//go:build e2e

package e2e

import (
	"net/url"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// (c) Work tracker: an admin mints a work.write token (compose cannot verify can-core personal access
// tokens, see docs/local-compose.md), the token creates a project and an issue through the API, and the
// same token signs in to the web UI where the backlog and the board show the issue.
func TestWorkTrackerAPIAndUI(t *testing.T) {
	admin := workAPI(t)
	u := harness.Unique()
	key := "E" + strings.ToUpper(u[:5]) // project keys are upper-case letters and digits
	key = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r + 17 // digits -> letters, keys must not contain digits in some configs
		}
		return r
	}, key)

	tok := admin.Post(t, "/v1/tokens", map[string]any{"kind": "service", "subject": "svc:e2e-" + u, "name": "e2e " + u, "scopes": []string{"work.write"}}).
		Expect(t, "mint a work.write token", 201, 200).Str("token")
	if tok == "" {
		t.Fatalf("the tokens API returned no secret")
	}
	api := admin.WithToken(tok)
	api.Post(t, "/v1/projects", map[string]any{"key": key, "name": "E2E " + u}).Expect(t, "token without work.admin cannot create a project", 403)
	admin.Post(t, "/v1/projects", map[string]any{"key": key, "name": "E2E " + u}).Expect(t, "create project "+key, 201)

	title := "E2E issue " + u
	iss := api.Post(t, "/v1/projects/"+key+"/issues", map[string]any{"type": "task", "title": title, "priority": "P2"}).Expect(t, "create issue with the scoped token", 201)
	ikey := iss.Str("key")
	if ikey == "" {
		t.Fatalf("no issue key in the create response: %s", iss.String())
	}
	api.Post(t, "/v1/issues/"+ikey+"/comments", map[string]any{"body": "from the e2e suite"}).Expect(t, "comment", 201, 200)
	api.Get(t, "/v1/projects/"+key+"/board").Expect(t, "board API", 200)

	// Web UI: sign in by pasting the token.
	s := harness.NewSession(env)
	const host = "work.localhost"
	s.Do(t, harness.Req{Host: host, Path: "/ui/"}).Expect(t, "UI without a session", 303)
	page := s.Do(t, harness.Req{Host: host, Path: "/ui/login"}).Expect(t, "UI login page", 200)
	origin := map[string]string{"Origin": "http://" + host + ":" + env.Port}
	s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/ui/login", Headers: origin,
		Body: url.Values{"csrf": {harness.CSRF(page.String())}, "token": {"not-a-token-0123456789abcdef0123456789"}}}).Expect(t, "UI refuses a wrong token", 401)
	page = s.Do(t, harness.Req{Host: host, Path: "/ui/login"})
	s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/ui/login", Headers: origin,
		Body: url.Values{"csrf": {harness.CSRF(page.String())}, "token": {tok}}}).Expect(t, "UI sign-in with the minted token", 303)
	backlog := s.Do(t, harness.Req{Host: host, Path: "/ui/projects/" + key}).Expect(t, "UI backlog", 200)
	if !strings.Contains(backlog.String(), title) {
		t.Fatalf("the UI backlog of %s does not list %q", key, title)
	}
	board := s.Do(t, harness.Req{Host: host, Path: "/ui/projects/" + key + "/board?sprint=all"}).Expect(t, "UI board", 200)
	if !strings.Contains(board.String(), title) {
		t.Fatalf("the UI board of %s does not show %q", key, title)
	}
	s.Do(t, harness.Req{Host: host, Path: "/ui/issues/" + ikey}).Expect(t, "UI issue page", 200)
}

// (f) Finance: party -> invoice -> separation of duties -> approval by a second subject -> send ->
// public link (no login) -> payment settles the invoice.
func TestFinanceInvoiceFlow(t *testing.T) {
	admin := financeAPI(t)
	u := harness.Unique()

	party := admin.Post(t, "/v1/parties", map[string]any{"kind": "customer", "name": "E2E Customer " + u, "email": "customer-" + u + "@acme.example"}).Expect(t, "create party", 201).Str("id")
	inv := admin.Post(t, "/v1/invoices", map[string]any{
		"party_id": mustInt(t, party), "currency": "USD",
		"lines": []map[string]any{{"description": "Anvil " + u, "quantity_milli": 2000, "unit_price_minor": 4900}},
	}).Expect(t, "create invoice", 201)
	id := inv.Str("id")
	admin.Post(t, "/v1/invoices/"+id+"/approve", nil).Expect(t, "creator cannot approve their own invoice", 403, 409)

	appr := admin.Post(t, "/v1/tokens", map[string]any{"kind": "service", "subject": "e2e-approver-" + u, "name": "e2e approver " + u, "scopes": []string{"finance.approve", "finance.read"}}).
		Expect(t, "mint approver token", 201, 200).Str("token")
	if appr == "" {
		t.Fatalf("no approver token returned")
	}
	approver := admin.WithToken(appr)
	approver.Post(t, "/v1/invoices/"+id+"/send", nil).Expect(t, "approver (no write scope) cannot send", 403)
	got := approver.Post(t, "/v1/invoices/"+id+"/approve", nil).Expect(t, "approval by a second subject", 200)
	if got.Str("number") == "" {
		t.Fatalf("an approved invoice must have a number: %s", got.String())
	}
	admin.Post(t, "/v1/invoices/"+id+"/send", nil).Expect(t, "send", 200)

	link := admin.Post(t, "/v1/invoices/"+id+"/link", map[string]any{}).Expect(t, "issue public link", 200, 201)
	linkURL := link.Str("url")
	if linkURL == "" {
		t.Fatalf("no url in the link response: %s", link.String())
	}
	u2, err := url.Parse(linkURL)
	if err != nil || !strings.HasPrefix(u2.Path, "/public/invoices/") {
		t.Fatalf("unexpected public link %q", linkURL)
	}
	// Public: no token at all, through the edge on the finance host.
	pub := web.Do(t, harness.Req{Host: "finance.localhost", Path: u2.Path}).Expect(t, "public invoice link without a login", 200)
	if !strings.Contains(pub.String(), "Anvil "+u) {
		t.Fatalf("the public invoice page does not show the invoice lines")
	}
	if strings.Contains(pub.String(), "e2e-approver") {
		t.Fatalf("the public invoice page leaks the approver's subject")
	}
	web.Do(t, harness.Req{Host: "finance.localhost", Path: u2.Path + "x"}).Expect(t, "a forged link is 404", 404)

	admin.Post(t, "/v1/invoices/"+id+"/payments", map[string]any{"amount_minor": 9800, "currency": "USD", "method": "bank_transfer", "reference": "e2e " + u, "received_on": "2026-01-15"}).
		Expect(t, "record payment", 201, 200)
	after := admin.Get(t, "/v1/invoices/"+id).Expect(t, "read invoice", 200)
	if after.Str("status") != "paid" {
		t.Fatalf("invoice %s should be paid after a payment of the full total, status = %q: %s", id, after.Str("status"), after.String())
	}
}
