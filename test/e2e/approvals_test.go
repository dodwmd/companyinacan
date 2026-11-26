//go:build e2e

package e2e

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// approverSession signs in an administrator of this name, creating the account (or, when an earlier
// run did, giving it a new TOTP secret), and grants it the roles.
func approverSession(t *testing.T, name string, roles ...string) *harness.Client {
	t.Helper()
	pw := env.Secret(t, "core", "CAN_ADMIN_PASSWORD")
	run := func(args ...string) (out string) {
		defer func() {
			if recover() != nil {
				out = ""
			}
		}()
		return env.Run(&quietT{}, append([]string{"exec", "-T", "-e", "CAN_ADMIN_PASSWORD=" + pw, "core", "/can-core", "admin"}, args...)...)
	}
	out := run("bootstrap", "--username", name)
	if harness.ParseTOTPSecret(out) == "" {
		out = env.Run(t, "exec", "-T", "-e", "CAN_ADMIN_PASSWORD="+pw, "core", "/can-core", "admin", "reset-password", "--username", name, "--rotate-totp")
	}
	secret := harness.ParseTOTPSecret(out)
	if secret == "" {
		t.Fatalf("no TOTP secret for %s", name)
	}
	for _, role := range roles {
		env.Run(t, "exec", "-T", "core", "/can-core", "admin", "roles", "grant", name, role)
	}
	s := harness.NewSession(env)
	page := s.Do(t, harness.Req{Host: "admin.localhost", Path: "/login"}).Expect(t, "login page", 200)
	code, err := harness.TOTP(secret, time.Now())
	if err != nil {
		t.Fatalf("%v", err)
	}
	s.Do(t, harness.Req{Method: "POST", Host: "admin.localhost", Path: "/login", Headers: adminOrigin(),
		Body: url.Values{"csrf": {harness.CSRF(page.String())}, "username": {name}, "password": {pw}, "totp": {code}}}).Expect(t, name+" signs in", 303)
	return s
}

// inInbox waits for the request with this title in the session's inbox and returns its page address.
func inInbox(t *testing.T, s *harness.Client, title string) string {
	t.Helper()
	want := regexp.MustCompile(`href="(/approvals/[^"/]+)">` + regexp.QuoteMeta(title) + `<`)
	var path string
	harness.Wait(t, title+" in the approvals inbox", time.Minute, func() bool {
		r := s.Do(t, harness.Req{Host: "admin.localhost", Path: "/approvals"})
		if m := want.FindStringSubmatch(r.String()); r.Status == 200 && m != nil {
			path = m[1]
		}
		return path != ""
	})
	return path
}

func decideInInbox(t *testing.T, s *harness.Client, path, decision string) {
	t.Helper()
	page := s.Do(t, harness.Req{Host: "admin.localhost", Path: path}).Expect(t, "approval request page", 200)
	s.Do(t, harness.Req{Method: "POST", Host: "admin.localhost", Path: path + "/decide", Headers: adminOrigin(),
		Body: url.Values{"csrf": {harness.CSRF(page.String())}, "decision": {decision}}}).Expect(t, decision+" in the admin UI", 303)
}

func reaches(t *testing.T, api harness.API, path, status string) {
	t.Helper()
	harness.Wait(t, path+" to become "+status, time.Minute, func() bool {
		return api.Get(t, path).Str("status") == status
	})
}

