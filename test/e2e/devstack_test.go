//go:build e2e

package e2e

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

var (
	newPAT     = regexp.MustCompile(`<code>(canpat_[A-Za-z0-9_-]+)</code>`)
	revokeForm = regexp.MustCompile(`action="/tokens/([^"/]+)/revoke"`)
	launchLink = regexp.MustCompile(`class="button" href="(http://localhost:[^"]+)"`)
)

var (
	sharedOnce  sync.Once
	sharedAdmin *harness.Client
)

// adminSession is one admin session at admin.localhost shared by these scenarios: each login needs a
// fresh authentication code, so signing in for every scenario would wait for one every time.
func adminSession(t *testing.T) *harness.Client {
	t.Helper()
	sharedOnce.Do(func() {
		s := harness.NewSession(env)
		adminLogin(t, s)
		sharedAdmin = s
	})
	if sharedAdmin == nil {
		t.Fatalf("the shared admin sign-in failed in an earlier scenario")
	}
	return sharedAdmin
}

// issuePAT issues a personal access token in the admin UI session s (signed in at admin.localhost).
func issuePAT(t *testing.T, s *harness.Client, module string, perms ...string) string {
	t.Helper()
	const host = "admin.localhost"
	page := s.Do(t, harness.Req{Host: host, Path: "/tokens"}).Expect(t, "tokens page", 200)
	r := s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/tokens", Headers: adminOrigin(),
		Body: url.Values{"csrf": {harness.CSRF(page.String())}, "name": {"e2e " + module + " " + harness.Unique()}, "module": {module}, "permissions": perms, "ttl_hours": {"24"}}}).
		Expect(t, "issue a "+module+" token", 200)
	m := newPAT.FindStringSubmatch(r.String())
	if m == nil {
		t.Fatalf("no token on the page after issuing for %s", module)
	}
	return m[1]
}

func adminOrigin() map[string]string {
	return map[string]string{"Origin": "http://admin.localhost:" + env.Port}
}

// approveInCanCore asks can-core to approve a draft invoice (as the admin, with a personal access
// token) and has a second administrator, holding the finance role, approve it in the admin UI.
func approveInCanCore(t *testing.T, fin harness.API, invoiceID, u string) {
	t.Helper()
	const host = "admin.localhost"
	name, pw := "e2efin"+u, env.Secret(t, "core", "CAN_ADMIN_PASSWORD")
	out := env.Run(t, "exec", "-T", "-e", "CAN_ADMIN_PASSWORD="+pw, "core", "/can-core", "admin", "bootstrap", "--username", name)
	totpSecret := harness.ParseTOTPSecret(out)
	if totpSecret == "" {
		t.Fatalf("no TOTP secret for the second administrator")
	}
	env.Run(t, "exec", "-T", "core", "/can-core", "admin", "roles", "grant", name, "finance:finance")

	tok := issuePAT(t, adminSession(t), "finance", "finance.read", "finance.write")
	fin.WithToken(tok).Post(t, "/v1/invoices/"+invoiceID+"/request-approval", nil).Expect(t, "request approval through can-core", 200, 201, 202)

	s := harness.NewSession(env)
	login := s.Do(t, harness.Req{Host: host, Path: "/login"}).Expect(t, "login page", 200)
	code, err := harness.TOTP(totpSecret, time.Now())
	if err != nil {
		t.Fatalf("%v", err)
	}
	s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/login", Headers: adminOrigin(),
		Body: url.Values{"csrf": {harness.CSRF(login.String())}, "username": {name}, "password": {pw}, "totp": {code}}}).Expect(t, "second administrator signs in", 303)
	list := s.Do(t, harness.Req{Host: host, Path: "/approvals"}).Expect(t, "approvals page", 200)
	m := regexp.MustCompile(`href="(/approvals/[^"/]+)">Invoice draft ` + regexp.QuoteMeta(invoiceID) + `<`).FindStringSubmatch(list.String())
	if m == nil {
		t.Fatalf("the approver does not see the request for invoice %s: %.600s", invoiceID, list.String())
	}
	page := s.Do(t, harness.Req{Host: host, Path: m[1]}).Expect(t, "approval request page", 200)
	s.Do(t, harness.Req{Method: "POST", Host: host, Path: m[1] + "/decide", Headers: adminOrigin(),
		Body: url.Values{"csrf": {harness.CSRF(page.String())}, "decision": {"approve"}}}).Expect(t, "approve in the admin UI", 303)
}

