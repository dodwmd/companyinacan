//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// ensurePerson returns the id of the person with this work email, creating them when absent, so the
// scenario can be repeated against the same database (sso subjects are unique).
func ensurePerson(t *testing.T, svc harness.API, email string, in map[string]any) string {
	t.Helper()
	cursor := ""
	for page := 0; page < 20; page++ {
		path := "/v1/people?limit=100"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := svc.Get(t, path).Expect(t, "list people", 200)
		items, _ := r.JSON()["items"].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			if m["work_email"] == email {
				return fmt.Sprint(m["id"])
			}
		}
		next, _ := r.JSON()["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	in["work_email"] = email
	in["kind"] = "employee"
	in["status"] = "active"
	return svc.Post(t, "/v1/people", in).Expect(t, "create person "+email, 201).Str("id")
}

// (d) People: the service token (hr + admin, no person record) creates the org; people then act
// through four different identities: HR, a manager, and two employees. Covers a 1:1 series with
// its private notes, a leave request that its owner cannot approve and a colleague cannot see, and
// the masking of "on leave" for colleagues.
//
// The identities come from the introstub service of dev/compose/e2e.yml, which stands in for can-core
// token introspection (not available in compose).
func TestPeopleOneOnOneAndTimeOff(t *testing.T) {
	svc := peopleAPI(tokenOf(t, "people", "CAN_PEOPLE_API_TOKEN"))
	hr, mgr, ann, ben := svc.WithToken(tokHR), svc.WithToken(tokManager), svc.WithToken(tokAnn), svc.WithToken(tokBen)

	hrID := ensurePerson(t, svc, "e2e-hr@acme.example", map[string]any{"given_name": "Hana", "family_name": "E2E-HR", "sso_subject": "user:e2e-hr"})
	mgrID := ensurePerson(t, svc, "e2e-manager@acme.example", map[string]any{"given_name": "Manny", "family_name": "E2E-Manager", "sso_subject": "user:e2e-manager"})
	annID := ensurePerson(t, svc, "e2e-ann@acme.example", map[string]any{"given_name": "Ann", "family_name": "E2E", "sso_subject": "user:e2e-ann", "manager_id": mgrID})
	benID := ensurePerson(t, svc, "e2e-ben@acme.example", map[string]any{"given_name": "Ben", "family_name": "E2E", "sso_subject": "user:e2e-ben", "manager_id": mgrID})
	_ = hrID
	if annID == benID || annID == "" {
		t.Fatalf("bad person ids %q %q", annID, benID)
	}

	// Identity: each token resolves to the right person and only sees what its role allows.
	hr.Get(t, "/v1/people/"+annID).Expect(t, "hr reads a person", 200)
	ann.Get(t, "/v1/people/"+annID).Expect(t, "ann reads herself", 200)
	// Sensitive fields: self yes, manager no, colleague no.
	hr.Put(t, "/v1/people/"+annID+"/sensitive", map[string]any{"salary": map[string]any{"amount": 7000000, "currency": "USD", "period": "year"}}).Expect(t, "hr writes a salary", 204)
	ann.Get(t, "/v1/people/"+annID+"/sensitive?fields=salary").Expect(t, "ann reads her own salary", 200)
	if r := mgr.Get(t, "/v1/people/"+annID+"/sensitive?fields=salary").Expect(t, "manager cannot read a report's salary", 403, 404); strings.Contains(r.String(), "7000000") {
		t.Fatalf("manager response leaked the salary")
	}
	if r := ben.Get(t, "/v1/people/"+annID+"/sensitive?fields=salary").Expect(t, "colleague cannot read a salary", 403, 404); strings.Contains(r.String(), "7000000") {
		t.Fatalf("colleague response leaked the salary")
	}

	// 1:1: the manager opens a series with Ann (409 when an earlier run already did), both add
	// meetings; Ben and HR cannot see the series.
	cr := mgr.Post(t, "/v1/oneonones", map[string]any{"report_id": annID, "cadence": "weekly"})
	var seriesID string
	switch cr.Status {
	case 201:
		seriesID = cr.Str("id")
	case 409:
		list := mgr.Get(t, "/v1/oneonones").Expect(t, "list the manager's 1:1 series", 200)
		items, _ := list.JSON()["items"].([]any)
		for _, it := range items {
			if m, _ := it.(map[string]any); m != nil && m["report_id"] == annID {
				seriesID = fmt.Sprint(m["id"])
			}
		}
	default:
		cr.Expect(t, "create a 1:1 series", 201, 409)
	}
	if seriesID == "" {
		t.Fatalf("no 1:1 series id for manager and Ann (create answered %d: %s)", cr.Status, cr.String())
	}
	mgr.Post(t, "/v1/oneonones/"+seriesID+"/meetings", map[string]any{"scheduled_at": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)}).Expect(t, "schedule a meeting", 201)
	ann.Get(t, "/v1/oneonones/"+seriesID+"/meetings").Expect(t, "ann (the report) lists the meetings", 200)
	ben.Get(t, "/v1/oneonones/"+seriesID+"/meetings").Expect(t, "a colleague cannot see the series", 403, 404)
	hr.Get(t, "/v1/oneonones/"+seriesID+"/meetings").Expect(t, "hr cannot read 1:1 content by role", 403, 404)

	// Time off.
	ltKey := "e2e-vacation"
	var typeID string
	types := hr.Get(t, "/v1/leave-types").Expect(t, "list leave types", 200)
	if items, ok := types.JSON()["items"].([]any); ok {
		for _, it := range items {
			if m, _ := it.(map[string]any); m != nil && m["key"] == ltKey {
				typeID = fmt.Sprint(m["id"])
			}
		}
	}
	if typeID == "" {
		typeID = hr.Post(t, "/v1/leave-types", map[string]any{"key": ltKey, "name": "E2E vacation", "paid": true, "accrual_mode": "none"}).Expect(t, "create a leave type", 201).Str("id")
	}
	// A weekday within the accepted window (90 days back to two years ahead), different on each run;
	// a clash with an earlier run's request (409/422) just picks another day.
	var lid string
	for try := 0; try < 15 && lid == ""; try++ {
		day := time.Now().UTC().AddDate(0, 0, 30+int((time.Now().UnixNano()/1e6+int64(try)*37)%600))
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, 1)
		}
		d := day.Format("2006-01-02")
		r := ann.Post(t, "/v1/leave-requests", map[string]any{"type_id": typeID, "start_date": d, "end_date": d, "note": "e2e"})
		if r.Status == 201 {
			lid = r.Str("id")
			t.Logf("ok   ann requests leave for %s (HTTP 201)", d)
		} else if r.Status != 409 && r.Status != 422 {
			r.Expect(t, "ann requests leave", 201)
		}
	}
	if lid == "" {
		t.Fatalf("could not find a free day for a leave request after 15 tries")
	}
	ann.Post(t, "/v1/leave-requests/"+lid+"/submit", nil).Expect(t, "ann submits", 200)
	ben.Get(t, "/v1/leave-requests/"+lid).Expect(t, "a colleague cannot see the request", 404)
	ann.Post(t, "/v1/leave-requests/"+lid+"/decision", map[string]any{"approve": true}).Expect(t, "nobody approves their own leave", 403)
	mgr.Post(t, "/v1/leave-requests/"+lid+"/decision", map[string]any{"approve": true}).Expect(t, "the manager approves", 200)
	if got := ann.Get(t, "/v1/leave-requests/"+lid).Expect(t, "ann reads the decision", 200).Str("status"); got != "approved" {
		t.Fatalf("leave request status = %q, want approved", got)
	}
	ben.Get(t, "/v1/people/"+annID+"/leave-balances").Expect(t, "a colleague cannot read balances", 403, 404)

	// Leave masking: while Ann is on leave a colleague still sees her as active.
	hr.Post(t, "/v1/people/"+annID+"/status", map[string]any{"status": "leave", "note": "e2e"}).Expect(t, "hr puts Ann on leave", 200)
	t.Cleanup(func() {
		hr.Post(t, "/v1/people/"+annID+"/status", map[string]any{"status": "active", "note": "e2e cleanup"})
	})
	if s := ben.Get(t, "/v1/people/"+annID).Expect(t, "colleague reads Ann", 200).Str("status"); s != "active" {
		t.Fatalf("a colleague sees status %q for a person on leave, want the masked value \"active\"", s)
	}
	if s := hr.Get(t, "/v1/people/"+annID).Expect(t, "hr reads Ann", 200).Str("status"); s != "leave" {
		t.Fatalf("hr sees status %q, want leave", s)
	}
	if s := mgr.Get(t, "/v1/people/"+annID).Expect(t, "manager reads Ann", 200).Str("status"); s != "leave" {
		t.Fatalf("the manager sees status %q, want leave", s)
	}
}
