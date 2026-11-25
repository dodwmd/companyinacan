package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/events"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

type fakeLines map[string]string

func (f fakeLines) ManagerOf(_ context.Context, subject string) (string, error) {
	if m, ok := f[subject]; ok {
		return m, nil
	}
	return "", ErrNoManager
}

type recNotifier struct{ notices []notify.Notice }

func (r *recNotifier) Notify(_ context.Context, n notify.Notice) (int64, bool, error) {
	r.notices = append(r.notices, n)
	return int64(len(r.notices)), true, nil
}

func (r *recNotifier) to(subject, typ string) int {
	n := 0
	for _, x := range r.notices {
		if x.Subject == subject && x.Type == typ {
			n++
		}
	}
	return n
}

type env struct {
	t     *testing.T
	db    *store.DB
	e     *Engine
	rb    *rbac.Service
	lines fakeLines
	notes *recNotifier
	now   time.Time
}

const exp = "expenses"

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	x := &env{t: t, db: db, lines: fakeLines{}, notes: &recNotifier{}, now: time.Unix(1_700_000_000, 0)}
	db.Now = func() time.Time { return x.now }
	al := audit.New(db)
	x.rb = rbac.New(db, al, rbac.Options{Now: func() time.Time { return x.now }})
	bus := events.NewBus(db)
	x.e = New(db, al, Deps{Roles: x.rb, Perms: x.rb, Lines: x.lines, Notifier: x.notes, Bus: bus, Now: func() time.Time { return x.now }})
	mk := func(id string, roles map[string][]string, subs ...store.Subscription) {
		m := store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, HMACSecret: "s",
			Manifest: config.Module{ID: id, Roles: roles, Approvals: []config.ManifestApproval{{Type: "expense"}}}}
		if err := db.UpsertModule(ctx, m, nil, subs); err != nil {
			t.Fatal(err)
		}
	}
	mk(exp, map[string][]string{"finance": {"expenses.approve"}, "legal": {"legal.review"}, "director": {"expenses.director"}},
		store.Subscription{ModuleID: exp, Event: "approval.decided.v1", Service: exp, Port: 80, Webhook: "/hook"})
	mk("vendors", nil, store.Subscription{ModuleID: "vendors", Event: "approval.decided.v1", Service: "vendors", Port: 80, Webhook: "/hook"})
	for _, g := range []struct{ s, r string }{{"user:fin1", "finance"}, {"user:fin2", "finance"}, {"user:fin3", "finance"}, {"user:lawyer", "legal"}, {"user:dir", "director"}} {
		if err := x.rb.Grant(ctx, "test", g.s, g.r, ""); err != nil {
			t.Fatal(err)
		}
	}
	x.lines["user:emp"] = "user:boss"
	return x
}

func i64(v int64) *int64 { return &v }

func (x *env) policy(p config.ApprovalPolicy) {
	x.t.Helper()
	if err := x.e.SyncPolicies(context.Background(), map[string]config.ApprovalPolicy{"expense": p}); err != nil {
		x.t.Fatal(err)
	}
}

var chain = config.ApprovalPolicy{Steps: []config.ApprovalStep{
	{Name: "manager", Approvers: []config.Approver{{ManagerOfRequester: true}}},
	{Name: "finance", Approvers: []config.Approver{{Role: "finance"}}, When: &config.StepCondition{AmountGTE: i64(100_000)}},
}}

