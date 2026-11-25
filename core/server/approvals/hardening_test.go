package approvals

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/rbac"
)

func TestDecideRefusesAnExpiredRequestWithoutTheTicker(t *testing.T) {
	x := newEnv(t)
	p := chain
	p.ExpiresAfter = "1h"
	x.policy(p)
	r := x.create("k", 1)
	x.now = x.now.Add(2 * time.Hour) // no Tick has run
	if err := x.decide(r.ID, "user:boss", "approve"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approving after expiry: %v, want ErrNotPending", err)
	}
	if x.status(r.ID) != "pending" {
		t.Fatal("the ticker, not Decide, moves the status")
	}
	if n, _ := x.e.Tick(context.Background()); n != 1 || x.status(r.ID) != "expired" {
		t.Fatalf("tick = %d, status %s", n, x.status(r.ID))
	}
}

type failExpire struct {
	Appender
	target string
}

func (f failExpire) AppendTx(ctx context.Context, tx *sql.Tx, e audit.Entry) (audit.Record, error) {
	if e.Action == "approval.expire" && e.Target == f.target {
		return audit.Record{}, errors.New("audit unavailable")
	}
	return f.Appender.AppendTx(ctx, tx, e)
}

func TestTickContinuesPastAFailingRow(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	p := chain
	p.ExpiresAfter = "1h"
	x.policy(p)
	bad := x.create("a", 1)
	x.now = x.now.Add(time.Minute)
	good := x.create("b", 1)
	x.e.audit = failExpire{Appender: x.e.audit, target: bad.ID}
	x.now = x.now.Add(3 * time.Hour)
	n, err := x.e.Tick(ctx)
	if err == nil || n != 1 {
		t.Fatalf("tick = %d, %v; want the good row handled and the failure reported", n, err)
	}
	if x.status(good.ID) != "expired" || x.status(bad.ID) != "pending" {
		t.Fatalf("good=%s bad=%s", x.status(good.ID), x.status(bad.ID))
	}
	// the failing row is not retried every tick
	if n, err := x.e.Tick(ctx); n != 0 || err != nil {
		t.Fatalf("second tick = %d, %v; the failed row must back off", n, err)
	}
	x.e.audit = failExpire{Appender: x.e.audit.(failExpire).Appender, target: "none"}
	x.now = x.now.Add(failBackoff + time.Second)
	if n, _ := x.e.Tick(ctx); n != 1 || x.status(bad.ID) != "expired" {
		t.Fatalf("after the backoff the row is retried: %d %s", n, x.status(bad.ID))
	}
}

func TestScopedRoleAssignmentDoesNotMakeAnApprover(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	if err := x.rb.Grant(ctx, rbac.CLIActor, "user:scoped", "finance", "dept/a/*"); err != nil {
		t.Fatal(err)
	}
	x.policy(config.ApprovalPolicy{Steps: []config.ApprovalStep{{Name: "fin", Approvers: []config.Approver{{Role: "finance"}}}}})
	r := x.create("k", 1)
	if err := x.decide(r.ID, "user:scoped", "approve"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("a scoped finance assignment approved: %v", err)
	}
	if w, _ := x.e.Waiting(ctx, "user:scoped", 10); len(w) != 0 {
		t.Fatal("a scoped assignment must not fill the waiting inbox")
	}
	if w, _ := x.e.Waiting(ctx, "user:fin1", 10); len(w) != 1 {
		t.Fatal("unscoped holders still do")
	}
	if x.notes.to("user:scoped", "approval.requested") != 0 {
		t.Fatal("a scoped holder must not be notified as an approver")
	}
}

type errLines struct{}

func (errLines) ManagerOf(context.Context, string) (string, error) {
	return "", errors.New("directory down")
}

func TestResolverFailureWhileOpeningALaterStepIsAnError(t *testing.T) {
	x := newEnv(t)
	x.policy(config.ApprovalPolicy{Steps: []config.ApprovalStep{
		{Name: "fin", Approvers: []config.Approver{{Role: "finance"}}},
		{Name: "mgr", Approvers: []config.Approver{{ManagerOfRequester: true}}, Fallback: []config.Approver{{Role: "director"}}},
	}})
	r := x.create("k", 1)
	x.e.d.Lines = errLines{}
	err := x.decide(r.ID, "user:fin1", "approve")
	if !errors.Is(err, ErrResolver) {
		t.Fatalf("err = %v, want ErrResolver (not a silent fallback)", err)
	}
	if x.status(r.ID) != "pending" {
		t.Fatal("the vote must not be recorded when the next step cannot be opened")
	}
	x.e.d.Lines = x.lines
	if err := x.decide(r.ID, "user:fin1", "approve"); err != nil {
		t.Fatalf("retry after the directory recovers: %v", err)
	}
}

func TestParallelStepsOneVotePerPerson(t *testing.T) {
	x := newEnv(t)
	x.policy(config.ApprovalPolicy{Steps: []config.ApprovalStep{
		{Name: "a", Approvers: []config.Approver{{Role: "finance"}}},
		{Name: "b", Parallel: true, Approvers: []config.Approver{{Role: "finance"}}},
	}})
	r := x.create("k", 1)
	if err := x.decide(r.ID, "user:fin1", "approve"); err != nil {
		t.Fatal(err)
	}
	if err := x.decide(r.ID, "user:fin1", "approve"); !errors.Is(err, ErrAlreadyVoted) {
		t.Fatalf("a person eligible for both parallel steps votes once: %v", err)
	}
	if err := x.decide(r.ID, "user:fin2", "approve"); err != nil || x.status(r.ID) != "approved" {
		t.Fatalf("a second person completes the other step: %v %s", err, x.status(r.ID))
	}
}
