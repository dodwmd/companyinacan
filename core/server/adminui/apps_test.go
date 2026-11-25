package adminui

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/ticket"
)

var launchTicketRe = regexp.MustCompile(`https://expenses\.example\.org/login/ticket\?t=(cant_[A-Za-z0-9_-]{43})`)

// newAppsUI is the platform harness with a module that has a UI whose name is hostile markup.
func newAppsUI(t *testing.T) *pui {
	t.Helper()
	p := newPUI(t)
	ctx := context.Background()
	m, err := p.db.GetModule(ctx, "expenses")
	if err != nil {
		t.Fatal(err)
	}
	m.Manifest.Name = `Expenses <script>alert(1)</script>`
	m.Manifest.Runtime.Subdomain = "expenses"
	m.Manifest.Runtime.WebLogin = &config.WebLogin{ViaSubdomain: true}
	if err := p.db.UpsertModule(ctx, m, nil, nil); err != nil {
		t.Fatal(err)
	}
	p.ui.Tickets = &ticket.Service{DB: p.db, Audit: p.audit, Now: func() time.Time { return *p.now }, Launcher: ticket.Launcher{Domain: "example.org"},
		Holds: func(ctx context.Context, subject, perm string) (bool, error) {
			return p.rb.Check(ctx, subject, perm, "")
		}}
	return p
}

func TestAppsPageListsModulesEscapedAndOffersAPostToOpen(t *testing.T) {
	p := newAppsUI(t)
	root := p.loginAs("root")
	r := p.do("GET", "/apps", nil, root.with()...)
	if r.StatusCode != 200 {
		t.Fatalf("%d\n%s", r.StatusCode, r.body)
	}
	if strings.Contains(r.body, "<script>alert(1)</script>") || !strings.Contains(r.body, "&lt;script&gt;") {
		t.Fatalf("module name from a manifest is not escaped:\n%s", r.body)
	}
	if !strings.Contains(r.body, `<form method="post" action="/apps/expenses/launch"`) || !strings.Contains(r.body, `name="csrf"`) {
		t.Fatalf("no POST form with a CSRF field:\n%s", r.body)
	}
	if strings.Contains(r.body, "cant_") || strings.Contains(r.body, "login/ticket") || strings.Contains(r.body, "example.org") {
		t.Fatalf("the page carries a ticket or a module address before the click:\n%s", r.body)
	}
	if probs := a11yProblems(r.body); len(probs) > 0 {
		t.Errorf("accessibility problems: %v", probs)
	}
	if !strings.Contains(r.body, `href="/apps"`) {
		t.Errorf("no Apps entry in the navigation")
	}
}

func TestAppsPageHidesWhatYouMayNotOpen(t *testing.T) {
	p := newAppsUI(t)
	alice := p.loginAs("alice")
	r := p.do("GET", "/apps", nil, alice.with()...)
	if r.StatusCode != 200 || strings.Contains(r.body, "/apps/expenses/launch") {
		t.Fatalf("%d\n%s", r.StatusCode, r.body)
	}
	r = p.do("POST", "/apps/expenses/launch", url.Values{"csrf": {alice.csrf}}, alice.with()...)
	if r.StatusCode != 404 || launchTicketRe.MatchString(r.body) {
		t.Fatalf("launch without a permission = %d", r.StatusCode)
	}
}