func (x *env) create(key string, amount int64) Request {
	x.t.Helper()
	r, _, err := x.e.Create(context.Background(), CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:emp", Ref: "exp-1", Title: "Laptop", Amount: amount, IdempotencyKey: key})
	if err != nil {
		x.t.Fatal(err)
	}
	return r
}

func (x *env) decide(id, actor, d string) error {
	_, err := x.e.Decide(context.Background(), id, actor, d, "")
	return err
}

func (x *env) status(id string) string {
	x.t.Helper()
	d, err := x.e.Get(context.Background(), id)
	if err != nil {
		x.t.Fatal(err)
	}
	return d.Request.Status
}

func (x *env) decidedEvents() []string {
	rows, _ := x.db.QueryContext(context.Background(), `SELECT data_json FROM events WHERE type='approval.decided.v1' ORDER BY time, id`)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestSequentialChainManagerThenFinance(t *testing.T) {
	x := newEnv(t)
	x.policy(chain)
	r := x.create("k1", 250_000)
	if err := x.decide(r.ID, "user:fin1", "approve"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("finance acting on the manager step: err = %v, want ErrNotEligible", err)
	}
	if x.notes.to("user:boss", "approval.requested") != 1 || x.notes.to("user:fin1", "approval.requested") != 0 {
		t.Fatalf("only the first step's approver is told at first: %+v", x.notes.notices)
	}
	if err := x.decide(r.ID, "user:boss", "approve"); err != nil {
		t.Fatal(err)
	}
	if x.status(r.ID) != "pending" {
		t.Fatal("finance step still outstanding")
	}
	if x.notes.to("user:fin1", "approval.requested") != 1 || x.notes.to("user:fin2", "approval.requested") != 1 {
		t.Fatal("finance approvers must be told when their step opens")
	}
	if err := x.decide(r.ID, "user:fin1", "approve"); err != nil {
		t.Fatal(err)
	}
	if x.status(r.ID) != "approved" {
		t.Fatalf("status = %s", x.status(r.ID))
	}
	ev := x.decidedEvents()
	if len(ev) != 1 || !strings.Contains(ev[0], `"outcome":"approved"`) || !strings.Contains(ev[0], r.ID) {
		t.Fatalf("decided events = %v", ev)
	}
	if x.notes.to("user:emp", "approval.decided") != 1 {
		t.Fatal("the requester must be told the outcome")
	}
}

func TestDecidedWebhookGoesOnlyToTheOwningModule(t *testing.T) {
	x := newEnv(t)
	x.policy(config.ApprovalPolicy{Steps: []config.ApprovalStep{{Name: "fin", Approvers: []config.Approver{{Role: "finance"}}}}})
	r := x.create("k", 10)
	if err := x.decide(r.ID, "user:fin1", "approve"); err != nil {
		t.Fatal(err)
	}
	ds, _ := x.db.ListDeliveries(context.Background(), 10)
	if len(ds) != 1 || ds[0].ModuleID != exp {
		t.Fatalf("deliveries = %+v; only %s may receive its own decisions", ds, exp)
	}
}

func TestThresholdSkipsFinanceForSmallAmounts(t *testing.T) {
	x := newEnv(t)
	x.policy(chain)
	r := x.create("k", 5_000)
	if err := x.decide(r.ID, "user:boss", "approve"); err != nil {
		t.Fatal(err)
	}
	if x.status(r.ID) != "approved" {
		t.Fatal("below the threshold the manager alone approves")
	}
}

func TestParallelStepsBothRequired(t *testing.T) {
	x := newEnv(t)
	x.policy(config.ApprovalPolicy{Steps: []config.ApprovalStep{
		{Name: "finance", Approvers: []config.Approver{{Role: "finance"}}},
		{Name: "legal", Parallel: true, Approvers: []config.Approver{{Role: "legal"}}},
		{Name: "director", Approvers: []config.Approver{{Role: "director"}}},
	}})
	r := x.create("k", 1)
	if err := x.decide(r.ID, "user:dir", "approve"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("director before the parallel stage completes: %v", err)
	}
	_ = x.decide(r.ID, "user:lawyer", "approve")
	if x.notes.to("user:dir", "approval.requested") != 0 {
		t.Fatal("director must wait for both parallel steps")
	}
	_ = x.decide(r.ID, "user:fin1", "approve")
	if x.notes.to("user:dir", "approval.requested") != 1 {
		t.Fatal("director is told once both parallel steps are approved")
	}
	if err := x.decide(r.ID, "user:dir", "approve"); err != nil {
		t.Fatal(err)
	}
	if x.status(r.ID) != "approved" {
		t.Fatal("not approved")
	}
}

func TestQuorumNeedsDistinctApprovers(t *testing.T) {
	x := newEnv(t)
	x.policy(config.ApprovalPolicy{Steps: []config.ApprovalStep{{Name: "fin", Quorum: 2, Approvers: []config.Approver{{Role: "finance"}}}}})
	r := x.create("k", 1)
	_ = x.decide(r.ID, "user:fin1", "approve")
	if err := x.decide(r.ID, "user:fin1", "approve"); !errors.Is(err, ErrAlreadyVoted) {
		t.Fatalf("the same approver twice: %v, want ErrAlreadyVoted", err)
	}
	if x.status(r.ID) != "pending" {
		t.Fatal("one approver cannot satisfy a quorum of two")
	}
	_ = x.decide(r.ID, "user:fin2", "approve")
	if x.status(r.ID) != "approved" {
		t.Fatal("two distinct approvers satisfy it")
	}
}

func TestSeparationOfDuties(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(config.ApprovalPolicy{Steps: []config.ApprovalStep{
		{Name: "a", Approvers: []config.Approver{{Role: "finance"}}},
		{Name: "b", Approvers: []config.Approver{{Role: "finance"}}},
	}})
	// A requester who is a finance approver can never approve their own request.
	r, _, err := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:fin1", IdempotencyKey: "self"})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.decide(r.ID, "user:fin1", "approve"); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self approval: %v, want ErrSelfApproval", err)
	}
	if err := x.decide(r.ID, "user:fin1", "reject"); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self rejection is also refused: %v", err)
	}
	// The same person cannot approve two steps of one request.
	if err := x.decide(r.ID, "user:fin2", "approve"); err != nil {
		t.Fatal(err)
	}
	if err := x.decide(r.ID, "user:fin2", "approve"); !errors.Is(err, ErrAlreadyVoted) && !errors.Is(err, ErrNotEligible) {
		t.Fatalf("approving step b as the step a approver: %v", err)
	}
	if err := x.decide(r.ID, "user:fin3", "approve"); err != nil {
		t.Fatal(err)
	}
	if x.status(r.ID) != "approved" {
		t.Fatal("two different people approved")
	}
	// Delegation cannot be used to approve for the requester, or as the requester.
	if _, err := x.e.Delegate(ctx, "user:fin1", "user:fin2", x.now, x.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r2, _, _ := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:fin1", IdempotencyKey: "self2"})
	if err := x.decide(r2.ID, "user:lawyer", "approve"); !errors.Is(err, ErrNotEligible) {
		t.Fatal("setup: lawyer is not a finance approver")
	}
	_, _ = x.e.Delegate(ctx, "user:fin2", "user:fin1", x.now, x.now.Add(time.Hour))
	if err := x.decide(r2.ID, "user:fin1", "approve"); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("the requester acting through a delegation: %v", err)
	}
}

