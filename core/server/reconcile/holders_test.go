package reconcile

import (
	"context"
	"strings"
	"testing"
)

// A held upgrade names who holds the roles that gain, without making them part of what is approved
// (specs/068-equity-board-role).
func TestAHeldUpgradeNamesWhoHoldsTheRoleThatGains(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	g.r.RequireApproval = true
	apply(t, g, desired(nil, nil, mf("work", "1", nil)))
	next := mf("work", "2", nil)
	next.Permissions = []string{"work.export"}
	next.Roles = map[string][]string{"board": {"work.export"}}
	d := desired(nil, nil, next)

	first := apply(t, g, d).Held[0]
	if len(first.Notes) != 1 || !strings.Contains(first.Notes[0], "work:board is assigned to nobody now") {
		t.Fatalf("notes = %q", first.Notes)
	}
	for _, who := range []string{"user:bea", "user:abe"} {
		if _, err := g.db.ExecContext(ctx, `INSERT INTO rbac_assignments(subject, role, scope, granted_by, granted_at) VALUES (?, 'work:board', '', 'user:root', 1)`, who); err != nil {
			t.Fatal(err)
		}
	}
	held := apply(t, g, d).Held[0]
	if len(held.Notes) != 1 || !strings.Contains(held.Notes[0], "work:board is assigned now to user:abe, user:bea:") {
		t.Fatalf("notes = %q", held.Notes)
	}
	if held.Approval != first.Approval {
		t.Fatal("who holds a role changed the approval code")
	}
	rec, _ := g.db.GetModule(ctx, "work")
	if p, ok := ParsePending(rec.Pending); !ok || len(p.Notes) != 1 || p.Notes[0] != held.Notes[0] {
		t.Fatalf("not recorded for status and the admin UI: %q", rec.Pending)
	}
}
