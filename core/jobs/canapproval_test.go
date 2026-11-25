package jobs

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/dodwmd/companyinacan/core/modclient"
	"github.com/dodwmd/companyinacan/core/modclient/fakecore"
)

var recruiterPerms = []string{"jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "ats.interview"}

func requisition(t *testing.T) (*env, *fakecore.Core, string, string) {
	t.Helper()
	e, core := canEnv(t)
	core.AddToken(jobsPAT, fakecore.Token{Subject: "user:alice", Permissions: recruiterPerms})
	r := e.want(e.do(jobsPAT, "POST", "/v1/jobs", jobBody("Engineer", "user:bob")), 201)
	id := r.m(t)["id"].(string)
	ar := e.want(e.do(jobsPAT, "POST", "/v1/jobs/"+id+"/approval-requests", nil), 200).m(t)
	if ar["approval"] != "pending" {
		t.Fatalf("%v", ar)
	}
	return e, core, id, "apr_1"
}

func deliver(t *testing.T, e *env, ev modclient.Event) int {
	t.Helper()
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	code, err := fakecore.Deliver(context.Background(), srv.URL+"/v1/can/events", canSecret, ev)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestRequisitionApprovalGoesThroughCanCore(t *testing.T) {
	e, core, id, apr := requisition(t)
	if len(core.Approvals) != 1 || core.Approvals[0].Type != "requisition" || core.Approvals[0].Requester != "user:alice" || core.Approvals[0].Ref != "job:"+id {
		t.Fatalf("%+v", core.Approvals)
	}
	// still pending until can-core says otherwise; nobody can open the job meanwhile when approval is required
	ev, ok := core.Decide(apr, "approved")
	if !ok {
		t.Fatal("no such approval")
	}
	if code := deliver(t, e, ev); code != 204 {
		t.Fatalf("delivery = %d", code)
	}
	if got := e.want(e.do(jobsPAT, "GET", "/v1/jobs/"+id, nil), 200).m(t)["approval"]; got != "approved" {
		t.Fatalf("approval = %v", got)
	}
	if code := deliver(t, e, ev); code != 204 { // redelivery is harmless
		t.Fatalf("redelivery = %d", code)
	}
}

func TestRejectedAndCancelledDecisionsSendTheRequesterBack(t *testing.T) {
	for _, outcome := range []string{"rejected", "cancelled", "expired"} {
		e, core, id, apr := requisition(t)
		ev, _ := core.Decide(apr, outcome)
		if code := deliver(t, e, ev); code != 204 {
			t.Fatalf("%s: delivery = %d", outcome, code)
		}
		if got := e.want(e.do(jobsPAT, "GET", "/v1/jobs/"+id, nil), 200).m(t)["approval"]; got != "rejected" {
			t.Fatalf("%s: approval = %v", outcome, got)
		}
	}
}

func TestSeparationOfDutiesHoldsEvenIfTheEngineWereWrong(t *testing.T) {
	e, core, id, apr := requisition(t)
	ev, _ := core.Decide(apr, "approved")
	var d map[string]any
	_ = json.Unmarshal(ev.Data, &d)
	d["approvers"] = []string{"user:alice"} // the requester among the approvers
	ev.Data, _ = json.Marshal(d)
	if code := deliver(t, e, ev); code != 204 {
		t.Fatalf("%d", code)
	}
	if got := e.want(e.do(jobsPAT, "GET", "/v1/jobs/"+id, nil), 200).m(t)["approval"]; got != "rejected" {
		t.Fatalf("a self-approved requisition was accepted: %v", got)
	}
}

func TestOnlySignedDeliveriesAreAccepted(t *testing.T) {
	e, core, id, apr := requisition(t)
	ev, _ := core.Decide(apr, "approved")
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	if code, _ := fakecore.Deliver(context.Background(), srv.URL+"/v1/can/events", "wrong-secret-wrong-secret", ev); code != 401 {
		t.Fatalf("wrong secret = %d", code)
	}
	if got := e.want(e.do(jobsPAT, "GET", "/v1/jobs/"+id, nil), 200).m(t)["approval"]; got != "pending" {
		t.Fatalf("a forged decision changed the requisition: %v", got)
	}
}

func TestEventsOfOtherTypesAndForeignRequestsAreIgnored(t *testing.T) {
	e, core, id, apr := requisition(t)
	ev, _ := core.Decide(apr, "approved")
	var d map[string]any
	_ = json.Unmarshal(ev.Data, &d)
	d["request_id"] = "apr_unknown"
	ev.Data, _ = json.Marshal(d)
	if code := deliver(t, e, ev); code != 204 {
		t.Fatalf("%d", code)
	}
	other := modclient.Event{ID: "evt_x", Type: "lead.created.v1", Data: json.RawMessage(`{}`)}
	if code := deliver(t, e, other); code != 204 {
		t.Fatalf("%d", code)
	}
	if got := e.want(e.do(jobsPAT, "GET", "/v1/jobs/"+id, nil), 200).m(t)["approval"]; got != "pending" {
		t.Fatalf("%v", got)
	}
}

func TestManualDecisionIsClosedWhileCanCoreDecides(t *testing.T) {
	e, _, id, _ := requisition(t)
	if r := e.do(adminTok, "POST", "/v1/jobs/"+id+"/approval", map[string]any{"decision": "approve"}); r.Code != 409 {
		t.Fatalf("an admin approved by hand behind can-core's back: %d %s", r.Code, r.Body)
	}
	if got := e.want(e.do(jobsPAT, "GET", "/v1/jobs/"+id, nil), 200).m(t)["approval"]; got != "pending" {
		t.Fatalf("%v", got)
	}
}

func TestBootstrapTokenCannotRequestACanCoreApproval(t *testing.T) {
	e, _ := canEnv(t)
	r := e.want(e.do(adminTok, "POST", "/v1/jobs", jobBody("Engineer", "user:bob")), 201)
	id := r.m(t)["id"].(string)
	if r := e.do(adminTok, "POST", "/v1/jobs/"+id+"/approval-requests", nil); r.Code != 422 {
		t.Fatalf("the bootstrap token has no identity the engine can apply separation of duties to: %d %s", r.Code, r.Body)
	}
}

func TestApprovalRequestFailsClosedWhenCanCoreIsDown(t *testing.T) {
	e, core := canEnv(t)
	core.AddToken(jobsPAT, fakecore.Token{Subject: "user:alice", Permissions: recruiterPerms})
	id := e.want(e.do(jobsPAT, "POST", "/v1/jobs", jobBody("Engineer", "user:bob")), 201).m(t)["id"].(string)
	core.SetDown(true)
	core.AddToken(jobsPAT+"2", fakecore.Token{Subject: "user:alice", Permissions: recruiterPerms}) // not reachable while down
	r := e.do(jobsPAT, "POST", "/v1/jobs/"+id+"/approval-requests", nil)
	if r.Code == 200 {
		t.Fatalf("approved or accepted while can-core was down: %s", r.Body)
	}
	core.SetDown(false)
	got := e.want(e.do(jobsPAT, "GET", "/v1/jobs/"+id, nil), 200).m(t)["approval"]
	if got != "none" {
		t.Fatalf("approval state %v after a failed request", got)
	}
}
