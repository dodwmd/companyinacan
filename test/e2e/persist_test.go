//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// (i) Restart persistence: write something into every service that has a database, take the whole
// stack down WITHOUT -v (named volumes survive), bring it up again, and read it all back, including a
// fresh admin login with the TOTP secret created before the restart. Set E2E_RESTART=0 to skip (for
// example when the stack is not managed by the compose command in E2E_COMPOSE).
func TestRestartKeepsData(t *testing.T) {
	if os.Getenv("E2E_RESTART") == "0" {
		t.Skip("E2E_RESTART=0")
	}
	u := harness.Unique()
	adminSecret(t) // rotate once before the restart; the same secret must still work after it

	// Write.
	work := workAPI(t)
	key := "P" + strings.ToUpper(strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return 'A' + (r - '0')
		}
		return r
	}, u[:5]))
	work.Post(t, "/v1/projects", map[string]any{"key": key, "name": "Persist " + u}).Expect(t, "work: create project", 201)
	work.Post(t, "/v1/projects/"+key+"/issues", map[string]any{"type": "task", "title": "survives restart " + u}).Expect(t, "work: create issue", 201)

	fin := financeAPI(t)
	party := fin.Post(t, "/v1/parties", map[string]any{"kind": "customer", "name": "Persist Customer " + u, "email": "p-" + u + "@acme.example"}).Expect(t, "finance: create party", 201).Str("id")

	people := peopleAPI(tokenOf(t, "people", "CAN_PEOPLE_API_TOKEN"))
	pid := people.Post(t, "/v1/people", map[string]any{"kind": "employee", "status": "active", "given_name": "Persist", "family_name": u, "work_email": "persist-" + u + "@acme.example"}).Expect(t, "people: create person", 201).Str("id")

	jobs := jobsAPI(t)
	job := jobs.Post(t, "/v1/jobs", map[string]any{"title": "Persist Role " + u, "department": "Eng", "location": map[string]any{"country": "US"}, "remote": "remote", "employment_type": "full_time",
		"hiring_manager": "hm", "headcount": 1, "sections": []map[string]string{{"heading": "About", "body": "x"}}}).Expect(t, "jobs: create requisition", 201).Str("id")

	eq := equityAPI(tokenOf(t, "equity", "CAN_EQUITY_API_TOKEN"))
	ent := eq.Post(t, "/v1/entities", map[string]any{"name": "Persist Inc " + u, "jurisdiction": "US", "currency": "USD"}).Expect(t, "equity: create entity", 201).Str("id")

	// Restart without deleting volumes.
	t.Logf("taking the stack down (no -v) and up again; this takes a while")
	env.Run(t, "down")
	env.Run(t, "up", "-d")
	harness.Wait(t, "the edge and every module to answer after the restart", 3*time.Minute, func() bool {
		for _, h := range []string{"work.localhost", "people.localhost", "jobs.localhost", "finance.localhost", "equity.localhost"} {
			if tryGet(h, "/healthz") != 200 {
				return false
			}
		}
		return tryGet("admin.localhost", "/login") == 200 && tryGet("localhost", "/") == 200
	})

	// Read back.
	if !strings.Contains(work.Get(t, "/v1/issues?project="+key).Expect(t, "work: issues after restart", 200).String(), "survives restart "+u) {
		t.Errorf("work: the issue is gone after a restart")
	}
	if !strings.Contains(fin.Get(t, "/v1/parties/"+party).Expect(t, "finance: party after restart", 200).String(), "Persist Customer "+u) {
		t.Errorf("finance: the party is gone after a restart")
	}
	if got := people.Get(t, "/v1/people/"+pid).Expect(t, "people: person after restart", 200).Str("family_name"); got != u {
		t.Errorf("people: family_name = %q after restart, want %q", got, u)
	}
	jobs.Get(t, "/v1/jobs/"+job).Expect(t, "jobs: requisition after restart", 200)
	eq.Get(t, "/v1/entities/"+ent).Expect(t, "equity: entity after restart", 200)

	// The admin account, its password and its TOTP secret survived: log in again.
	adminLogin(t, harness.NewSession(env))
}
