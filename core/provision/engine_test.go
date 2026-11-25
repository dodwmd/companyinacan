package provision

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

func created(party, status string) map[string]any {
	return map[string]any{"party_id": party, "kind": "employee", "status": status}
}

func statusChanged(party, from, to, date string) map[string]any {
	return map[string]any{"party_id": party, "kind": "employee", "from": from, "to": to, "effective_date": date}
}

func (h *harness) mustEvent(typ string, data any) {
	h.t.Helper()
	if err := h.event(typ, data); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) exec(q string, args ...any) {
	h.t.Helper()
	if _, err := h.st.db.Exec(q, args...); err != nil {
		h.t.Fatal(err)
	}
}

// linkAccounts gives alice active accounts and group memberships, as a finished joiner would.
func (h *harness) linkAccounts(party string, conns ...string) {
	h.t.Helper()
	for _, c := range conns {
		if err := h.st.LinkAccount(context.Background(), "test", party, c, "ext-"+c+"-"+party[5:9]); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) addGroup(name string, members ...string) {
	h.t.Helper()
	h.exec(`INSERT INTO groups(id,display_name,created_at,updated_at) VALUES (?,?,1,1)`, "grp_"+name, name)
	for _, m := range members {
		h.exec(`INSERT INTO group_members(group_id,party_id) VALUES (?,?)`, "grp_"+name, m)
	}
}

func TestJoinerHappyPath(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "onboarding", "alice@example.com"))
	h.mustEvent(EventPersonCreated, created(alice, "onboarding"))
	h.settle()
	r := h.onlyRun()
	if r.State != RunSucceeded || r.Kind != KindJoiner {
		t.Fatalf("run %+v", r)
	}
	if got, want := h.a.ops(), []string{"create_account", "add_group:all-staff"}; !reflect.DeepEqual(got, want) {
		t.Errorf("app-a ops %v, want %v", got, want)
	}
	if got := h.b.ops(); !reflect.DeepEqual(got, []string{"create_account"}) {
		t.Errorf("app-b ops %v", got)
	}
	if !reflect.DeepEqual(h.tasks.ensured, []string{alice + "/tpl_join"}) {
		t.Errorf("tasks %v", h.tasks.ensured)
	}
	accts, _ := h.st.activeAccounts(context.Background(), alice)
	if len(accts) != 2 {
		t.Errorf("accounts %v", accts)
	}
	// every connector call carried a stable idempotency key
	for _, c := range h.a.calls {
		if !strings.HasPrefix(c.Key, r.ID+":") {
			t.Errorf("key %q", c.Key)
		}
	}
	// events carry ids only
	out := h.outbox()
	if len(out) != 4 {
		t.Fatalf("outbox %d: %v", len(out), out)
	}
	for _, o := range out {
		for _, bad := range []string{"alice@example.com", "Given", "Family", "+61", "user:sso"} {
			if strings.Contains(o, bad) {
				t.Errorf("event leaks %q: %s", bad, o)
			}
		}
	}
	if err := h.st.VerifyAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTriggersAreIdempotent(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "onboarding", "alice@example.com"))
	ev := modclient.Event{ID: "evt_000000000000000000000001", Type: EventPersonCreated}
	ev.Data, _ = json.Marshal(created(alice, "onboarding"))
	for i := 0; i < 3; i++ {
		if err := h.eng.HandleEvent(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	h.settle()
	// onboarding -> active is another joiner trigger: the finished joiner is not repeated
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.mustEvent(EventPersonStatusChanged, statusChanged(alice, "onboarding", "active", "2026-06-10"))
	h.settle()
	if n := len(h.runs()); n != 1 {
		t.Fatalf("%d runs", n)
	}
	if h.a.count("create_account") != 1 {
		t.Fatalf("account created %d times", h.a.count("create_account"))
	}
}

func TestTransientFailuresRetryWithBackoff(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.a.failures[opCreateAccount] = 2
	h.mustEvent(EventPersonCreated, created(alice, "active"))
	h.tick()
	if h.a.count("create_account") != 1 {
		t.Fatalf("calls %d", h.a.count("create_account"))
	}
	h.tick() // inside the backoff window: nothing is called
	if h.a.count("create_account") != 1 {
		t.Fatalf("retried inside the backoff: %d calls", h.a.count("create_account"))
	}
	h.clk.add(11 * time.Second)
	h.tick()
	if h.a.count("create_account") != 2 {
		t.Fatalf("calls %d", h.a.count("create_account"))
	}
	h.clk.add(15 * time.Second) // backoff doubled to 20 s: not due yet
	h.tick()
	if h.a.count("create_account") != 2 {
		t.Fatalf("backoff did not double: %d calls", h.a.count("create_account"))
	}
	h.clk.add(10 * time.Second)
	h.settle()
	r := h.onlyRun()
	if r.State != RunSucceeded || r.Steps[0].Attempts != 3 {
		t.Fatalf("run %s attempts %d", r.State, r.Steps[0].Attempts)
	}
	// all three attempts used the same key: the connector can de-duplicate
	keys := map[string]bool{}
	for _, c := range h.a.calls[:3] {
		keys[c.Key] = true
	}
	if len(keys) != 1 {
		t.Errorf("keys %v", keys)
	}
}

func TestPermanentFailureDeadLettersAndCompensates(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.b.permFail[opCreateAccount] = "http_400"
	h.mustEvent(EventPersonCreated, created(alice, "active"))
	h.settle()
	r := h.onlyRun()
	if r.State != RunCompensated {
		t.Fatalf("state %s", r.State)
	}
	if got, want := h.a.ops(), []string{"create_account", "add_group:all-staff", "remove_group:all-staff", "disable_account"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("app-a ops %v, want %v (newest first)", got, want)
	}
	var dead *Step
	for i := range r.Steps {
		if r.Steps[i].State == StepDead {
			dead = &r.Steps[i]
		}
	}
	if dead == nil || dead.ErrorCode != "http_400" || dead.Attempts != 1 {
		t.Fatalf("dead step %+v", dead)
	}
	accts, _ := h.st.activeAccounts(context.Background(), alice)
	if len(accts) != 0 {
		t.Errorf("accounts still active: %v", accts)
	}
	found := false
	for _, o := range h.outbox() {
		found = found || (strings.HasPrefix(o, EventFailed) && strings.Contains(o, `"reason":"permanent"`) && strings.Contains(o, `"connector":"app-b"`))
	}
	if !found {
		t.Errorf("no provision.failed.v1: %v", h.outbox())
	}
	dl, _ := h.st.DeadLetters(context.Background(), 10)
	if len(dl) != 1 {
		t.Errorf("dead letters %d", len(dl))
	}
}

func TestRetriesExhaustedDeadLetter(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.a.failures[opCreateAccount] = 99
	h.mustEvent(EventPersonCreated, created(alice, "active"))
	for i := 0; i < 6; i++ {
		h.tick()
		h.clk.add(2 * time.Minute)
	}
	r := h.onlyRun()
	if r.State != RunFailed && r.State != RunCompensated {
		t.Fatalf("state %s", r.State)
	}
	if h.a.count("create_account") != 3 {
		t.Fatalf("attempts %d, want exactly MaxAttempts=3", h.a.count("create_account"))
	}
	// a dead-lettered run can be retried by an administrator with fresh attempts
	h.a.failures[opCreateAccount] = 0
	if err := h.eng.RetryRun(context.Background(), "user:admin", r.ID); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if got := h.onlyRun(); got.State != RunSucceeded {
		t.Fatalf("after retry: %s", got.State)
	}
}

func TestCrashResumesWithTheSameKey(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.mustEvent(EventPersonCreated, created(alice, "active"))
	// the process died after the connector call but before the outcome was written
	run := h.onlyRun()
	h.exec(`UPDATE steps SET state='running', attempts=1 WHERE run_id=? AND ord=1`, run.ID)
	_ = h.a.CreateAccount // the first call happened with this key:
	_, _ = h.a.CreateAccount(context.Background(), run.ID+":1", person(alice, "active", "alice@example.com").Identity())
	h.eng = h.newEngine() // a restarted process
	h.settle()
	r := h.onlyRun()
	if r.State != RunSucceeded {
		t.Fatalf("state %s", r.State)
	}
	if r.Steps[0].Attempts != 2 {
		t.Errorf("attempts %d", r.Steps[0].Attempts)
	}
	same := 0
	for _, c := range h.a.calls {
		if c.Op == opCreateAccount && c.Key == run.ID+":1" {
			same++
		}
	}
	if same != 2 {
		t.Errorf("the resumed step used a different key (%d calls with the original)", same)
	}
}

func TestLeaverEffectiveDated(t *testing.T) {
	h := newHarness(t, nil)
	p := person(alice, "offboarding", "alice@example.com")
	p.EndDate = "2026-06-12"
	h.dir.put(p)
	h.linkAccounts(alice, "app-a", "app-b")
	h.exec(`INSERT INTO applied_groups(party_id,connector,grp) VALUES (?,?,?)`, alice, "app-a", "all-staff")
	h.mustEvent(EventPersonStatusChanged, statusChanged(alice, "active", "offboarding", "2026-06-05"))
	h.settle()
	r := h.onlyRun()
	if r.State != RunScheduled {
		t.Fatalf("state %s, want scheduled until the last day", r.State)
	}
	if len(h.a.calls)+len(h.b.calls)+len(h.core.calls) != 0 {
		t.Fatal("access was touched before the last day")
	}
	// the day before: still nothing
	h.clk.add(36 * time.Hour) // 2026-06-11 21:00
	h.settle()
	if len(h.a.calls) != 0 {
		t.Fatal("access was removed a day early")
	}
	// the last day: people applies the status change and publishes the offboarding
	h.clk.add(4 * time.Hour) // 2026-06-12 01:00
	p.Status = "former"
	h.dir.put(p)
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-12"})
	h.settle()
	r = h.onlyRun()
	if r.State != RunSucceeded {
		t.Fatalf("state %s steps %+v", r.State, r.Steps)
	}
	if want := [][2]string{{alice, "user:sso-aaaa"}}; !reflect.DeepEqual(h.core.calls, want) {
		t.Errorf("revocations %v", h.core.calls)
	}
	if got, want := h.a.ops(), []string{"remove_group:all-staff", "disable_account"}; !reflect.DeepEqual(got, want) {
		t.Errorf("app-a %v", got)
	}
	if got := h.b.ops(); !reflect.DeepEqual(got, []string{"disable_account"}) {
		t.Errorf("app-b %v", got)
	}
	if accts, _ := h.st.activeAccounts(context.Background(), alice); len(accts) != 0 {
		t.Errorf("active accounts %v", accts)
	}
	if !reflect.DeepEqual(h.tasks.ensured, []string{alice + "/tpl_leave"}) {
		t.Errorf("tasks %v", h.tasks.ensured)
	}
}

func TestLeaverWithNoLastDayWaitsForTheOffboardedEvent(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "offboarding", "alice@example.com"))
	h.linkAccounts(alice, "app-a")
	h.mustEvent(EventPersonStatusChanged, statusChanged(alice, "active", "offboarding", "2026-06-05"))
	h.clk.add(24 * time.Hour * 400)
	h.settle()
	if len(h.a.calls) != 0 {
		t.Fatal("access removed with no last day on record")
	}
}