// (specs/070-approvals-inbox) A timesheet, an expense and a purchase order wait in the admin UI's
// approvals inbox, cannot be approved in finance, and take the decision made there.
func TestFinanceRecordsAreApprovedInTheInbox(t *testing.T) {
	u := harness.Unique()
	approver := approverSession(t, "e2eap"+u, "finance:approver")
	fin := financeAPI(t)
	boss := fin.WithToken(fin.Post(t, "/v1/tokens", map[string]any{"kind": "service", "subject": "e2e-boss-" + u, "name": "e2e boss " + u, "scopes": []string{"finance.approve", "finance.read"}}).
		Expect(t, "mint an approver token", 201, 200).Str("token"))

	// Expense: approved in the inbox.
	today := time.Now().UTC().Format("2006-01-02")
	ex := fin.Post(t, "/v1/expenses", map[string]any{"description": "Taxi " + u, "amount_minor": 1850, "currency": "USD", "category": "travel", "incurred_on": today}).
		Expect(t, "create an expense", 201).Str("id")
	fin.Post(t, "/v1/expenses/"+ex+"/submit", nil).Expect(t, "submit the expense", 200)
	boss.Post(t, "/v1/expenses/"+ex+"/approve", nil).Expect(t, "finance does not approve around the engine", 409)
	page := inInbox(t, approver, "Expense "+ex)
	body := approver.Do(t, harness.Req{Host: "admin.localhost", Path: page}).Expect(t, "request page", 200).String()
	if strings.Contains(body, "Taxi "+u) {
		t.Errorf("the approval request holds the expense's description")
	}
	if !strings.Contains(body, `action="/apps/finance/launch"`) || !strings.Contains(body, `value="/expenses/`+ex+`"`) {
		t.Errorf("no link to the expense in finance: %.900s", body)
	}
	// The engine told the approver; no second reminder comes from the module's event.
	notes := notifiedSession(t, approver, regexp.MustCompile(`Approval needed: expense`))
	if strings.Contains(notes, "expense.submitted") {
		t.Errorf("the approver was told twice")
	}
	decideInInbox(t, approver, page, "approve")
	reaches(t, fin, "/v1/expenses/"+ex, "approved")

	// Timesheet: sent back in the inbox.
	d := time.Now().UTC()
	monday := d.AddDate(0, 0, -int((d.Weekday()+6)%7)).Format("2006-01-02")
	sh := fin.Post(t, "/v1/timesheets", map[string]any{"owner": "e2e-emp-" + u, "week_start": monday}).Expect(t, "open a timesheet", 201).Str("id")
	fin.Put(t, "/v1/timesheets/"+sh+"/entries", map[string]any{"entries": []map[string]any{{"day": monday, "project": "ACME", "minutes": 90}}}).Expect(t, "enter time", 200)
	fin.Post(t, "/v1/timesheets/"+sh+"/submit", nil).Expect(t, "submit the timesheet", 200)
	boss.Post(t, "/v1/timesheets/"+sh+"/approve", nil).Expect(t, "finance does not approve around the engine", 409)
	decideInInbox(t, approver, inInbox(t, approver, "Timesheet "+sh+", week of "+monday), "reject")
	reaches(t, fin, "/v1/timesheets/"+sh, "rejected")

	// Purchase order: requested, approved in the inbox, numbered by finance.
	vendor := fin.Post(t, "/v1/parties", map[string]any{"kind": "vendor", "name": "E2E Vendor " + u, "email": "vendor-" + u + "@acme.example"}).Expect(t, "create a vendor", 201).Str("id")
	po := fin.Post(t, "/v1/purchase-orders", map[string]any{"vendor_id": mustInt(t, vendor), "currency": "USD",
		"lines": []map[string]any{{"description": "Anvil " + u, "quantity_milli": 1000, "unit_price_minor": 4900}}}).Expect(t, "draft a purchase order", 201).Str("id")
	boss.Post(t, "/v1/purchase-orders/"+po+"/approve", nil).Expect(t, "not before the engine", 409)
	tok := issuePAT(t, adminSession(t), "finance", "finance.read", "finance.write")
	fin.WithToken(tok).Post(t, "/v1/purchase-orders/"+po+"/request-approval", nil).Expect(t, "ask for approval", 200)
	decideInInbox(t, approver, inInbox(t, approver, "Purchase order draft "+po), "approve")
	reaches(t, fin, "/v1/purchase-orders/"+po, "approved")
	if fin.Get(t, "/v1/purchase-orders/"+po).Str("number") == "" {
		t.Fatalf("an approved purchase order has a number")
	}
}

// notifiedSession waits until the session's notifications match want.
func notifiedSession(t *testing.T, s *harness.Client, want *regexp.Regexp) string {
	t.Helper()
	var page string
	harness.Wait(t, want.String()+" in the notifications", time.Minute, func() bool {
		r := s.Do(t, harness.Req{Host: "admin.localhost", Path: "/notifications"})
		page = r.String()
		return r.Status == 200 && want.MatchString(page)
	})
	return page
}