func TestRejectEndsTheRequest(t *testing.T) {
	x := newEnv(t)
	x.policy(chain)
	r := x.create("k", 250_000)
	if _, err := x.e.Decide(context.Background(), r.ID, "user:boss", "reject", "too expensive"); err != nil {
		t.Fatal(err)
	}
	if x.status(r.ID) != "rejected" {
		t.Fatal("not rejected")
	}
	if err := x.decide(r.ID, "user:boss", "approve"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("deciding a decided request: %v", err)
	}
	d, _ := x.e.Get(context.Background(), r.ID)
	found := false
	for _, dec := range d.Decisions {
		found = found || (dec.Kind == "reject" && dec.Comment == "too expensive" && dec.Actor == "user:boss")
	}
	if !found {
		t.Fatalf("decision history lacks the rejection with its comment: %+v", d.Decisions)
	}
}

func TestCancelAndComments(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	r := x.create("k", 250_000)
	if err := x.e.Comment(ctx, r.ID, "user:stranger", "hi"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("stranger comment: %v", err)
	}
	if err := x.e.Comment(ctx, r.ID, "user:boss", "which laptop?"); err != nil {
		t.Fatal(err)
	}
	if err := x.e.Comment(ctx, r.ID, "user:emp", strings.Repeat("x", MaxCommentLen+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized comment: %v", err)
	}
	if err := x.e.Cancel(ctx, r.ID, "user:boss", "no"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("an approver may not withdraw someone else's request: %v", err)
	}
	if err := x.e.Cancel(ctx, r.ID, "user:emp", "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if x.status(r.ID) != "cancelled" {
		t.Fatal("not cancelled")
	}
	ev := x.decidedEvents()
	if len(ev) != 1 || !strings.Contains(ev[0], `"outcome":"cancelled"`) {
		t.Fatalf("events = %v", ev)
	}
	if err := x.e.Cancel(ctx, r.ID, "user:emp", ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second cancel: %v", err)
	}
}

func TestAdminCanCancelAnyRequest(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	r := x.create("k", 1)
	if err := x.rb.Grant(ctx, "t", "user:ops", "operator", ""); err != nil {
		t.Fatal(err)
	}
	if err := x.e.Cancel(ctx, r.ID, "user:ops", "policy change"); err != nil {
		t.Fatal(err)
	}
}

func TestDelegationOutOfOffice(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	r := x.create("k", 1)
	if err := x.decide(r.ID, "user:deputy", "approve"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("no delegation yet: %v", err)
	}
	id, err := x.e.Delegate(ctx, "user:boss", "user:deputy", x.now, x.now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.e.Delegate(ctx, "user:boss", "user:boss", x.now, x.now.Add(time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self delegation: %v", err)
	}
	if _, err := x.e.Delegate(ctx, "user:boss", "user:deputy", x.now, x.now.Add(200*24*time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over-long delegation: %v", err)
	}
	w, _ := x.e.Waiting(ctx, "user:deputy", 10)
	if len(w) != 1 {
		t.Fatalf("the delegate sees the delegator's inbox: %d", len(w))
	}
	if err := x.e.RevokeDelegation(ctx, "user:boss", id); err != nil {
		t.Fatal(err)
	}
	if err := x.decide(r.ID, "user:deputy", "approve"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("revoked delegation still works: %v", err)
	}
	if _, err := x.e.Delegate(ctx, "user:boss", "user:deputy", x.now, x.now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := x.decide(r.ID, "user:deputy", "approve"); err != nil {
		t.Fatal(err)
	}
	d, _ := x.e.Get(ctx, r.ID)
	ok := false
	for _, dec := range d.Decisions {
		ok = ok || (dec.Kind == "approve" && dec.Actor == "user:deputy" && dec.OnBehalfOf == "user:boss")
	}
	if !ok {
		t.Fatalf("the decision must record who it was on behalf of: %+v", d.Decisions)
	}
	// Outside its window a delegation does nothing.
	r2 := x.create("k2", 1)
	x.now = x.now.Add(48 * time.Hour)
	if err := x.decide(r2.ID, "user:deputy", "approve"); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("expired delegation: %v", err)
	}
}

func TestExpiryAndEscalationTimers(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	p := chain
	p.EscalateAfter, p.EscalateTo, p.ExpiresAfter = "24h", []config.Approver{{Role: "director"}}, "72h"
	x.policy(p)
	r := x.create("k", 1)
	if n, _ := x.e.Tick(ctx); n != 0 {
		t.Fatal("nothing due yet")
	}
	x.now = x.now.Add(25 * time.Hour)
	if n, err := x.e.Tick(ctx); err != nil || n != 1 {
		t.Fatalf("tick = %d, %v", n, err)
	}
	if x.notes.to("user:dir", "approval.escalated") != 1 {
		t.Fatal("the escalation target must be told")
	}
	if err := x.decide(r.ID, "user:dir", "approve"); err != nil {
		t.Fatalf("an escalation target can decide: %v", err)
	}
	// escalation happens once
	r2 := x.create("k2", 1)
	x.now = x.now.Add(25 * time.Hour)
	_, _ = x.e.Tick(ctx)
	_, _ = x.e.Tick(ctx)
	if x.notes.to("user:dir", "approval.escalated") != 2 {
		t.Fatalf("escalations = %d, want one per request", x.notes.to("user:dir", "approval.escalated"))
	}
	x.now = x.now.Add(72 * time.Hour)
	if n, _ := x.e.Tick(ctx); n != 1 {
		t.Fatalf("expiry tick = %d", n)
	}
	if x.status(r2.ID) != "expired" {
		t.Fatalf("status = %s", x.status(r2.ID))
	}
	ev := x.decidedEvents()
	if len(ev) != 2 || !strings.Contains(ev[1], `"outcome":"expired"`) {
		t.Fatalf("events = %v", ev)
	}
	if err := x.decide(r2.ID, "user:boss", "approve"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("expired request decided: %v", err)
	}
}

func TestFailClosedWhenNoApproverCanBeFound(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	_, _, err := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:orphan", IdempotencyKey: "k"})
	if !errors.Is(err, ErrNoApprover) {
		t.Fatalf("requester without a manager: %v, want ErrNoApprover", err)
	}
	// with a fallback it works
	p := chain
	p.Steps = []config.ApprovalStep{{Name: "manager", Approvers: []config.Approver{{ManagerOfRequester: true}}, Fallback: []config.Approver{{Role: "director"}}}}
	x.policy(p)
	r, _, err := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:orphan", IdempotencyKey: "k2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.decide(r.ID, "user:dir", "approve"); err != nil {
		t.Fatalf("fallback approver: %v", err)
	}
	// a role nobody holds
	x.policy(config.ApprovalPolicy{Steps: []config.ApprovalStep{{Name: "x", Approvers: []config.Approver{{Role: "nobody"}}}}})
	if _, _, err := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:emp", IdempotencyKey: "k3"}); !errors.Is(err, ErrNoApprover) {
		t.Fatalf("empty role: %v", err)
	}
	// the requester is the only holder
	x.policy(config.ApprovalPolicy{Steps: []config.ApprovalStep{{Name: "x", Approvers: []config.Approver{{Role: "legal"}}}}})
	if _, _, err := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:lawyer", IdempotencyKey: "k4"}); !errors.Is(err, ErrNoApprover) {
		t.Fatalf("requester is the sole approver: %v", err)
	}
	// no policy and no default chain
	if err := x.e.SyncPolicies(ctx, map[string]config.ApprovalPolicy{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:emp", IdempotencyKey: "k5"}); !errors.Is(err, ErrNoPolicy) {
		t.Fatalf("no policy: %v", err)
	}
	// every step conditional and none applies
	x.policy(config.ApprovalPolicy{Steps: []config.ApprovalStep{{Name: "big", Approvers: []config.Approver{{Role: "finance"}}, When: &config.StepCondition{AmountGTE: i64(1000)}}}})
	if _, _, err := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:emp", Amount: 5, IdempotencyKey: "k6"}); !errors.Is(err, ErrNoApprover) {
		t.Fatalf("no applicable step: %v", err)
	}
}

func TestManifestDefaultChainWhenNoPolicy(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	r, _, err := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:emp", IdempotencyKey: "k", DefaultChain: []string{"manager", "finance"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = x.decide(r.ID, "user:boss", "approve")
	if err := x.decide(r.ID, "user:fin1", "approve"); err != nil {
		t.Fatal(err)
	}
	if x.status(r.ID) != "approved" {
		t.Fatal("default chain not followed")
	}
}

func TestIdempotentCreate(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	req := CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:emp", Ref: "e1", Amount: 5, IdempotencyKey: "same"}
	a, c1, err := x.e.Create(ctx, req)
	if err != nil || !c1 {
		t.Fatalf("%v %v", c1, err)
	}
	b, c2, err := x.e.Create(ctx, req)
	if err != nil || c2 || b.ID != a.ID {
		t.Fatalf("replay: id %s/%s created %v err %v", a.ID, b.ID, c2, err)
	}
	req.Amount = 6
	if _, _, err := x.e.Create(ctx, req); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same key, different body: %v", err)
	}
	// keys are per module
	other := req
	other.ModuleID = "vendors"
	other.Amount = 5
	if _, c, err := x.e.Create(ctx, other); err != nil || !c {
		t.Fatalf("same key from another module must be a new request: %v %v", c, err)
	}
	n := 0
	for _, e := range x.notes.notices {
		if e.Type == "approval.requested" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("approver notifications = %d; the replay must not notify again", n)
	}
}

func TestCreateValidation(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	ok := CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:emp", IdempotencyKey: "k"}
	for name, mut := range map[string]func(*CreateRequest){
		"no key":        func(r *CreateRequest) { r.IdempotencyKey = "" },
		"long key":      func(r *CreateRequest) { r.IdempotencyKey = strings.Repeat("k", 201) },
		"bad requester": func(r *CreateRequest) { r.Requester = "alice" },
		"bad type":      func(r *CreateRequest) { r.Type = "Expense!" },
		"negative":      func(r *CreateRequest) { r.Amount = -1 },
		"long title":    func(r *CreateRequest) { r.Title = strings.Repeat("t", 301) },
		"big payload": func(r *CreateRequest) {
			r.Payload = json.RawMessage(`{"x":"` + strings.Repeat("a", MaxPayloadBytes) + `"}`)
		},
		"payload nonobj": func(r *CreateRequest) { r.Payload = json.RawMessage(`[1]`) },
	} {
		r := ok
		mut(&r)
		if _, _, err := x.e.Create(ctx, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestWaitingInbox(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	r1 := x.create("a", 250_000)
	r2 := x.create("b", 1)
	w, err := x.e.Waiting(ctx, "user:boss", 10)
	if err != nil || len(w) != 2 {
		t.Fatalf("boss waiting = %d, %v", len(w), err)
	}
	if w, _ := x.e.Waiting(ctx, "user:fin1", 10); len(w) != 0 {
		t.Fatal("finance has nothing yet")
	}
	if w, _ := x.e.Waiting(ctx, "user:emp", 10); len(w) != 0 {
		t.Fatal("the requester never waits on their own request")
	}
	_ = x.decide(r1.ID, "user:boss", "approve")
	w, _ = x.e.Waiting(ctx, "user:boss", 10)
	if len(w) != 1 || w[0].ID != r2.ID {
		t.Fatalf("boss waiting after deciding = %+v", w)
	}
	if w, _ := x.e.Waiting(ctx, "user:fin2", 10); len(w) != 1 || w[0].ID != r1.ID {
		t.Fatalf("finance waiting = %+v", w)
	}
	mine, _ := x.e.Requested(ctx, "user:emp", 10)
	if len(mine) != 2 {
		t.Fatalf("requested = %d", len(mine))
	}
	all, _ := x.e.List(ctx, "pending", 1)
	if len(all) != 1 {
		t.Fatal("list limit not applied")
	}
}

func TestVisibility(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	r := x.create("k", 1)
	d, _ := x.e.Get(ctx, r.ID)
	for who, want := range map[string]bool{"user:emp": true, "user:boss": true, "user:stranger": false, "user:fin1": false} {
		if got := x.e.CanView(ctx, d, who); got != want {
			t.Errorf("CanView(%s) = %v, want %v", who, got, want)
		}
	}
	_ = x.rb.Grant(ctx, "t", "user:ops", "operator", "")
	if !x.e.CanView(ctx, d, "user:ops") {
		t.Error("an approvals admin sees everything")
	}
}

func TestDecisionsAreAudited(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	r := x.create("k", 1)
	_ = x.decide(r.ID, "user:boss", "approve")
	recs, _ := audit.New(x.db).List(ctx, 0, 100)
	seen := map[string]bool{}
	for _, rec := range recs {
		seen[rec.Action] = true
		if strings.Contains(rec.Detail, "Laptop") {
			t.Errorf("audit detail carries the free-text title: %s", rec.Detail)
		}
	}
	for _, a := range []string{"approval.request", "approval.approve"} {
		if !seen[a] {
			t.Errorf("no %s audit entry", a)
		}
	}
}

func TestEventsCarryIDsOnly(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	x.policy(chain)
	r, _, _ := x.e.Create(ctx, CreateRequest{ModuleID: exp, Type: "expense", Requester: "user:emp", Ref: "exp-9", Title: "Secret laptop for Mallory", Payload: json.RawMessage(`{"employee_name":"Mallory"}`), IdempotencyKey: "k"})
	_ = x.decide(r.ID, "user:boss", "approve")
	rows, _ := x.db.QueryContext(ctx, `SELECT type, data_json FROM events`)
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var ty, data string
		_ = rows.Scan(&ty, &data)
		n++
		if strings.Contains(data, "Mallory") || strings.Contains(data, "Secret") {
			t.Errorf("%s event carries free text: %s", ty, data)
		}
	}
	if n < 2 {
		t.Fatalf("events = %d, want requested and decided", n)
	}
	for _, nt := range x.notes.notices {
		for _, v := range nt.Data {
			if strings.Contains(v, "Mallory") || strings.Contains(v, "Secret") {
				t.Errorf("notification data carries free text: %v", nt.Data)
			}
		}
	}
}

func TestPolicyValidation(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	bad := map[string]config.ApprovalPolicy{
		"no steps":       {},
		"no approvers":   {Steps: []config.ApprovalStep{{Name: "a"}}},
		"first parallel": {Steps: []config.ApprovalStep{{Name: "a", Parallel: true, Approvers: []config.Approver{{Role: "finance"}}}}},
		"two fields":     {Steps: []config.ApprovalStep{{Name: "a", Approvers: []config.Approver{{Role: "finance", User: "user:x"}}}}},
		"bad duration":   {Steps: []config.ApprovalStep{{Name: "a", Approvers: []config.Approver{{Role: "finance"}}}}, ExpiresAfter: "soon"},
		"dup names":      {Steps: []config.ApprovalStep{{Name: "a", Approvers: []config.Approver{{Role: "finance"}}}, {Name: "a", Approvers: []config.Approver{{Role: "finance"}}}}},
		"escalate no to": {Steps: []config.ApprovalStep{{Name: "a", Approvers: []config.Approver{{Role: "finance"}}}}, EscalateAfter: "1h"},
	}
	for name, p := range bad {
		if err := x.e.SyncPolicies(ctx, map[string]config.ApprovalPolicy{"expense": p}); err == nil {
			t.Errorf("%s: policy accepted", name)
		}
	}
}

func TestTickIsBounded(t *testing.T) {
	x := newEnv(t)
	ctx := context.Background()
	p := chain
	p.ExpiresAfter = "1h"
	x.policy(p)
	for i := 0; i < TickBatch+20; i++ {
		x.create("k"+strings.Repeat("x", i%7)+string(rune('A'+i%26))+string(rune('a'+i/26)), 1)
	}
	x.now = x.now.Add(2 * time.Hour)
	n, err := x.e.Tick(ctx)
	if err != nil || n != TickBatch {
		t.Fatalf("tick processed %d (%v), want the batch size %d", n, err, TickBatch)
	}
	if n, _ = x.e.Tick(ctx); n != 20 {
		t.Fatalf("second tick processed %d, want the remaining 20", n)
	}
}
