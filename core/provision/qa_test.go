package provision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

func boolPtr(b bool) *bool { return &b }

// scim.write on a group must not by itself grant downstream access: groups sync only to
// connectors that opt in.
func TestGroupsSyncOnlyToConnectorsThatOptIn(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.addGroup("finance-admins", alice)
	h.mustEvent(EventPersonCreated, created(alice, "active"))
	h.settle()
	if got := h.b.count("add_group"); got != 0 {
		t.Fatalf("app-b (no opt-in) received %d group memberships", got)
	}
	if h.a.count("add_group:finance-admins") != 1 {
		t.Fatalf("app-a (opted in) ops %v", h.a.ops())
	}
	if (ConnConfig{}).syncGroups() {
		t.Fatal("sync_groups must default to false")
	}
}

func TestLeaverCutoffEndOfDay(t *testing.T) {
	for _, tc := range []struct {
		cutoff string
		now    time.Time // 2026-06-12 is the last day
		want   int
	}{
		{"", time.Date(2026, 6, 12, 1, 0, 0, 0, time.UTC), 1},
		{"end_of_day", time.Date(2026, 6, 12, 23, 0, 0, 0, time.UTC), 0},
		{"end_of_day", time.Date(2026, 6, 13, 0, 5, 0, 0, time.UTC), 1},
	} {
		h := newHarness(t, func(p *Policy) { p.EndOfDay = tc.cutoff == "end_of_day" })
		h.clk.t = tc.now
		p := person(alice, "offboarding", "alice@example.com")
		p.EndDate = "2026-06-12"
		h.dir.put(p)
		h.linkAccounts(alice, "app-a")
		h.mustEvent(EventPersonStatusChanged, statusChanged(alice, "active", "offboarding", "2026-06-01"))
		h.settle()
		if got := h.a.count("disable_account"); got != tc.want {
			t.Errorf("cutoff %q at %s: %d disables, want %d", tc.cutoff, tc.now, got, tc.want)
		}
	}
	cfg := &Config{Workflows: WorkflowConfig{LeaverCutoff: "noon"}}
	if _, err := cfg.Policy(); err == nil {
		t.Error("an unknown cutoff was accepted")
	}
}

// can-core being down or rate limiting never uses up the attempts of the kill-switch step.
func TestCoreUnavailableDoesNotConsumeAttempts(t *testing.T) {
	h := newHarness(t, nil) // MaxAttempts 3
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.core.err = modclient.ErrUnavailable
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	for i := 0; i < 8; i++ {
		h.settle()
		h.clk.add(2 * time.Minute)
	}
	r := h.onlyRun()
	for _, s := range r.Steps {
		if s.Op == opRevokeCore && (s.State == StepDead || s.Attempts != 0) {
			t.Fatalf("kill-switch step %+v after 8 outages", s)
		}
	}
	h.core.err = nil
	h.clk.add(2 * time.Minute)
	h.settle()
	if got := h.onlyRun(); got.State != RunSucceeded || len(h.core.calls) < 9 {
		t.Fatalf("state %s, calls %d", got.State, len(h.core.calls))
	}
	// a refusal is not an outage and still counts
	h2 := newHarness(t, nil)
	h2.dir.put(person(alice, "former", "alice@example.com"))
	h2.core.err = modclient.ErrRevokeRefused
	h2.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	for i := 0; i < 6; i++ {
		h2.settle()
		h2.clk.add(5 * time.Minute)
	}
	if got := h2.onlyRun(); got.State != RunFailed {
		t.Fatalf("a refusing can-core: %s", got.State)
	}
	_ = errors.Is
	_ = context.Background
}