func TestUrgentTerminationRemovesAccessAtOnce(t *testing.T) {
	h := newHarness(t, nil)
	p := person(alice, "offboarding", "alice@example.com")
	p.EndDate = "2026-06-30"
	h.dir.put(p)
	h.linkAccounts(alice, "app-a", "app-b")
	h.addGroup("admins", alice) // privileged: normally needs a second person
	h.mustEvent(EventPersonStatusChanged, statusChanged(alice, "active", "offboarding", "2026-06-10"))
	h.settle()
	if h.onlyRun().State != RunScheduled || len(h.a.calls) != 0 {
		t.Fatal("expected a scheduled run")
	}
	if err := h.eng.FlagUrgent(context.Background(), "user:sso-aaaa", alice); !errors.Is(err, ErrForbidden) {
		t.Fatalf("self flag: %v", err)
	}
	if err := h.eng.FlagUrgent(context.Background(), "user:hr-lead", alice); err != nil {
		t.Fatal(err)
	}
	h.settle()
	r := h.onlyRun()
	if !r.Urgent || h.a.count("disable_account") != 1 || h.b.count("disable_account") != 1 {
		t.Fatalf("urgent %v, disables %d/%d, state %s", r.Urgent, h.a.count("disable_account"), h.b.count("disable_account"), r.State)
	}
	if len(h.appr.created) != 0 {
		t.Errorf("an urgent removal waited for %d approvals", len(h.appr.created))
	}
	// the can-core kill switch waits for the directory to say "former": it is retried, not skipped
	if len(h.core.calls) != 0 {
		t.Errorf("revoked can-core access for a person who is still offboarding")
	}
	if err := h.eng.FlagUrgent(context.Background(), "user:hr-lead", bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown person: %v", err)
	}
}