// (k) A personal access token issued in the admin UI is accepted by work, people, jobs, finance and
// equity, with exactly the permissions chosen, and only by the module it was issued for.
func TestPersonalAccessTokensAgainstModules(t *testing.T) {
	const host = "admin.localhost"
	s := adminSession(t)
	origin := adminOrigin()
	issue := func(module string, perms ...string) string { return issuePAT(t, s, module, perms...) }
	api := func(module, tok string) harness.API {
		return harness.API{C: web, Host: module + ".localhost", Token: tok}
	}
	u := harness.Unique()
	key := "K" + strings.ToUpper(strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return 'A' + (r - '0')
		}
		return r
	}, u[:5]))

	workRead := api("work", issue("work", "work.read"))
	workRead.Get(t, "/v1/projects").Expect(t, "work.read token lists projects", 200)
	workRead.Post(t, "/v1/projects", map[string]any{"key": key, "name": "PAT " + u}).Expect(t, "work.read token cannot create a project", 403)
	api("work", issue("work", "work.admin")).Post(t, "/v1/projects", map[string]any{"key": key, "name": "PAT " + u}).Expect(t, "work.admin token creates a project", 201)

	person := map[string]any{"kind": "employee", "status": "active", "given_name": "Pat", "family_name": u, "work_email": "pat-" + u + "@acme.example"}
	reader := api("people", issue("people", "people.read"))
	reader.Get(t, "/v1/people").Expect(t, "people.read token lists the directory", 200)
	reader.Post(t, "/v1/people", person).Expect(t, "people.read token cannot create a person", 403)
	api("people", issue("people", "people.read", "people.write")).Post(t, "/v1/people", person).Expect(t, "an hr token creates a person", 201)

	jobsTok := issue("jobs", "jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "ats.interview")
	api("jobs", jobsTok).Get(t, "/v1/jobs").Expect(t, "recruiter token lists requisitions", 200)

	party := map[string]any{"kind": "customer", "name": "PAT Customer " + u, "email": "pat-" + u + "@acme.example"}
	api("finance", issue("finance", "finance.read")).Post(t, "/v1/parties", party).Expect(t, "finance.read token cannot create a party", 403)
	api("finance", issue("finance", "finance.read", "finance.write")).Post(t, "/v1/parties", party).Expect(t, "finance.write token creates a party", 201)

	equityAll := []string{"equity.read", "equity.write", "equity.approve", "equity.sensitive-read", "equity.admin"}
	api("equity", issue("equity", equityAll...)).Post(t, "/v1/entities", map[string]any{"name": "PAT Inc " + u, "jurisdiction": "US", "currency": "USD"}).Expect(t, "equity admin token creates an entity", 201)
	api("equity", issue("equity", "equity.read")).Get(t, "/v1/entities").Expect(t, "a partial equity role is no role", 401)

	// A token is for one module only.
	api("people", jobsTok).Get(t, "/v1/people").Expect(t, "a jobs token at people", 401)
	api("finance", jobsTok).Get(t, "/v1/parties").Expect(t, "a jobs token at finance", 401)

	// Revoked before first use (modules cache an introspection for a short while).
	revoked := issue("work", "work.read")
	page := s.Do(t, harness.Req{Host: host, Path: "/tokens"}).Expect(t, "tokens page", 200)
	ids := revokeForm.FindAllStringSubmatch(page.String(), -1)
	if len(ids) == 0 {
		t.Fatalf("no token to revoke on the page")
	}
	for _, id := range ids { // every token of this and earlier runs
		s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/tokens/" + id[1] + "/revoke", Headers: origin,
			Body: url.Values{"csrf": {harness.CSRF(page.String())}}}).Expect(t, "revoke", 303)
	}
	api("work", revoked).Get(t, "/v1/projects").Expect(t, "a revoked token", 401)
}

