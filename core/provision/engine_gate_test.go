package provision

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A privileged leaver loses access at once: removal is never gated, only deletion waits for a
// second person, and a pending, rejected or unanswered approval never keeps access enabled.
func TestPrivilegedLeaverAccessRemovalIsNeverGated(t *testing.T) {
	h := newHarness(t, func(p *Policy) {
		p.DeleteAfterDays = 30
		p.Conn["app-a"] = ConnConfig{Name: "app-a", Privileged: true}
	})
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.linkAccounts(alice, "app-a", "app-b")
	h.addGroup("admins", alice)
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	h.settle()
	if h.a.count("disable_account") != 1 || h.b.count("disable_account") != 1 || len(h.core.calls) != 1 {
		t.Fatalf("disables %d/%d, revocations %d", h.a.count("disable_account"), h.b.count("disable_account"), len(h.core.calls))
	}
	if len(h.appr.created) != 0 {
		t.Fatalf("approval asked for before the deletion date: %d", len(h.appr.created))
	}
	if r := h.onlyRun(); r.State != RunScheduled {
		t.Fatalf("state %s, want scheduled for the deletion", r.State)
	}
	// the deletion date arrives: the second person is asked, and while nobody answers nothing is deleted
	h.clk.add(31 * 24 * time.Hour)
	h.settle()
	if len(h.appr.created) != 1 || h.appr.created[0].Type != "privileged-leaver" || strings.Contains(h.appr.created[0].Title, "alice@") {
		t.Fatalf("approvals %+v", h.appr.created)
	}
	if h.a.count("delete_account") != 0 || h.b.count("delete_account") != 0 {
		t.Fatal("deleted without a second person")
	}
	var aprID string
	for _, s := range h.onlyRun().Steps {
		if s.State == StepAwaiting {
			aprID = s.ApprovalID
		}
	}
	// the leaver's own account cannot approve the deletion
	if err := h.event(EventApprovalDecided, map[string]any{"request_id": aprID, "type": "privileged-leaver", "outcome": "approved", "approvers": []string{"user:sso-aaaa"}}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if h.a.count("delete_account") != 0 {
		t.Fatal("the leaver approved their own deletion")
	}
	// a rejection (or an expiry) never re-enables or keeps access: the accounts stay disabled
	if accts, _ := h.st.activeAccounts(context.Background(), alice); len(accts) != 0 {
		t.Fatalf("accounts active after the rejection: %v", accts)
	}
	if err := h.st.VerifyAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPrivilegedLeaverDeletionProceedsOnSecondPersonApproval(t *testing.T) {
	h := newHarness(t, func(p *Policy) { p.DeleteAfterDays = 1 })
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.linkAccounts(alice, "app-a")
	h.accountGone(alice, "app-b")
	h.addGroup("admins", alice)
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	h.settle()
	h.clk.add(48 * time.Hour)
	h.settle()
	var aprID string
	for _, s := range h.onlyRun().Steps {
		if s.State == StepAwaiting {
			aprID = s.ApprovalID
		}
	}
	// a decision for a request that is not ours changes nothing
	h.mustEvent(EventApprovalDecided, map[string]any{"request_id": "apr_unknown", "type": "privileged-leaver", "outcome": "approved", "approvers": []string{"user:boss"}})
	if h.a.count("delete_account") != 0 {
		t.Fatal("unknown approval released a step")
	}
	h.mustEvent(EventApprovalDecided, map[string]any{"request_id": aprID, "type": "privileged-leaver", "outcome": "approved", "approvers": []string{"user:boss"}})
	h.settle()
	if h.a.count("delete_account") != 1 || h.onlyRun().State != RunSucceeded {
		t.Fatalf("deletes %d state %s", h.a.count("delete_account"), h.onlyRun().State)
	}
}

func TestNoApprovalsEngineStillDisablesButNeverDeletes(t *testing.T) {
	h := newHarness(t, func(p *Policy) { p.DeleteAfterDays = 1 })
	h.eng.Approvals = nil
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.linkAccounts(alice, "app-a")
	h.addGroup("admins", alice)
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	h.settle()
	h.clk.add(48 * time.Hour)
	h.settle()
	if h.a.count("disable_account") != 1 {
		t.Fatal("access removal waited for an approvals engine")
	}
	if h.a.count("delete_account") != 0 {
		t.Fatal("a sensitive deletion ran without an approvals engine")
	}
}