func TestFlagUrgentRefusesActivePeople(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	if err := h.eng.FlagUrgent(context.Background(), "user:hr-lead", alice); !errors.Is(err, ErrConflict) {
		t.Fatalf("flagging an active person: %v", err)
	}
}

func TestPrivilegedLeaverNeedsASecondPerson(t *testing.T) {
	h := newHarness(t, nil)
	p := person(alice, "former", "alice@example.com")
	h.dir.put(p)
	h.linkAccounts(alice, "app-a")
	h.addGroup("admins", alice)
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	h.settle()
	r := h.onlyRun()
	if r.State != RunAwaiting {
		t.Fatalf("state %s", r.State)
	}
	if h.a.count("disable_account") != 0 {
		t.Fatal("a privileged account was disabled without a second person")
	}
	h.settle()
	h.tick()
	if len(h.appr.created) != 1 {
		t.Fatalf("approvals requested: %d", len(h.appr.created))
	}
	ap := h.appr.created[0]
	if ap.Type != "privileged-leaver" || ap.Requester != "party:"+alice || strings.Contains(ap.Title, "alice@") {
		t.Fatalf("approval %+v", ap)
	}
	var aprID string
	for _, s := range h.onlyRun().Steps {
		if s.State == StepAwaiting {
			aprID = s.ApprovalID
		}
	}
	decide := func(approvers ...string) error {
		return h.event(EventApprovalDecided, map[string]any{"request_id": aprID, "module_id": "provisioning", "type": "privileged-leaver", "requester": "party:" + alice, "outcome": "approved", "approvers": approvers})
	}
	// the person's own account cannot approve their removal
	if err := decide("user:sso-aaaa"); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if h.a.count("disable_account") != 0 {
		t.Fatal("the leaver approved their own offboarding")
	}
	if got := h.onlyRun(); got.State != RunFailed {
		t.Fatalf("state %s", got.State)
	}
}

