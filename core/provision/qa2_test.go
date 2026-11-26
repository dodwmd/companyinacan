package provision

import (
	"context"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// waitDelays ticks until the revoke_core step has waited n times and returns the delays it was given.
func waitDelays(t *testing.T, h *harness, n int) (delays []time.Duration, step Step) {
	t.Helper()
	for i := 0; i < n; i++ {
		h.tick()
		run := h.onlyRun()
		for _, s := range run.Steps {
			if s.Op == opRevokeCore {
				step = s
			}
		}
		next, _ := time.Parse(time.RFC3339, step.NextAt)
		delays = append(delays, next.Sub(h.clk.now()))
		h.clk.add(next.Sub(h.clk.now()))
	}
	return delays, step
}

func wantDelays(t *testing.T, got []time.Duration) {
	t.Helper()
	want := []time.Duration{10, 20, 40, 60, 60, 60} // base 10 s doubling, capped at max_delay 60 s
	for i, g := range got {
		if g != want[i]*time.Second {
			t.Fatalf("delays %v, want exponential and capped %v", got, want[:len(got)])
		}
	}
}

// Waiting for the directory to say "former" never dead-letters the kill switch and backs off
// exponentially to the cap, on its own counter.
func TestNotOffboardedYetWaitsExponentiallyWithoutUsingAttempts(t *testing.T) {
	h := newHarness(t, nil)
	h.linkAccounts(alice, "app-a", "app-b")
	h.dir.put(person(alice, "offboarding", "alice@example.com"))
	h.mustEvent(EventPersonStatusChanged, statusChanged(alice, "active", "offboarding", "2026-06-10"))
	if err := h.eng.FlagUrgent(context.Background(), "user:hr", alice); err != nil {
		t.Fatal(err)
	}
	delays, step := waitDelays(t, h, 6)
	wantDelays(t, delays)
	if step.Attempts != 0 || step.State == StepDead {
		t.Fatalf("step %+v", step)
	}
	p := person(alice, "former", "alice@example.com")
	h.dir.put(p)
	h.clk.add(2 * time.Minute)
	h.settle()
	if len(h.core.calls) != 1 || h.onlyRun().State != RunSucceeded {
		t.Fatalf("calls %d state %s", len(h.core.calls), h.onlyRun().State)
	}
}

func TestOutageBackoffIsExponentialAndCapped(t *testing.T) {
	h := newHarness(t, nil)
	h.linkAccounts(alice, "app-a", "app-b")
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.core.err = modclient.ErrUnavailable
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	delays, step := waitDelays(t, h, 6)
	wantDelays(t, delays)
	if step.Attempts != 0 {
		t.Fatalf("attempts %d", step.Attempts)
	}
}

// A retried dead delete asks for a fresh approval: the old request and its decision are not reused.
func TestRetryRequestsAFreshApproval(t *testing.T) {
	h := newHarness(t, func(p *Policy) { p.DeleteAfterDays = 1 })
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.linkAccounts(alice, "app-a")
	h.accountGone(alice, "app-b")
	h.addGroup("admins", alice)
	h.a.permFail[opDeleteAccount] = "http_400"
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	h.settle()
	h.clk.add(48 * time.Hour)
	h.settle()
	approve := func() {
		t.Helper()
		var id string
		for _, s := range h.onlyRun().Steps {
			if s.State == StepAwaiting {
				id = s.ApprovalID
			}
		}
		if id == "" {
			t.Fatal("no step awaits an approval")
		}
		h.mustEvent(EventApprovalDecided, map[string]any{"request_id": id, "type": "privileged-leaver", "outcome": "approved", "approvers": []string{"user:boss"}})
		h.settle()
	}
	approve()
	run := h.onlyRun()
	if run.State != RunFailed || h.a.count("delete_account") != 1 {
		t.Fatalf("state %s deletes %d", run.State, h.a.count("delete_account"))
	}
	if err := h.eng.RetryRun(context.Background(), "user:admin", run.ID); err != nil {
		t.Fatal(err)
	}
	h.a.permFail = map[string]string{}
	h.settle()
	if len(h.appr.created) != 2 || h.appr.created[0].Key == h.appr.created[1].Key {
		t.Fatalf("approvals %+v", h.appr.created)
	}
	if h.a.count("delete_account") != 1 {
		t.Fatal("the retried delete ran on the old approval")
	}
	approve()
	if h.a.count("delete_account") != 2 || h.onlyRun().State != RunSucceeded {
		t.Fatalf("deletes %d state %s", h.a.count("delete_account"), h.onlyRun().State)
	}
}

// A user PATCH is audited before the directory is touched, and the outcome after.
func TestSCIMUserPatchAuditsIntentThenOutcome(t *testing.T) {
	e := newSCIMEnv(t, SCIMConfig{})
	resp, _, _ := e.do("PATCH", "/scim/v2/Users/"+alice, e.write, "", patchBody(map[string]any{"op": "replace", "path": "displayName", "value": "Al"}))
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	recs, _ := e.st.AuditList(context.Background(), 0, 100)
	var begin, done int64
	for _, r := range recs {
		switch r.Action {
		case "scim.user.patch.begin":
			begin = r.Seq
		case "scim.user.patch":
			done = r.Seq
		}
	}
	if begin == 0 || done <= begin {
		t.Fatalf("begin %d outcome %d", begin, done)
	}
}
