package reconcile

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// The upgrade that brings reminders (specs/060-reminders): what it widens is held for approval and
// applies once approved; a first install and a restore do not ask.
func TestTheRemindersUpgradeIsHeldAndApprovable(t *testing.T) {
	ctx := context.Background()
	ids := []string{"entity", "finance", "jobs", "people", "work"}
	added := []string{"invoice.overdue.v1", "contract.declined.v1", "expense.submitted.v1", "expense.decided.v1", "timesheet.submitted.v1",
		"timesheet.decided.v1", "work.issue.p0.v1", "timeoff.requested.v1", "timeoff.decided.v1"}
	shipped := loadModules(t, "../../../modules", ids...)

	// The release before: the same manifests without the rules and the new events.
	before := loadModules(t, "../../../modules", ids...)
	for _, m := range before.Manifests {
		m.Version = "0.0.1"
		m.Notifications = nil
		kept := m.Publishes[:0:0]
		for _, p := range m.Publishes {
			if !slices.Contains(added, p.Event) {
				kept = append(kept, p)
			}
		}
		m.Publishes = kept
	}

	g := newRig(t)
	g.r.RequireApproval = true
	if res := apply(t, g, shipped); len(res.Held) != 0 || len(res.Actions) != len(ids) {
		t.Fatalf("a first install asks nothing: %+v", res)
	}

	g = newRig(t)
	g.r.RequireApproval = true
	apply(t, g, before)
	res := apply(t, g, shipped)
	if len(res.Held) != len(ids) || len(res.Actions) != 0 {
		t.Fatalf("every module that widens waits: %+v", res)
	}
	for _, h := range res.Held {
		t.Logf("%s: %s", h.Module, strings.Join(h.Widens, "; "))
		for _, w := range h.Widens {
			if !strings.HasPrefix(w, "may publish event ") && !strings.HasPrefix(w, "notifies ") {
				t.Errorf("%s widens in a way the upgrade notes do not say: %q", h.Module, w)
			}
		}
		rec, err := g.db.GetModule(ctx, h.Module)
		if err != nil || len(rec.Manifest.Notifications) != 0 || rec.Version != "0.0.1" {
			t.Fatalf("%s: the installed manifest changed while waiting: %v", h.Module, err)
		}
		if err := g.db.ApproveUpgrade(ctx, h.Module, h.Approval); err != nil {
			t.Fatalf("approving %s: %v", h.Module, err)
		}
	}
	if res = apply(t, g, shipped); len(res.Held) != 0 || len(res.Actions) != len(ids) {
		t.Fatalf("approved: %+v", res)
	}
	var rules []config.NotifyRule
	for _, id := range ids {
		rec, err := g.db.GetModule(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		rules = append(rules, rec.Manifest.Notifications...)
	}
	if len(rules) != 13 {
		t.Fatalf("%d rules recorded", len(rules))
	}

	// A restore approves everything ("*").
	g = newRig(t)
	g.r.RequireApproval = true
	apply(t, g, before)
	for _, id := range ids {
		if err := g.db.ApproveUpgrade(ctx, id, "*"); err != nil {
			t.Fatal(err)
		}
	}
	if res = apply(t, g, shipped); len(res.Held) != 0 || len(res.Actions) != len(ids) {
		t.Fatalf("a restore: %+v", res)
	}
}
