//go:build e2e

package e2e

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

const equityCore = "equity-core.localhost"

// account is a can-core user made for one run, with its authenticator secret.
type account struct{ name, secret string }

// newAccount makes a user with no role (the first account of an installation is its owner; later
// ones have nothing).
func newAccount(t *testing.T, name string) account {
	t.Helper()
	pw := env.Secret(t, "core", "CAN_ADMIN_PASSWORD")
	out := env.Run(t, "exec", "-T", "-e", "CAN_ADMIN_PASSWORD="+pw, "core", "/can-core", "admin", "bootstrap", "--username", name)
	secret := harness.ParseTOTPSecret(out)
	if secret == "" {
		t.Fatalf("no TOTP secret for %s", name)
	}
	return account{name, secret}
}

// signIn is a new admin UI session of the account. Each account signs in once per code step.
func (a account) signIn(t *testing.T, host, port string) *harness.Client {
	t.Helper()
	s := harness.NewSession(env)
	page := s.Do(t, harness.Req{Host: host, Port: port, Path: "/login"}).Expect(t, "login page", 200)
	code, err := harness.TOTP(a.secret, time.Now())
	if err != nil {
		t.Fatalf("%v", err)
	}
	s.Do(t, harness.Req{Method: "POST", Host: host, Port: port, Path: "/login", Headers: map[string]string{"Origin": "http://" + host + ":" + port},
		Body: url.Values{"csrf": {harness.CSRF(page.String())}, "username": {a.name}, "password": {env.Secret(t, "core", "CAN_ADMIN_PASSWORD")}, "totp": {code}}}).
		Expect(t, a.name+" signs in", 303)
	return s
}

// roleChange assigns or removes a role on the users page, as the owner.
func roleChange(t *testing.T, owner *harness.Client, user, action, role string) {
	t.Helper()
	const host = "admin.localhost"
	page := owner.Do(t, harness.Req{Host: host, Path: "/users/" + user}).Expect(t, "user page", 200)
	owner.Do(t, harness.Req{Method: "POST", Host: host, Path: "/users/" + user + "/roles/" + action, Headers: adminOrigin(),
		Body: url.Values{"csrf": {harness.CSRF(page.String())}, "role": {role}, "scope": {""}}}).Expect(t, action+" "+role+" of "+user, 303)
}

