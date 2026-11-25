package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

const (
	finCanSecret = "can-hook-secret-0123456789abcdef"
	writerPAT    = finPAT + "W"
	approverPAT  = finPAT + "A"
	adminPAT     = finPAT + "D"
)

func approvalEnv(t *testing.T) (*env, *fakecore.Core) {
	t.Helper()
	core := fakecore.New(t, "finance")
	e := newEnv(t, func(c *Config, o *Options) {
		c.Webhook.URL = ""
		o.Can = core.Client(t)
		o.CanSecret = finCanSecret
	})
	e.mu.Lock()
	e.now = time.Now() // the webhook replay window uses the app clock
	e.mu.Unlock()
	core.AddToken(writerPAT, fakecore.Token{Subject: "user:alice", Permissions: []string{PermWrite}})
	core.AddToken(approverPAT, fakecore.Token{Subject: "user:bob", Permissions: []string{PermApprove}})
	core.AddToken(adminPAT, fakecore.Token{Subject: "user:alice", Permissions: []string{PermAdmin}})
	return e, core
}

func draft(t *testing.T, e *env) *Invoice {
	t.Helper()
	pid := e.party(adminTok, "Acme Pty", "customer")
	return e.invoice(adminTok, pid, nil, line("Consulting", 1000, 150000))
}

