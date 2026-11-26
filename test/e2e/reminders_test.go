//go:build e2e

package e2e

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// notified waits until the admin's inbox (admin UI) matches want.
func notified(t *testing.T, what string, want *regexp.Regexp) string {
	t.Helper()
	var page string
	// A module delivers its outbox every 15 s and can-core caches the notification rules for 30 s.
	harness.Wait(t, what+" in the admin's notifications", 2*time.Minute, func() bool {
		r := adminSession(t).Do(t, harness.Req{Host: "admin.localhost", Path: "/notifications"})
		page = r.String()
		return r.Status == 200 && want.MatchString(page)
	})
	return page
}

// Reminders (specs/060-reminders): an overdue invoice and a new job application reach the people
// holding the role, through the real modules, can-core's event rules and the admin inbox.
func TestRemindersReachTheInbox(t *testing.T) {
	u := harness.Unique()
	// Other scenarios give these roles to their own accounts, so say who is to be told here.
	for _, role := range []string{"finance:finance", "jobs:recruiter"} {
		env.Run(t, "exec", "-T", "core", "/can-core", "admin", "roles", "grant", "admin", role)
	}

	// A backdated invoice that fell due yesterday, approved.
	day := func(ago int) string { return time.Now().UTC().AddDate(0, 0, -ago).Format("2006-01-02") }
	fin := financeAPI(t)
	party := fin.Post(t, "/v1/parties", map[string]any{"kind": "customer", "name": "Late Payer " + u, "email": "late-" + u + "@acme.example"}).Expect(t, "create party", 201).Str("id")
	id := fin.Post(t, "/v1/invoices", map[string]any{"party_id": mustInt(t, party), "currency": "USD", "issue_date": day(2), "due_date": day(1),
		"lines": []map[string]any{{"description": "Anvil " + u, "quantity_milli": 1000, "unit_price_minor": 4900}},
	}).Expect(t, "create invoice", 201).Str("id")
	approver := fin.WithToken(fin.Post(t, "/v1/tokens", map[string]any{"kind": "service", "subject": "e2e-late-" + u, "name": "e2e late " + u, "scopes": []string{"finance.approve", "finance.read"}}).
		Expect(t, "mint approver token", 201, 200).Str("token"))
	approveInCanCore(t, fin, id, "late"+u)
	harness.Wait(t, "the approval decision to reach finance", time.Minute, func() bool {
		return approver.Post(t, "/v1/invoices/"+id+"/approve", nil).Status == 200
	})
	// The hourly sweep may have got there first; either way a second sweep finds nothing.
	fin.Post(t, "/v1/invoices/overdue-sweep", map[string]any{}).Expect(t, "sweep overdue invoices", 200)
	again := fin.Post(t, "/v1/invoices/overdue-sweep", map[string]any{}).Expect(t, "sweep a second time", 200)
	if n, _ := again.JSON()["invoices"].(float64); n != 0 {
		t.Fatalf("a second sweep announced invoices again: %s", again.String())
	}
	// On a stack with a backlog of overdue invoices the sweep sends one summary instead.
	page := notified(t, "the overdue invoice", regexp.MustCompile(`Invoice `+regexp.QuoteMeta(id)+` is past its due date|[0-9]+ invoices are past their due date`))
	if strings.Contains(page, "Late Payer "+u) {
		t.Errorf("a notification names the customer")
	}

	// A candidate applies through the public form.
	jobs := jobsAPI(t)
	job := jobs.Post(t, "/v1/jobs", map[string]any{
		"title": "Reminder Engineer " + u, "department": "Engineering", "location": map[string]any{"city": "Austin", "region": "TX", "country": "US"},
		"remote": "hybrid", "employment_type": "full_time", "hiring_manager": "hm-" + u, "headcount": 1,
		"sections": []map[string]string{{"heading": "About", "body": "Build things."}},
	}).Expect(t, "create requisition", 201)
	jobID, slug := job.Str("id"), job.Str("slug")
	jobs.Post(t, "/v1/jobs/"+jobID+"/transitions", map[string]any{"to": "open"}).Expect(t, "open the requisition", 200)
	careers := web.Do(t, harness.Req{Host: "jobs.localhost", Path: "/careers/" + slug}).Expect(t, "careers page", 200)
	m := timingToken.FindStringSubmatch(careers.String())
	if m == nil {
		t.Fatalf("no timing token in the apply form")
	}
	time.Sleep(3 * time.Second)
	mp, ctype := multipartBody(t, map[string]string{"_t": m[1], "name": "Remy Applicant " + u, "email": "remy-" + u + "@example.test", "phone": "+1 555 0101", "cover_letter": "Hello.", "consent": "on", "website": ""})
	web.Do(t, harness.Req{Method: "POST", Host: "jobs.localhost", Path: "/careers/" + slug + "/apply", Body: mp,
		Headers: map[string]string{"Content-Type": ctype, "Origin": "http://jobs.localhost:" + env.Port}}).Expect(t, "apply", 303)
	page = notified(t, "the new application", regexp.MustCompile(`arrived for job `+regexp.QuoteMeta(jobID)))
	if strings.Contains(page, "Remy Applicant") || strings.Contains(page, "remy-"+u) {
		t.Errorf("a notification names the candidate")
	}
}