func TestPrivilegedLeaverProceedsOnSecondPersonApproval(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.linkAccounts(alice, "app-a")
	h.addGroup("admins", alice)
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	h.settle()
	var aprID string
	for _, s := range h.onlyRun().Steps {
		if s.State == StepAwaiting {
			aprID = s.ApprovalID
		}
	}
	// a decision for a request that is not ours, or a different type, changes nothing
	h.mustEvent(EventApprovalDecided, map[string]any{"request_id": "apr_unknown", "type": "privileged-leaver", "outcome": "approved", "approvers": []string{"user:boss"}})
	if h.a.count("disable_account") != 0 {
		t.Fatal("unknown approval released a step")
	}
	h.mustEvent(EventApprovalDecided, map[string]any{"request_id": aprID, "type": "privileged-leaver", "outcome": "approved", "approvers": []string{"user:boss"}})
	h.settle()
	if h.a.count("disable_account") != 1 || h.onlyRun().State != RunSucceeded {
		t.Fatalf("disables %d state %s", h.a.count("disable_account"), h.onlyRun().State)
	}
	if err := h.st.VerifyAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedApprovalFailsClosed(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.addGroup("admins", alice)
	h.exec(`INSERT INTO group_members(group_id,party_id) VALUES ('grp_admins',?)`, bob) // unrelated
	h.mustEvent(EventPersonCreated, created(alice, "active"))
	h.settle()
	r := h.onlyRun()
	// joining the privileged group needs approval: the account exists, the privileged membership does not
	if r.State != RunAwaiting || h.a.count("add_group:admins") != 0 {
		t.Fatalf("state %s ops %v", r.State, h.a.ops())
	}
	h.appr.status["apr_"+strings.Repeat("0", 24)] = "rejected"
	h.tick()
	h.settle()
	if got := h.onlyRun(); got.State != RunCompensated {
		t.Fatalf("state %s", got.State)
	}
	if h.a.count("add_group:admins") != 0 {
		t.Fatal("privileged group granted after rejection")
	}
}

func TestNoApprovalsEngineMeansNoSensitiveSteps(t *testing.T) {
	h := newHarness(t, nil)
	h.eng.Approvals = nil
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.linkAccounts(alice, "app-a")
	h.addGroup("admins", alice)
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	h.settle()
	if h.a.count("disable_account") != 0 {
		t.Fatal("sensitive step ran without an approvals engine")
	}
}

func TestEventsAreOnlyHints(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	// an offboarded event for a person the directory says is active starts nothing
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	h.mustEvent(EventPersonStatusChanged, statusChanged(alice, "active", "offboarding", "2026-06-10"))
	// a joiner for a person who is not in the directory starts nothing
	h.mustEvent(EventPersonCreated, created(bob, "active"))
	// a candidate gets no access
	h.dir.put(person(carol, "candidate", "carol@example.com"))
	h.mustEvent(EventPersonCreated, created(carol, "candidate"))
	// malformed data is ignored, not retried forever
	h.mustEvent(EventPersonCreated, map[string]any{"party_id": "nope"})
	h.settle()
	if n := len(h.runs()); n != 0 {
		t.Fatalf("%d runs started", n)
	}
	if !contains(h.auditActions(), "trigger.ignored") {
		t.Error("ignored triggers are not audited")
	}
}

func TestChecklistActionsTriggerWorkflowsAfterVerification(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.mustEvent(EventTaskCompleted, map[string]any{"task_id": "task_x", "party_id": alice, "action": "identity.provision"})
	h.mustEvent(EventTaskCompleted, map[string]any{"party_id": alice, "action": "identity.deprovision"}) // active: refused
	h.mustEvent(EventTaskCompleted, map[string]any{"party_id": alice, "action": "something.else"})
	rs := h.runs()
	if len(rs) != 1 || rs[0].Kind != KindJoiner {
		t.Fatalf("runs %+v", rs)
	}
}

func TestDirectoryOutageIsRetriedNotLost(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	ev := modclient.Event{ID: "evt_00000000000000000000aaaa", Type: EventPersonCreated}
	ev.Data, _ = json.Marshal(created(alice, "active"))
	h.dir.down = true
	if err := h.eng.HandleEvent(context.Background(), ev); err == nil {
		t.Fatal("an outage was acknowledged")
	}
	h.dir.down = false
	if err := h.eng.HandleEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(h.runs()) != 1 {
		t.Fatal("the redelivered event did not start the run")
	}
}

func TestDryRunCallsNothing(t *testing.T) {
	h := newHarness(t, func(p *Policy) { p.DryRun = true })
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.mustEvent(EventPersonCreated, created(alice, "active"))
	h.settle()
	r := h.onlyRun()
	if r.State != RunSucceeded || !r.DryRun || len(h.a.calls)+len(h.b.calls)+len(h.tasks.ensured) != 0 {
		t.Fatalf("dry run: %+v", r)
	}
	for _, s := range r.Steps {
		if s.State != StepSkipped || s.ErrorCode != "dry_run" {
			t.Errorf("step %+v", s)
		}
	}
	if accts, _ := h.st.activeAccounts(context.Background(), alice); len(accts) != 0 {
		t.Error("a dry run recorded accounts")
	}
	// the plan endpoint is read-only
	pr, err := h.eng.Plan(context.Background(), StartRequest{Kind: KindJoiner, PartyID: alice})
	if err != nil || len(pr.Steps) == 0 {
		t.Fatalf("plan %v %v", pr, err)
	}
}

func TestStubConnectorFailsLoudlyButDoesNotBlockOthers(t *testing.T) {
	h := newHarness(t, nil)
	cs, _ := NewConnectorSet(&Stub{ConnName: "app-b", Kind: "slack"}, h.a)
	h.eng.Conns = cs
	h.dir.put(person(alice, "former", "alice@example.com"))
	h.linkAccounts(alice, "app-a", "app-b")
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	h.settle()
	r := h.onlyRun()
	if r.State != RunFailed {
		t.Fatalf("state %s", r.State)
	}
	if h.a.count("disable_account") != 1 {
		t.Fatal("a dead stub blocked the working connector")
	}
	found := false
	for _, s := range r.Steps {
		found = found || (s.Connector == "app-b" && s.State == StepDead && s.ErrorCode == "unimplemented")
	}
	if !found {
		t.Fatalf("steps %+v", r.Steps)
	}
}

func TestMoverChangesOnlyTheDifference(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.linkAccounts(alice, "app-a")
	h.addGroup("eng", alice)
	h.exec(`INSERT INTO applied_groups(party_id,connector,grp) VALUES (?,?,?),(?,?,?)`, alice, "app-a", "all-staff", alice, "app-a", "sales")
	run, created, err := h.eng.Start(context.Background(), StartRequest{Kind: KindMover, PartyID: alice, Trigger: "api", Key: "move-1", Initiator: "user:hr"})
	if err != nil || !created {
		t.Fatal(err)
	}
	h.settle()
	if got, want := h.a.ops(), []string{"update_account", "add_group:eng", "remove_group:sales"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ops %v want %v", got, want)
	}
	if len(h.b.calls) != 0 {
		t.Error("a connector without an account was touched")
	}
	// the same idempotency key returns the same run
	again, created2, err := h.eng.Start(context.Background(), StartRequest{Kind: KindMover, PartyID: alice, Trigger: "api", Key: "move-1"})
	if err != nil || created2 || again.ID != run.ID {
		t.Fatalf("again %v %v %v", again.ID, created2, err)
	}
}

func TestOptionalTaskFailureDoesNotFailTheRun(t *testing.T) {
	h := newHarness(t, nil)
	h.tasks.err = ErrUnavailable
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.mustEvent(EventPersonCreated, created(alice, "active"))
	for i := 0; i < 8; i++ {
		h.settle()
		h.clk.add(2 * time.Minute)
	}
	r := h.onlyRun()
	if r.State != RunSucceeded {
		t.Fatalf("state %s", r.State)
	}
	last := r.Steps[len(r.Steps)-1]
	if last.Op != opCreateTasks || last.State != StepSkipped {
		t.Fatalf("last step %+v", last)
	}
}

func TestCalledOffOffboardingCancelsAWaitingLeaver(t *testing.T) {
	h := newHarness(t, nil)
	p := person(alice, "offboarding", "alice@example.com")
	p.EndDate = "2026-06-30"
	h.dir.put(p)
	h.linkAccounts(alice, "app-a")
	h.mustEvent(EventPersonStatusChanged, statusChanged(alice, "active", "offboarding", "2026-06-10"))
	h.settle()
	p.Status = "active"
	h.dir.put(p)
	h.mustEvent(EventPersonStatusChanged, statusChanged(alice, "offboarding", "active", "2026-06-11"))
	if r := h.onlyRun(); r.State != RunCancelled {
		t.Fatalf("state %s", r.State)
	}
}

func TestLeaverRevokesCoreOnlyForCoreSubjects(t *testing.T) {
	h := newHarness(t, nil)
	p := person(alice, "former", "alice@example.com")
	p.SSOSubject = "alice-at-idp" // not a can-core subject: nothing to revoke there
	h.dir.put(p)
	h.linkAccounts(alice, "app-a")
	h.mustEvent(EventPersonOffboarded, map[string]any{"party_id": alice, "kind": "employee", "effective_date": "2026-06-10"})
	h.settle()
	if len(h.core.calls) != 0 || h.onlyRun().State != RunSucceeded {
		t.Fatalf("revocations %v state %s", h.core.calls, h.onlyRun().State)
	}
}

func TestOutboxDeliveryIsAtLeastOnce(t *testing.T) {
	h := newHarness(t, nil)
	h.dir.put(person(alice, "active", "alice@example.com"))
	h.mustEvent(EventPersonCreated, created(alice, "active"))
	h.settle()
	pub := &fakePub{fail: true}
	n, err := h.eng.DeliverPending(context.Background(), pub)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	pub.fail = false
	h.clk.add(2 * time.Minute)
	n, err = h.eng.DeliverPending(context.Background(), pub)
	if err != nil || n == 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, e := range pub.sent {
		var env struct {
			ID, Type string
			Data     map[string]any
		}
		if json.Unmarshal(e, &env) != nil || !strings.HasPrefix(env.ID, "evt_") || !strings.HasPrefix(env.Type, "provision.") {
			t.Fatalf("envelope %s", e)
		}
	}
	if left := h.outbox(); len(left) != 0 {
		t.Fatalf("%d events left", len(left))
	}
}

type fakePub struct {
	fail bool
	sent [][]byte
}

func (p *fakePub) PublishEnvelope(_ context.Context, env []byte) error {
	if p.fail {
		return errors.New("down")
	}
	p.sent = append(p.sent, env)
	return nil
}

func TestPlanBoundsAndBackoff(t *testing.T) {
	p := testPolicy()
	p.BaseDelay, p.MaxDelay = time.Second, 10*time.Second
	var got []time.Duration
	for i := 1; i <= 6; i++ {
		got = append(got, p.backoff(i))
	}
	want := []time.Duration{1, 2, 4, 8, 10, 10}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff %v", got)
		}
	}
	p.MaxStepsPerRun = 2
	cs, _ := NewConnectorSet(newFakeConn("app-a"), newFakeConn("app-b"))
	if _, err := p.plan(cs, planInput{Kind: KindJoiner, Person: person(alice, "active", "a@example.com")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized plan: %v", err)
	}
}