func (e *env) deliver(t *testing.T, ev modclient.Event) int {
	t.Helper()
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	code, err := fakecore.Deliver(context.Background(), srv.URL+"/v1/can/events", finCanSecret, ev)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

type approvalOut struct {
	ApprovalID string `json:"approval_id"`
	Status     string `json:"status"`
}

func requestApproval(t *testing.T, e *env, tok string, id int64) approvalOut {
	t.Helper()
	var out approvalOut
	e.must(200, tok, "POST", fmt.Sprintf("/v1/invoices/%d/request-approval", id), nil, &out)
	return out
}

func TestInvoiceApprovalNeedsAnApprovedCanCoreRequest(t *testing.T) {
	e, core := approvalEnv(t)
	inv := draft(t, e)
	// before any request: a finance.approve holder alone is not enough
	if code := e.do(approverPAT, "POST", fmt.Sprintf("/v1/invoices/%d/approve", inv.ID), nil, nil); code != 403 {
		t.Fatalf("approve without a can-core approval = %d", code)
	}
	out := requestApproval(t, e, writerPAT, inv.ID)
	if out.Status != "pending" || out.ApprovalID == "" {
		t.Fatalf("%+v", out)
	}
	a := core.Approvals[0]
	if a.Type != "invoice" || a.Requester != "user:alice" || a.Ref != fmt.Sprintf("invoice:%d", inv.ID) {
		t.Fatalf("%+v", a)
	}
	if code := e.do(approverPAT, "POST", fmt.Sprintf("/v1/invoices/%d/approve", inv.ID), nil, nil); code != 403 {
		t.Fatalf("approve while can-core still pending = %d", code)
	}
	ev, _ := core.Decide(out.ApprovalID, "approved")
	if code := e.deliver(t, ev); code != 204 {
		t.Fatalf("delivery = %d", code)
	}
	var got Invoice
	e.must(200, approverPAT, "POST", fmt.Sprintf("/v1/invoices/%d/approve", inv.ID), nil, &got)
	if got.Status != StApproved || got.ApprovedBy == nil || *got.ApprovedBy != "user:bob" {
		t.Fatalf("%+v", got)
	}
}

func TestRequesterCannotApproveEvenWithFinanceAdmin(t *testing.T) {
	e, core := approvalEnv(t)
	inv := draft(t, e)
	out := requestApproval(t, e, writerPAT, inv.ID) // alice requests
	ev, _ := core.Decide(out.ApprovalID, "approved")
	_ = e.deliver(t, ev)
	// alice also holds finance.admin through another token: still the requester
	if code := e.do(adminPAT, "POST", fmt.Sprintf("/v1/invoices/%d/approve", inv.ID), nil, nil); code != 403 {
		t.Fatalf("the requester approved her own invoice: %d", code)
	}
}

func TestRejectedAndEditedInvoicesAreNotApproved(t *testing.T) {
	e, core := approvalEnv(t)
	inv := draft(t, e)
	out := requestApproval(t, e, writerPAT, inv.ID)
	ev, _ := core.Decide(out.ApprovalID, "rejected")
	_ = e.deliver(t, ev)
	if code := e.do(approverPAT, "POST", fmt.Sprintf("/v1/invoices/%d/approve", inv.ID), nil, nil); code != 403 {
		t.Fatalf("a rejected request approved the invoice: %d", code)
	}

	// a new request, approved, then the invoice is edited: the approval was of other content
	inv2 := draft(t, e)
	out2 := requestApproval(t, e, writerPAT, inv2.ID)
	ev2, _ := core.Decide(out2.ApprovalID, "approved")
	_ = e.deliver(t, ev2)
	e.must(200, adminTok, "PATCH", fmt.Sprintf("/v1/invoices/%d", inv2.ID), obj{"notes": "changed after approval"}, nil)
	if code := e.do(approverPAT, "POST", fmt.Sprintf("/v1/invoices/%d/approve", inv2.ID), nil, nil); code != 403 {
		t.Fatalf("an edited invoice kept its approval: %d", code)
	}
}

func TestRequestingAgainIsIdempotentWhilePending(t *testing.T) {
	e, core := approvalEnv(t)
	inv := draft(t, e)
	a := requestApproval(t, e, writerPAT, inv.ID)
	b := requestApproval(t, e, writerPAT, inv.ID)
	if a.ApprovalID != b.ApprovalID || len(core.Approvals) != 1 {
		t.Fatalf("%+v %+v %d", a, b, len(core.Approvals))
	}
}

func TestOnlyAPersonCanRequestAndOnlyForADraft(t *testing.T) {
	e, core := approvalEnv(t)
	inv := draft(t, e)
	if code := e.do(adminTok, "POST", fmt.Sprintf("/v1/invoices/%d/request-approval", inv.ID), nil, nil); code != 422 {
		t.Fatalf("the bootstrap token has no identity for separation of duties: %d", code)
	}
	if code := e.do(approverPAT, "POST", fmt.Sprintf("/v1/invoices/%d/request-approval", inv.ID), nil, nil); code != 403 {
		t.Fatalf("a token without finance.write requested an approval: %d", code)
	}
	out := requestApproval(t, e, writerPAT, inv.ID)
	ev, _ := core.Decide(out.ApprovalID, "approved")
	_ = e.deliver(t, ev)
	e.must(200, approverPAT, "POST", fmt.Sprintf("/v1/invoices/%d/approve", inv.ID), nil, nil)
	if code := e.do(writerPAT, "POST", fmt.Sprintf("/v1/invoices/%d/request-approval", inv.ID), nil, nil); code != 409 {
		t.Fatalf("an approved invoice is not a draft: %d", code)
	}
}

func TestRequestFailsClosedWhenCanCoreIsDown(t *testing.T) {
	e, core := approvalEnv(t)
	inv := draft(t, e)
	core.SetDown(true)
	e.mu.Lock()
	e.mu.Unlock()
	// PAT introspection is cached for a short time only if it was used; use the bootstrap path
	// for the draft and a fresh personal token (a cache miss) for the request.
	core.SetDown(false)
	core.AddToken(writerPAT+"2", fakecore.Token{Subject: "user:carol", Permissions: []string{PermWrite}})
	if code := e.do(writerPAT+"2", "GET", "/v1/invoices", nil, nil); code != 200 { // warms the identity cache
		t.Fatalf("%d", code)
	}
	core.SetDown(true)
	code := e.do(writerPAT+"2", "POST", fmt.Sprintf("/v1/invoices/%d/request-approval", inv.ID), nil, nil)
	if code != 503 && code != 502 {
		t.Fatalf("with can-core down the request must fail visibly, got %d", code)
	}
	if code := e.do(approverPAT, "POST", fmt.Sprintf("/v1/invoices/%d/approve", inv.ID), nil, nil); code == 200 {
		t.Fatal("approved while can-core was down")
	}
}

func TestForgedAndForeignDecisionsAreIgnored(t *testing.T) {
	e, core := approvalEnv(t)
	inv := draft(t, e)
	out := requestApproval(t, e, writerPAT, inv.ID)
	ev, _ := core.Decide(out.ApprovalID, "approved")
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	if code, _ := fakecore.Deliver(context.Background(), srv.URL+"/v1/can/events", "wrong-secret-wrong-secret", ev); code != 401 {
		t.Fatalf("forged = %d", code)
	}
	var d map[string]any
	_ = json.Unmarshal(ev.Data, &d)
	d["request_id"] = "apr_unknown"
	ev.Data, _ = json.Marshal(d)
	ev.ID = "evt_other"
	if code := e.deliver(t, ev); code != 204 {
		t.Fatalf("unknown request = %d", code)
	}
	if code := e.do(approverPAT, "POST", fmt.Sprintf("/v1/invoices/%d/approve", inv.ID), nil, nil); code != 403 {
		t.Fatalf("a decision for an unknown request approved the invoice: %d", code)
	}
}

func TestApprovalsOfOtherKindsStillUseThePermissionRule(t *testing.T) {
	// Expenses, timesheets and purchase orders are not wired to can-core yet: with a can-core
	// client installed they keep their rule (finance.approve plus separation of duties).
	e, _ := approvalEnv(t)
	c := &CoreApprover{store: e.app.store}
	ok, err := c.CanApprove(context.Background(), &Principal{Subject: "user:bob", Permissions: []string{PermApprove}}, ApprovalRequest{Kind: "expense"})
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	ok, _ = c.CanApprove(context.Background(), &Principal{Subject: "user:bob", Permissions: []string{PermWrite}}, ApprovalRequest{Kind: "expense"})
	if ok {
		t.Fatal("no finance.approve")
	}
}