// (l) The Apps launcher signs the admin in to each module web UI with a one-time ticket, over plain
// http on one host name (docs/sso.md). The link is useless in a browser that did not launch it.
func TestAppsLaunchSignsInToModuleUIs(t *testing.T) {
	const host = "localhost"
	adminPort := strconv.Itoa(env.AppsPort)
	s := harness.NewSession(env)
	adminLoginAt(t, s, host, adminPort)
	apps := s.Do(t, harness.Req{Host: host, Port: adminPort, Path: "/apps"}).Expect(t, "apps page", 200)
	csrf := harness.CSRF(apps.String())

	// can-people's web UI admits only people in its directory: link one to the admin account. A repeat
	// run finds it there already.
	u := harness.Unique()
	peopleAPI(tokenOf(t, "people", "CAN_PEOPLE_API_TOKEN")).Post(t, "/v1/people", map[string]any{"kind": "employee", "status": "active", "given_name": "Admin", "family_name": u,
		"work_email": "admin-" + u + "@acme.example", "sso_subject": "user:admin"}).Expect(t, "link a directory person to the admin account", 201, 409, 422)

	for i, app := range []struct{ id, base string }{{"work", "/ui"}, {"people", ""}, {"jobs", ""}, {"finance", ""}} {
		port := strconv.Itoa(env.AppsPort + 1 + i)
		if !strings.Contains(apps.String(), `action="/apps/`+app.id+`/launch"`) {
			t.Fatalf("Apps does not offer %s: %.400s", app.id, apps.String())
		}
		s.Do(t, harness.Req{Host: host, Port: port, Path: app.base + "/"}).Expect(t, app.id+" UI before the launch", 303)
		r := s.Do(t, harness.Req{Method: "POST", Host: host, Port: adminPort, Path: "/apps/" + app.id + "/launch",
			Headers: map[string]string{"Origin": "http://" + host + ":" + adminPort}, Body: url.Values{"csrf": {csrf}}}).Expect(t, "launch "+app.id, 200)
		m := launchLink.FindStringSubmatch(r.String())
		if m == nil {
			t.Fatalf("no launch address for %s: %.400s", app.id, r.String())
		}
		u, err := url.Parse(html.UnescapeString(m[1]))
		if err != nil || u.Port() != port || u.Path != app.base+"/login/ticket" {
			t.Fatalf("%s: unexpected launch address (path %q, port %q)", app.id, u.Path, u.Port())
		}
		at := func(path string) harness.Req { return harness.Req{Host: host, Port: port, Path: path} }

		harness.NewSession(env).Do(t, at(u.RequestURI())).Expect(t, app.id+": the link in another browser", 401)
		page := s.Do(t, at(u.RequestURI())).Expect(t, app.id+" ticket page", 200)
		post := at(app.base + "/login/ticket")
		post.Method = "POST"
		post.Headers = map[string]string{"Origin": "http://" + host + ":" + port, "Sec-Fetch-Site": "same-origin"}
		post.Body = url.Values{"csrf": {harness.CSRF(page.String())}, "ticket": {u.Query().Get("t")}}
		in := s.Do(t, post).Expect(t, app.id+" ticket sign-in", 303)
		s.Do(t, at(in.Header.Get("Location"))).Expect(t, app.id+" UI signed in", 200)
		s.Do(t, post).Expect(t, app.id+": the ticket works once", 401, 403)
	}

	// Reached by a name that shares no cookie with the modules, launching is refused.
	other := adminSession(t)
	page := other.Do(t, harness.Req{Host: "admin.localhost", Path: "/apps"}).Expect(t, "apps on admin.localhost", 200)
	other.Do(t, harness.Req{Method: "POST", Host: "admin.localhost", Path: "/apps/work/launch",
		Headers: map[string]string{"Origin": "http://admin.localhost:" + env.Port}, Body: url.Values{"csrf": {harness.CSRF(page.String())}}}).
		Expect(t, "launch from admin.localhost", 409)
}

// (m) The module catalogue: disabling a module there (state recording only, the container keeps
// running) stops its personal access tokens, and enabling it again brings them back.
func TestCatalogueDisableAndEnableAModule(t *testing.T) {
	const host = "admin.localhost"
	s := adminSession(t)
	enabled := regexp.MustCompile(`name="enable" value="([a-z-]+)" checked`)
	version := regexp.MustCompile(`name="version" value="([^"]+)"`)
	applyPath := regexp.MustCompile(`action="(/changes/[^"]+/apply)"`)
	set := func(equity bool) {
		cat := s.Do(t, harness.Req{Host: host, Path: "/catalogue"}).Expect(t, "catalogue", 200)
		v := version.FindStringSubmatch(cat.String())
		if v == nil {
			t.Fatalf("no company version on the catalogue page")
		}
		var want []string
		for _, m := range enabled.FindAllStringSubmatch(cat.String(), -1) {
			if m[1] != "equity" {
				want = append(want, m[1])
			}
		}
		if equity {
			want = append(want, "equity")
		}
		r := s.Do(t, harness.Req{Method: "POST", Host: host, Path: "/changes", Headers: adminOrigin(),
			Body: url.Values{"csrf": {harness.CSRF(cat.String())}, "version": {html.UnescapeString(v[1])}, "enable": want, "ack": {"experimental:equity"}}})
		if r.Status == 303 && strings.HasPrefix(r.Header.Get("Location"), "/catalogue") {
			return // already so
		}
		r.Expect(t, "propose the change", 303)
		page := s.Do(t, harness.Req{Host: host, Path: r.Header.Get("Location")}).Expect(t, "change preview", 200)
		a := applyPath.FindStringSubmatch(page.String())
		if a == nil {
			t.Fatalf("the change preview offers no apply: %.600s", page.String())
		}
		s.Do(t, harness.Req{Method: "POST", Host: host, Path: a[1], Headers: adminOrigin(),
			Body: url.Values{"csrf": {harness.CSRF(page.String())}}}).Expect(t, "apply the change", 303)
	}
	offered := func() bool {
		return strings.Contains(s.Do(t, harness.Req{Host: host, Path: "/tokens"}).String(), `value="equity.read"`)
	}
	set(false)
	t.Cleanup(func() { set(true) })
	harness.Wait(t, "equity to be recorded as disabled", time.Minute, func() bool { return !offered() })
	set(true)
	harness.Wait(t, "equity to be recorded as enabled again", time.Minute, offered)
}