// (n) Equity under can-core, with no fake identities: the owner gives accounts the equity roles in the
// admin UI, their can-core tokens and Apps sign-ins carry them, and a grant is drafted by HR and
// approved by a director, through the API and through the web UI. Nobody else can approve it, the
// holder least of all, and taking the board role away ends it.
func TestEquityBoardApprovalThroughCanCore(t *testing.T) {
	const adminHost = "admin.localhost"
	u := harness.Unique()
	hr, chair, director, admin := newAccount(t, "eqhr"+u), newAccount(t, "eqchair"+u), newAccount(t, "eqdir"+u), newAccount(t, "eqadm"+u)

	owner := harness.NewSession(env)
	adminLogin(t, owner) // a fresh sign-in: assigning a role needs a recent one
	roleChange(t, owner, hr.name, "grant", "equity:hr")
	roleChange(t, owner, chair.name, "grant", "equity:board")
	roleChange(t, owner, director.name, "grant", "equity:board")
	roleChange(t, owner, admin.name, "grant", "equity:admin")

	people := peopleAPI(tokenOf(t, "people", "CAN_PEOPLE_API_TOKEN"))
	person := func(name, subject string) string {
		body := map[string]any{"kind": "employee", "status": "active", "given_name": "Eq", "family_name": name, "work_email": name + "@acme.example"}
		if subject != "" {
			body["sso_subject"] = "user:" + subject
		}
		return people.Post(t, "/v1/people", body).Expect(t, "directory person "+name, 201).Str("id")
	}
	person(hr.name, hr.name)
	person(chair.name, chair.name)
	directorPerson := person(director.name, director.name)
	person(admin.name, admin.name)
	employee := person("eqemp"+u, "")

	boardPerms := []string{"equity.board", "equity.read", "equity.approve", "equity.sensitive-read", "equity.admin"}
	api := func(a account, perms ...string) harness.API {
		return equityCoreAPI(issuePAT(t, a.signIn(t, adminHost, env.Port), "equity", perms...))
	}
	hrAPI := api(hr, "equity.read", "equity.write", "equity.sensitive-read", "equity.self-read")
	directorAPI := api(director, boardPerms...)
	adminSess := admin.signIn(t, adminHost, env.Port)
	adminAPI := equityCoreAPI(issuePAT(t, adminSess, "equity", "equity.read", "equity.write", "equity.approve", "equity.sensitive-read", "equity.admin"))
	// an equity administrator cannot put the board's permission into a token
	page := adminSess.Do(t, harness.Req{Host: adminHost, Path: "/tokens"}).Expect(t, "tokens page", 200)
	adminSess.Do(t, harness.Req{Method: "POST", Host: adminHost, Path: "/tokens", Headers: adminOrigin(),
		Body: url.Values{"csrf": {harness.CSRF(page.String())}, "name": {"no " + u}, "module": {"equity"}, "permissions": boardPerms, "ttl_hours": {"24"}}}).
		Expect(t, "an administrator without equity.board asks for it", 400, 403)
	// the chair signs in once at the Apps address and uses that session for the token and the launch
	appsPort := strconv.Itoa(env.AppsPort)
	chairSess := chair.signIn(t, "localhost", appsPort)
	chairAPI := equityCoreAPI(issuePATAt(t, chairSess, "localhost", appsPort, "equity", boardPerms...))

	svc := equityCoreAPI(tokenOf(t, "equity", "CAN_EQUITY_API_TOKEN"))
	ent := svc.Post(t, "/v1/entities", map[string]any{"name": "Board Inc " + u, "jurisdiction": "US", "currency": "USD"}).Expect(t, "create entity", 201).Str("id")
	class := svc.Post(t, "/v1/entities/"+ent+"/share-classes", map[string]any{"name": "Common", "kind": "common", "par_minor": 1, "currency": "USD", "authorised": "10000000"}).Expect(t, "share class", 201).Str("id")
	plan := svc.Post(t, "/v1/entities/"+ent+"/plans", map[string]any{"name": "Plan " + u, "class_id": class, "authorised": "1000000", "adopted_on": "2026-01-02"}).Expect(t, "plan", 201).Str("id")

	holder := func(personID, name string) string {
		id := hrAPI.Post(t, "/v1/entities/"+ent+"/stakeholders", map[string]any{"kind": "employee", "person_id": personID, "name": name}).Expect(t, "HR registers "+name, 201).Str("id")
		chairAPI.Post(t, "/v1/stakeholders/"+id+"/verify-person", nil).Expect(t, "a director confirms "+name, 200)
		return id
	}
	pending := func(stk string) string {
		id := hrAPI.Post(t, "/v1/plans/"+plan+"/grants", map[string]any{
			"stakeholder_id": stk, "type": "NSO", "quantity": "4800", "strike_minor": 125, "currency": "USD", "grant_date": "2026-01-15", "vesting_start": "2026-01-15",
			"schedule": map[string]any{"kind": "periodic", "months": 48, "cliff_months": 12, "interval": "monthly"}, "expiry_date": "2036-01-15",
		}).Expect(t, "HR drafts a grant", 201).Str("id")
		hrAPI.Post(t, "/v1/grants/"+id+"/submit", nil).Expect(t, "HR submits", 200)
		return id
	}
	status := func(id string) string {
		return hrAPI.Get(t, "/v1/grants/"+id).Expect(t, "read the grant", 200).Str("status")
	}
	empStk := holder(employee, "Employee "+u)

	first := pending(empStk)
	hrAPI.Post(t, "/v1/grants/"+first+"/approve", nil).Expect(t, "HR cannot approve", 403)
	adminAPI.Post(t, "/v1/grants/"+first+"/approve", nil).Expect(t, "an equity administrator cannot approve", 403)
	svc.Post(t, "/v1/grants/"+first+"/approve", nil).Expect(t, "the service account cannot approve", 403)
	own := pending(holder(directorPerson, "Director "+u))
	if r := directorAPI.Post(t, "/v1/grants/"+own+"/approve", nil).Expect(t, "a director cannot approve their own grant", 403); !strings.Contains(r.String(), "separation") {
		t.Fatalf("refused for another reason: %.300s", r.String())
	}
	chairAPI.Post(t, "/v1/grants/"+first+"/approve", nil).Expect(t, "a director approves with a can-core token", 200)
	if got := status(first); got != "approved" {
		t.Fatalf("status %q", got)
	}

	// The same in the web UI, signed in from Apps. Apps points at the stack's other equity service,
	// so the ticket and the launching browser's cookie are carried over to this one.
	second := pending(empStk)
	apps := chairSess.Do(t, harness.Req{Host: "localhost", Port: appsPort, Path: "/apps"}).Expect(t, "apps page", 200)
	launch := chairSess.Do(t, harness.Req{Method: "POST", Host: "localhost", Port: appsPort, Path: "/apps/equity/launch",
		Headers: map[string]string{"Origin": "http://localhost:" + appsPort}, Body: url.Values{"csrf": {harness.CSRF(apps.String())}}}).Expect(t, "launch equity", 200)
	link := launchLink.FindStringSubmatch(launch.String())
	binding := regexp.MustCompile(`^[^=]+=[^;]+`).FindString(launch.Header.Get("Set-Cookie"))
	if link == nil || binding == "" {
		t.Fatalf("no launch address or cookie")
	}
	to, err := url.Parse(html.UnescapeString(link[1]))
	if err != nil {
		t.Fatalf("launch address: %v", err)
	}
	ui := harness.NewSession(env)
	ticket := ui.Do(t, harness.Req{Host: equityCore, Path: to.RequestURI(), Headers: map[string]string{"Cookie": binding}}).Expect(t, "ticket page", 200)
	origin := map[string]string{"Origin": "http://" + equityCore + ":" + env.Port, "Sec-Fetch-Site": "same-origin"}
	ui.Do(t, harness.Req{Method: "POST", Host: equityCore, Path: "/login/ticket",
		Headers: map[string]string{"Origin": origin["Origin"], "Sec-Fetch-Site": "same-origin", "Cookie": binding},
		Body:    url.Values{"csrf": {harness.CSRF(ticket.String())}, "ticket": {to.Query().Get("t")}}}).Expect(t, "ticket sign-in", 303)
	grantPage := ui.Do(t, harness.Req{Host: equityCore, Path: "/company/grants/" + second}).Expect(t, "the grant in the UI", 200)
	if !strings.Contains(grantPage.String(), `action="/company/grants/`+second+`/approve"`) {
		t.Fatalf("the director is not offered approval: %.600s", grantPage.String())
	}
	ui.Do(t, harness.Req{Method: "POST", Host: equityCore, Path: "/company/grants/" + second + "/approve", Headers: origin,
		Body: url.Values{"csrf": {harness.CSRF(grantPage.String())}, "confirm": {"yes"}}}).Expect(t, "approve in the UI after the step-up", 200)
	if got := status(second); got != "approved" {
		t.Fatalf("status %q", got)
	}

	// Taking the role away ends the director's tokens.
	roleChange(t, owner, chair.name, "revoke", "equity:board")
	third := pending(empStk)
	harness.Wait(t, "the removed director's token to stop working", time.Minute, func() bool {
		return chairAPI.Get(t, "/v1/me/identity").Status == 401
	})
	chairAPI.Post(t, "/v1/grants/"+third+"/approve", nil).Expect(t, "after the role was removed", 401)
	if got := status(third); got != "pending_approval" {
		t.Fatalf("status %q", got)
	}
}