func TestLaunchMintsATicketAndMovesTheBrowserOn(t *testing.T) {
	p := newAppsUI(t)
	root := p.loginAs("root")
	r := p.do("POST", "/apps/expenses/launch", url.Values{"csrf": {root.csrf}, "return": {"/reports"}}, root.with()...)
	if r.StatusCode != 200 {
		t.Fatalf("%d %s\n%s", r.StatusCode, r.Header.Get("Location"), r.body)
	}
	m := launchTicketRe.FindStringSubmatch(r.body)
	if m == nil || !strings.Contains(r.body, `http-equiv="refresh"`) {
		t.Fatalf("no interstitial to the module:\n%s", r.body)
	}
	if r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers: %v", r.Header)
	}
	if strings.Contains(r.Header.Get("Location"), "cant_") {
		t.Fatal("the ticket is in a redirect header")
	}
	red, ok := p.ui.Tickets.Redeem(context.Background(), "expenses", m[1])
	if !ok || red.Subject != "user:root" || red.ReturnPath != "/reports" {
		t.Fatalf("redeem %+v %v", red, ok)
	}
	// Audited with ids only: the ticket never reaches the log.
	recs, _ := p.audit.Query(context.Background(), audit.Filter{ActionPrefix: "ui.ticket.", Limit: 20})
	if len(recs) < 2 {
		t.Fatalf("audit: %+v", recs)
	}
	for _, rec := range recs {
		if strings.Contains(rec.Detail+rec.Target+rec.Actor, m[1]) {
			t.Fatalf("ticket in the audit log: %+v", rec)
		}
	}
	if strings.Contains(p.logs.String(), m[1]) {
		t.Fatal("the ticket reached the request log")
	}
}

func TestLaunchNeedsCSRFSessionAndPost(t *testing.T) {
	p := newAppsUI(t)
	root := p.loginAs("root")
	if r := p.do("POST", "/apps/expenses/launch", url.Values{"csrf": {"wrong"}}, root.with()...); r.StatusCode != 403 || launchTicketRe.MatchString(r.body) {
		t.Fatalf("bad csrf = %d", r.StatusCode)
	}
	if r := p.do("POST", "/apps/expenses/launch", url.Values{}, root.with()...); r.StatusCode != 403 {
		t.Fatalf("missing csrf = %d", r.StatusCode)
	}
	if r := p.do("POST", "/apps/expenses/launch", url.Values{"csrf": {root.csrf}}); r.StatusCode != 303 || r.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous = %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := p.do("GET", "/apps/expenses/launch", nil, root.with()...); r.StatusCode == 200 || launchTicketRe.MatchString(r.body) {
		t.Fatalf("a GET minted a ticket: %d", r.StatusCode)
	}
	// A cross-site form post (Sec-Fetch-Site) is refused even with a valid token: the harness has
	// no header hook, so the check is on the route table (authed enforces it for every POST).
}

func TestLaunchRefusesHostileReturnPaths(t *testing.T) {
	p := newAppsUI(t)
	root := p.loginAs("root")
	for _, bad := range []string{"https://evil.example/", "//evil.example", "/a/../b", "/a?x=1", "javascript:alert(1)"} {
		r := p.do("POST", "/apps/expenses/launch", url.Values{"csrf": {root.csrf}, "return": {bad}}, root.with()...)
		if r.StatusCode != 400 || launchTicketRe.MatchString(r.body) {
			t.Errorf("return %q = %d", bad, r.StatusCode)
		}
	}
}

func TestLaunchIsRateLimitedPerUser(t *testing.T) {
	p := newAppsUI(t)
	root := p.loginAs("root")
	for i := 0; i < ticket.MintPerMinute; i++ {
		if r := p.do("POST", "/apps/expenses/launch", url.Values{"csrf": {root.csrf}}, root.with()...); r.StatusCode != 200 {
			t.Fatalf("launch %d = %d", i, r.StatusCode)
		}
	}
	if r := p.do("POST", "/apps/expenses/launch", url.Values{"csrf": {root.csrf}}, root.with()...); r.StatusCode != 429 || launchTicketRe.MatchString(r.body) {
		t.Fatalf("over the limit = %d", r.StatusCode)
	}
}

func TestAppsAreUnavailableWithoutTheService(t *testing.T) {
	p := newPUI(t)
	root := p.loginAs("root")
	if r := p.do("GET", "/apps", nil, root.with()...); r.StatusCode != 503 {
		t.Fatalf("%d", r.StatusCode)
	}
	if strings.Contains(p.do("GET", "/settings", nil, root.with()...).body, `href="/apps"`) {
		t.Fatal("Apps shown in the navigation without the service")
	}
}
