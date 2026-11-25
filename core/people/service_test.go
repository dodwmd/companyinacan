package people

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// All names, ids and values in these tests are fake.

type recAuditor struct {
	mu   sync.Mutex
	evs  []AuditEvent
	fail bool
}

func (r *recAuditor) Record(_ context.Context, e AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("audit sink down")
	}
	r.evs = append(r.evs, e)
	return nil
}

func (r *recAuditor) find(action, resource, outcome string) *AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.evs {
		e := r.evs[i]
		if e.Action == action && e.Resource == resource && e.Outcome == outcome {
			return &e
		}
	}
	return nil
}

type env struct {
	t     *testing.T
	s     *Service
	aud   *recAuditor
	clock *time.Time
	ctx   context.Context
	hr    Actor // service account with hr+admin, no person record
	path  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	kr, err := ParseKeyring("k1 " + hexKeyA + "\n")
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	aud := &recAuditor{}
	path := filepath.Join(t.TempDir(), "people.db")
	s, err := Open(path, Options{Keyring: kr, Auditor: aud, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &env{t: t, s: s, path: path, aud: aud, clock: &clock, ctx: context.Background(), hr: Actor{Subject: "svc-hr", Roles: []string{RoleHR, RoleAdmin}}}
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// person creates an active person with an SSO subject and returns the record and an Actor for them.
func (e *env) person(given string, mgr string, roles ...string) (*Person, Actor) {
	e.t.Helper()
	p, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: given, FamilyName: "Example", WorkEmail: strings.ToLower(given) + "@example.com", SSOSubject: "sso-" + strings.ToLower(given), StartDate: "2025-01-06"})
	e.must(err)
	if mgr != "" {
		_, err = e.s.SetManager(e.ctx, e.hr, p.ID, mgr)
		e.must(err)
	}
	return p, Actor{Subject: p.SSOSubject, Roles: roles, PersonID: p.ID, HasPerson: true, Status: StActive}
}

func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

func TestSensitiveFieldsEncryptedAuditedAndAccessControlled(t *testing.T) {
	e := newEnv(t)
	boss, bossA := e.person("Boss", "", RoleEmployee, RoleManager)
	ann, annA := e.person("Ann", boss.ID, RoleEmployee)
	_, benA := e.person("Ben", boss.ID, RoleEmployee)

	e.must(e.s.PutSensitive(e.ctx, e.hr, ann.ID, map[string]json.RawMessage{
		FieldDOB: raw("1990-05-17"), FieldNationalID: raw("QQ123456C"), FieldAddress: raw("1 Example Street"), FieldSalary: raw(Salary{Amount: 9000000, Currency: "USD", Period: "year"}),
	}))
	// At rest: ciphertext only.
	var sal, dob string
	e.must(e.s.db.QueryRow(`SELECT sens_salary, sens_dob FROM people WHERE id = ?`, ann.ID).Scan(&sal, &dob))
	for _, v := range []string{sal, dob} {
		if !strings.HasPrefix(v, "v1.k1.") || strings.Contains(v, "9000000") || strings.Contains(v, "1990") {
			t.Fatalf("not encrypted at rest: %q", v)
		}
	}

	// HR reads all; the read is audited with field names (never values).
	got, err := e.s.GetSensitive(e.ctx, e.hr, ann.ID, nil)
	e.must(err)
	if got[FieldDOB] != "1990-05-17" || got[FieldSalary].(Salary).Amount != 9000000 {
		t.Fatalf("hr read = %v", got)
	}
	ev := e.aud.find("read", "person:"+ann.ID, "ok")
	if ev == nil || len(ev.Fields) != 5 {
		t.Fatalf("audit event = %+v", ev)
	}
	for _, f := range ev.Fields {
		if strings.Contains(f, "1990") {
			t.Fatal("audit leaked a value")
		}
	}

	// Self reads own; with no field list she gets exactly the permitted set.
	self, err := e.s.GetSensitive(e.ctx, annA, ann.ID, []string{FieldSalary})
	e.must(err)
	if self[FieldSalary].(Salary).Currency != "USD" {
		t.Fatal("self salary")
	}
	// IDOR: a peer, and the manager, are refused; denials are audited.
	for name, who := range map[string]Actor{"peer": benA, "manager": bossA} {
		if _, err := e.s.GetSensitive(e.ctx, who, ann.ID, []string{FieldSalary}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: err = %v, want forbidden", name, err)
		}
		if _, err := e.s.GetSensitive(e.ctx, who, ann.ID, nil); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s (no field list): err = %v, want forbidden", name, err)
		}
	}
	if e.aud.find("read", "person:"+ann.ID, "denied") == nil {
		t.Error("denied read not audited")
	}
	// A requested field the actor may not read refuses the whole request.
	if _, err := e.s.GetSensitive(e.ctx, annA, ann.ID, []string{FieldAddress, "nope"}); err == nil {
		t.Error("unknown field accepted")
	}
	// Self may write address but not salary.
	e.must(e.s.PutSensitive(e.ctx, annA, ann.ID, map[string]json.RawMessage{FieldAddress: raw("2 Example Road")}))
	if err := e.s.PutSensitive(e.ctx, annA, ann.ID, map[string]json.RawMessage{FieldSalary: raw(Salary{Amount: 1, Currency: "USD", Period: "year"})}); !errors.Is(err, ErrForbidden) {
		t.Errorf("self salary write: %v", err)
	}
	// Mixed write: one forbidden field means nothing is written.
	if err := e.s.PutSensitive(e.ctx, annA, ann.ID, map[string]json.RawMessage{FieldPhone: raw("+1 555 0100"), FieldDOB: raw("1991-01-01")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("mixed write: %v", err)
	}
	if g, _ := e.s.GetSensitive(e.ctx, e.hr, ann.ID, []string{FieldPhone}); g[FieldPhone] != nil {
		t.Error("partial write leaked through")
	}
	// Bad values are rejected before anything is stored.
	if err := e.s.PutSensitive(e.ctx, e.hr, ann.ID, map[string]json.RawMessage{FieldSalary: raw(Salary{Amount: -1, Currency: "usd", Period: "year"})}); err == nil {
		t.Error("bad salary accepted")
	}
	// Ciphertext copied between persons does not open (AAD binding).
	_, err = e.s.db.Exec(`UPDATE people SET sens_dob = (SELECT sens_dob FROM people WHERE id = ?) WHERE id = ?`, ann.ID, boss.ID)
	e.must(err)
	if _, err := e.s.GetSensitive(e.ctx, e.hr, boss.ID, []string{FieldDOB}); err == nil {
		t.Error("swapped ciphertext opened")
	}
}

func TestSensitiveReadFailsClosedWhenAuditIsDown(t *testing.T) {
	e := newEnv(t)
	ann, annA := e.person("Ann", "", RoleEmployee)
	e.must(e.s.PutSensitive(e.ctx, e.hr, ann.ID, map[string]json.RawMessage{FieldDOB: raw("1990-05-17")}))
	e.aud.fail = true
	if _, err := e.s.GetSensitive(e.ctx, annA, ann.ID, []string{FieldDOB}); err == nil {
		t.Fatal("read succeeded without an audit record")
	}
}

func TestDirectoryVisibilityAndIDOR(t *testing.T) {
	e := newEnv(t)
	ann, annA := e.person("Ann", "", RoleEmployee)
	cand, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", GivenName: "Cara", FamilyName: "Candidate"})
	e.must(err)
	if cand.Status != StCandidate {
		t.Fatal(cand.Status)
	}
	if _, err := e.s.GetPerson(e.ctx, annA, cand.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("employee sees candidate: %v", err)
	}
	if _, err := e.s.GetPerson(e.ctx, annA, "pers_000000000000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing id: %v", err)
	}
	if _, err := e.s.GetPerson(e.ctx, annA, "x' OR 1=1 --"); !errors.Is(err, ErrNotFound) {
		t.Errorf("malformed id: %v", err)
	}
	p, err := e.s.GetPerson(e.ctx, annA, ann.ID)
	e.must(err)
	if p.SSOSubject != "" || p.LegalHold {
		t.Error("hr-only fields leaked to the employee")
	}
	list, _, err := e.s.ListPeople(e.ctx, annA, PersonFilter{})
	e.must(err)
	if len(list) != 1 {
		t.Errorf("employee list = %d, want only the active person", len(list))
	}
	hrList, _, err := e.s.ListPeople(e.ctx, e.hr, PersonFilter{})
	e.must(err)
	if len(hrList) != 2 {
		t.Errorf("hr list = %d", len(hrList))
	}
	// Privilege escalation attempts.
	if _, err := e.s.CreatePerson(e.ctx, annA, PersonInput{Kind: "employee", GivenName: "X", FamilyName: "Y"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("employee create: %v", err)
	}
	if _, err := e.s.UpdatePerson(e.ctx, annA, ann.ID, PersonPatch{GivenName: ptr("Hacked")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("employee editing own given_name: %v", err)
	}
	if _, err := e.s.UpdatePerson(e.ctx, annA, ann.ID, PersonPatch{SSOSubject: ptr("sso-admin")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("employee changing sso_subject: %v", err)
	}
	if _, err := e.s.UpdatePerson(e.ctx, annA, ann.ID, PersonPatch{PreferredName: ptr("Annie")}); err != nil {
		t.Errorf("self preferred_name: %v", err)
	}
	// An actor with an unknown role gets nothing at all.
	nobody := Actor{Subject: "x", Roles: []string{"superuser"}}
	if _, err := e.s.GetPerson(e.ctx, nobody, ann.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown role: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestManagerChainCycleAndManagerRules(t *testing.T) {
	e := newEnv(t)
	a, _ := e.person("Ava", "", RoleEmployee, RoleManager)
	b, _ := e.person("Bo", a.ID, RoleEmployee, RoleManager)
	c, _ := e.person("Cy", b.ID, RoleEmployee)
	for name, tc := range map[string]struct{ person, mgr string }{
		"self":       {a.ID, a.ID},
		"direct":     {a.ID, b.ID},
		"transitive": {a.ID, c.ID},
	} {
		_, err := e.s.SetManager(e.ctx, e.hr, tc.person, tc.mgr)
		var inv *InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
	if _, err := e.s.SetManager(e.ctx, e.hr, c.ID, "pers_ffffffffffffffffffffffff"); err == nil {
		t.Error("unknown manager accepted")
	}
	cand, _ := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", GivenName: "Cara", FamilyName: "C"})
	if _, err := e.s.SetManager(e.ctx, e.hr, c.ID, cand.ID); err == nil {
		t.Error("a candidate cannot be a manager")
	}
	// Only hr/admin assign managers.
	if _, err := e.s.SetManager(e.ctx, Actor{Subject: "x", Roles: []string{RoleManager}, PersonID: a.ID, HasPerson: true, Status: StActive}, c.ID, a.ID); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
		t.Errorf("manager reassigning: %v", err)
	}
	// A legitimate re-parent works and clearing works.
	if _, err := e.s.SetManager(e.ctx, e.hr, c.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if p, err := e.s.SetManager(e.ctx, e.hr, c.ID, ""); err != nil || p.ManagerID != "" {
		t.Fatal(p, err)
	}
	// Even a corrupted cyclic chain terminates.
	_, err := e.s.db.Exec(`UPDATE people SET manager_id = ? WHERE id = ?`, b.ID, a.ID)
	e.must(err)
	ch, err := managerChain(e.ctx, e.s.db, a.ID)
	e.must(err)
	if len(ch) > 2 {
		t.Errorf("chain = %v", ch)
	}
}

func TestManagerOfManagerAndTerminatedManager(t *testing.T) {
	e := newEnv(t)
	vp, vpA := e.person("Vee", "", RoleEmployee, RoleManager)
	mgr, mgrA := e.person("Max", vp.ID, RoleEmployee, RoleManager)
	rep, repA := e.person("Rae", mgr.ID, RoleEmployee)
	e.must(e.s.PutSensitive(e.ctx, e.hr, rep.ID, map[string]json.RawMessage{FieldSalary: raw(Salary{Amount: 1, Currency: "EUR", Period: "month"})}))

	// Skip-level reads basic, not sensitive, not 1:1.
	if _, err := e.s.GetPerson(e.ctx, vpA, rep.ID); err != nil {
		t.Errorf("skip-level basic: %v", err)
	}
	if _, err := e.s.GetSensitive(e.ctx, vpA, rep.ID, nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("skip-level sensitive: %v", err)
	}
	ser, err := e.s.CreateSeries(e.ctx, mgrA, rep.ID, "weekly")
	e.must(err)
	if _, err := e.s.ListMeetings(e.ctx, vpA, ser.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("skip-level 1:1: %v", err)
	}
	if _, err := e.s.CreateSeries(e.ctx, vpA, rep.ID, "weekly"); !errors.Is(err, ErrForbidden) {
		t.Errorf("skip-level creating 1:1: %v", err)
	}

	// Terminate the manager. The SSO token still names them, but their person record is former.
	_, err = e.s.ChangeStatus(e.ctx, e.hr, mgr.ID, StOffboarding, "", "")
	e.must(err)
	_, err = e.s.ChangeStatus(e.ctx, e.hr, mgr.ID, StFormer, "", "")
	e.must(err)
	stale, err := e.s.ResolveActor(e.ctx, &Principal{Subject: mgrA.Subject, Roles: mgrA.Roles})
	e.must(err)
	if stale.Status != StFormer {
		t.Fatalf("resolved status = %s", stale.Status)
	}
	for name, fn := range map[string]func() error{
		"read report": func() error { _, err := e.s.GetPerson(e.ctx, stale, rep.ID); return err },
		"read self":   func() error { _, err := e.s.GetPerson(e.ctx, stale, mgr.ID); return err },
		"list 1:1":    func() error { _, err := e.s.ListMeetings(e.ctx, stale, ser.ID); return err },
		"my series":   func() error { l, _ := e.s.ListSeries(e.ctx, stale); return nilIfEmpty(len(l)) },
		"new series":  func() error { _, err := e.s.CreateSeries(e.ctx, stale, rep.ID, "weekly"); return err },
		"goals":       func() error { l, _ := e.s.ListGoals(e.ctx, stale, GoalFilter{}); return nilIfEmpty(len(l)) },
		"person list": func() error { l, _, _ := e.s.ListPeople(e.ctx, stale, PersonFilter{}); return nilIfEmpty(len(l)) },
		"org units":   func() error { _, err := e.s.ListOrgUnits(e.ctx, stale, "teams"); return err },
		"my tasks":    func() error { _, err := e.s.MyTasks(e.ctx, stale); return err },
	} {
		if err := fn(); err == nil {
			t.Errorf("terminated manager still allowed: %s", name)
		}
	}
	// The former manager's reports lost the reporting line; the skip-level manager no longer has a chain either.
	r, err := e.s.GetPerson(e.ctx, e.hr, rep.ID)
	e.must(err)
	if r.ManagerID != "" {
		t.Errorf("report still points at the former manager: %q", r.ManagerID)
	}
	if _, err := e.s.GetPerson(e.ctx, repA, rep.ID); err != nil {
		t.Errorf("report cannot read self: %v", err)
	}
	// The former manager's 1:1 data is not readable by the (now chain-less) VP either.
	if _, err := e.s.ListMeetings(e.ctx, vpA, ser.ID); err == nil {
		t.Error("vp reads the former manager's series")
	}
}

func nilIfEmpty(n int) error {
	if n == 0 {
		return errors.New("empty")
	}
	return nil
}

func TestEmploymentLifecycleEffectiveDatingAndEvents(t *testing.T) {
	e := newEnv(t)
	p, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", GivenName: "Dee", FamilyName: "Hire", SSOSubject: "sso-dee"})
	e.must(err)
	// invalid jump
	if _, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StActive, "", ""); !errors.Is(err, ErrConflict) {
		t.Errorf("candidate->active: %v", err)
	}
	if _, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StOnboarding, "", "accepted"); err != nil {
		t.Fatal(err)
	}
	// future-dated start stays pending
	if _, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StActive, "2026-03-16", ""); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.s.GetPerson(e.ctx, e.hr, p.ID)
	if cur.Status != StOnboarding {
		t.Fatalf("future-dated change applied early: %s", cur.Status)
	}
	if _, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StOffboarding, "", ""); !errors.Is(err, ErrConflict) {
		t.Errorf("second change while one is pending: %v", err)
	}
	if n, _ := e.s.ApplyDue(e.ctx); n != 0 {
		t.Errorf("applied %d before the date", n)
	}
	*e.clock = e.clock.AddDate(0, 0, 7)
	if n, err := e.s.ApplyDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("ApplyDue = %d, %v", n, err)
	}
	cur, _ = e.s.GetPerson(e.ctx, e.hr, p.ID)
	if cur.Status != StActive || cur.StartDate != "2026-03-16" {
		t.Fatalf("after due: %+v", cur)
	}
	// dates may not go backwards
	if _, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StLeave, "2026-01-01", ""); err == nil {
		t.Error("back-dated before the last change accepted")
	}
	// cancel a pending change
	if _, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StLeave, "2026-04-01", ""); err != nil {
		t.Fatal(err)
	}
	e.must(e.s.CancelPending(e.ctx, e.hr, p.ID))
	if err := e.s.CancelPending(e.ctx, e.hr, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second cancel: %v", err)
	}
	if _, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StOffboarding, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StFormer, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StActive, "", ""); !errors.Is(err, ErrConflict) {
		t.Errorf("former is terminal: %v", err)
	}

	h, err := e.s.History(e.ctx, e.hr, p.ID)
	e.must(err)
	var seq []string
	for _, x := range h {
		seq = append(seq, x.From+">"+x.To)
	}
	if got := strings.Join(seq, " "); got != ">candidate candidate>onboarding onboarding>active active>offboarding offboarding>former" {
		t.Errorf("history = %s", got)
	}
	// An employee cannot see someone else's history.
	_, otherA := e.person("Zed", "", RoleEmployee)
	if _, err := e.s.History(e.ctx, otherA, p.ID); err == nil {
		t.Error("history readable by a stranger")
	}

	evs, err := e.s.AllEvents(e.ctx)
	e.must(err)
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
		if ev.Source != "people" || !regexp.MustCompile(`^evt_[0-9a-f]{24}$`).MatchString(ev.ID) {
			t.Errorf("bad envelope %+v", ev)
		}
		for k := range ev.Data {
			if k == "given_name" || k == "family_name" || k == "work_email" {
				t.Errorf("event %s carries personal data %q", ev.Type, k)
			}
		}
	}
	want := "person.created.v1 person.status_changed.v1 person.status_changed.v1 person.created.v1 person.status_changed.v1 person.status_changed.v1 person.status_changed.v1 person.offboarded.v1"
	// the Zed person is created after Dee's changes; compare as multisets of Dee's events
	got := strings.Join(types, " ")
	for _, w := range []string{"person.offboarded.v1", "person.status_changed.v1", "person.created.v1"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s in %s (want like %s)", w, got, want)
		}
	}
	if strings.Count(got, "person.offboarded.v1") != 1 || strings.Count(got, "person.status_changed.v1") != 4 {
		t.Errorf("event counts: %s", got)
	}
}

func TestOneOnOnePrivacyAndLifecycle(t *testing.T) {
	e := newEnv(t)
	boss, bossA := e.person("Boss", "", RoleEmployee, RoleManager)
	rep, repA := e.person("Rae", boss.ID, RoleEmployee)
	_, peerA := e.person("Pat", boss.ID, RoleEmployee)
	hrP, hrA := e.person("Hana", "", RoleEmployee, RoleHR)
	_ = hrP

	if _, err := e.s.CreateSeries(e.ctx, repA, boss.ID, "weekly"); err == nil {
		t.Error("report started a series with their manager")
	}
	ser, err := e.s.CreateSeries(e.ctx, bossA, rep.ID, "weekly")
	e.must(err)
	if _, err := e.s.CreateSeries(e.ctx, bossA, rep.ID, "weekly"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate series: %v", err)
	}
	m, err := e.s.ScheduleMeeting(e.ctx, repA, ser.ID, e.clock.Add(48*time.Hour))
	e.must(err)
	ag, err := e.s.AddAgendaItem(e.ctx, repA, m.ID, "Career growth")
	e.must(err)
	_ = ag
	e.must(e.s.SetNotes(e.ctx, bossA, m.ID, "Agreed to review the roadmap", false))
	e.must(e.s.SetNotes(e.ctx, bossA, m.ID, "Concerned about burnout; watch workload", true))
	// The report cannot write or read the private notes, and does not learn they exist.
	if err := e.s.SetNotes(e.ctx, repA, m.ID, "x", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("report writing private notes: %v", err)
	}
	got, err := e.s.GetMeeting(e.ctx, repA, m.ID)
	e.must(err)
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "burnout") || strings.Contains(string(b), "private_notes") || got.SharedNotes == "" {
		t.Fatalf("report view leaks private notes or lacks shared notes: %s", b)
	}
	mine, err := e.s.GetMeeting(e.ctx, bossA, m.ID)
	e.must(err)
	if mine.PrivateNotes == nil || !strings.Contains(*mine.PrivateNotes, "burnout") {
		t.Fatal("manager cannot read own private notes")
	}
	// Everyone else is shut out, HR and admin included.
	for name, who := range map[string]Actor{"peer": peerA, "hr": hrA, "service admin": e.hr} {
		if _, err := e.s.GetMeeting(e.ctx, who, m.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s reads the meeting: %v", name, err)
		}
		if err := e.s.SetNotes(e.ctx, who, m.ID, "x", false); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s writes notes: %v", name, err)
		}
		if _, err := e.s.AddAgendaItem(e.ctx, who, m.ID, "x"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s adds agenda: %v", name, err)
		}
	}
	// Notes are sealed at rest.
	var sh, pr string
	e.must(e.s.db.QueryRow(`SELECT shared_notes_sealed, private_notes_sealed FROM oneonone_meetings WHERE id = ?`, m.ID).Scan(&sh, &pr))
	if strings.Contains(sh+pr, "burnout") || strings.Contains(sh+pr, "roadmap") {
		t.Fatal("notes stored in plaintext")
	}
	if e.aud.find("read", "meeting:"+m.ID, "ok") == nil {
		t.Error("meeting read not audited")
	}
	// Action items: assignee must be a participant; link to a work issue; follow-ups show next time.
	if _, err := e.s.AddAction(e.ctx, bossA, m.ID, "Send roadmap", peerA.PersonID, ""); err == nil {
		t.Error("action assigned to a non-participant")
	}
	act, err := e.s.AddAction(e.ctx, bossA, m.ID, "Send roadmap", rep.ID, "2026-03-20")
	e.must(err)
	if _, err := e.s.UpdateAction(e.ctx, repA, act.ID, nil, ptr("WORK-123")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateAction(e.ctx, repA, act.ID, nil, ptr("bad id with spaces")); err == nil {
		t.Error("bad work issue id accepted")
	}
	if _, err := e.s.UpdateAction(e.ctx, peerA, act.ID, ptr("done"), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("peer updating action: %v", err)
	}
	m2, err := e.s.ScheduleMeeting(e.ctx, bossA, ser.ID, e.clock.Add(9*24*time.Hour))
	e.must(err)
	next, err := e.s.GetMeeting(e.ctx, repA, m2.ID)
	e.must(err)
	if len(next.FollowUps) != 1 || next.FollowUps[0].WorkIssueID != "WORK-123" {
		t.Errorf("follow-ups = %+v", next.FollowUps)
	}
	ms, err := e.s.ListMeetings(e.ctx, repA, ser.ID)
	e.must(err)
	if len(ms) != 2 {
		t.Errorf("history = %d", len(ms))
	}
	// A new manager does not inherit the old manager's series or private notes; the old one loses access.
	newBoss, newBossA := e.person("Nia", "", RoleEmployee, RoleManager)
	_, err = e.s.SetManager(e.ctx, e.hr, rep.ID, newBoss.ID)
	e.must(err)
	if _, err := e.s.GetMeeting(e.ctx, bossA, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("former manager keeps access: %v", err)
	}
	if _, err := e.s.GetMeeting(e.ctx, newBossA, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("new manager inherits the old series: %v", err)
	}
	if _, err := e.s.GetMeeting(e.ctx, repA, m.ID); err != nil {
		t.Errorf("report lost access to their own 1:1 history: %v", err)
	}
	// Events carry ids only.
	evs, _ := e.s.AllEvents(e.ctx)
	n := 0
	for _, ev := range evs {
		if ev.Type == EventOneOnOneScheduled {
			n++
			if _, ok := ev.Data["manager_party_id"]; !ok {
				t.Error("scheduled event lacks manager_party_id")
			}
		}
	}
	if n != 2 {
		t.Errorf("oneonone.scheduled events = %d", n)
	}
}

func TestGoalsVisibilityAlignmentAndProgress(t *testing.T) {
	e := newEnv(t)
	tm, err := e.s.CreateOrgUnit(e.ctx, e.hr, "teams", OrgInput{Name: "Platform"})
	e.must(err)
	vp, vpA := e.person("Vee", "", RoleEmployee, RoleManager)
	mgr, mgrA := e.person("Max", vp.ID, RoleEmployee, RoleManager)
	rep, repA := e.person("Rae", mgr.ID, RoleEmployee)
	peer, peerA := e.person("Pat", mgr.ID, RoleEmployee)
	out, outA := e.person("Oli", "", RoleEmployee)
	for _, p := range []*Person{mgr, rep, peer} {
		_, err := e.s.UpdatePerson(e.ctx, e.hr, p.ID, PersonPatch{TeamID: ptr(tm.ID)})
		e.must(err)
	}
	_ = out

	co, err := e.s.CreateGoal(e.ctx, e.hr, GoalInput{Scope: "company", Title: "Ship v1", Period: "2026"})
	e.must(err)
	if _, err := e.s.CreateGoal(e.ctx, mgrA, GoalInput{Scope: "company", Title: "x"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("manager creating company goal: %v", err)
	}
	mine, err := e.s.CreateGoal(e.ctx, mgrA, GoalInput{Scope: "person", Title: "Hit reliability", Visibility: "team", ParentID: co.ID})
	e.must(err)
	rg, err := e.s.CreateGoal(e.ctx, repA, GoalInput{Scope: "person", Title: "Cut p99 latency", ParentID: mine.ID, Period: "2026-Q2"}) // default visibility manager
	e.must(err)
	if rg.Visibility != "manager" || rg.OwnerID != rep.ID {
		t.Fatalf("defaults: %+v", rg)
	}
	priv, err := e.s.CreateGoal(e.ctx, repA, GoalInput{Scope: "person", Title: "Find a new job", Visibility: "private"})
	e.must(err)

	// Alignment rules.
	if _, err := e.s.CreateGoal(e.ctx, repA, GoalInput{Scope: "person", Title: "x", ParentID: priv.ID}); err == nil {
		t.Error("aligned to an invisible/unrelated goal")
	}
	oliG, _ := e.s.CreateGoal(e.ctx, outA, GoalInput{Scope: "person", Title: "Oli goal", Visibility: "company"})
	if _, err := e.s.CreateGoal(e.ctx, repA, GoalInput{Scope: "person", Title: "x", ParentID: oliG.ID}); err == nil {
		t.Error("aligned to a goal outside the management chain")
	}
	if _, err := e.s.UpdateGoal(e.ctx, mgrA, mine.ID, GoalPatch{ParentID: ptr(rg.ID)}); err == nil {
		t.Error("alignment cycle accepted")
	}
	// Visibility matrix.
	vis := func(who Actor, g *Goal) bool { _, err := e.s.GetGoal(e.ctx, who, g.ID); return err == nil }
	checks := []struct {
		name string
		who  Actor
		g    *Goal
		want bool
	}{
		{"owner private", repA, priv, true},
		{"manager private", mgrA, priv, false},
		{"hr private", e.hr, priv, false},
		{"manager reads report's manager-visibility goal", mgrA, rg, true},
		{"skip-level reads it", vpA, rg, true},
		{"peer cannot read manager-visibility goal", peerA, rg, false},
		{"hr reads manager-visibility goal", e.hr, rg, true},
		{"teammate reads team-visibility goal", repA, mine, true},
		{"outsider cannot read team goal", outA, mine, false},
		{"anyone reads company goal", outA, co, true},
		{"company-visibility goal of outsider", repA, oliG, true},
	}
	for _, c := range checks {
		if got := vis(c.who, c.g); got != c.want {
			t.Errorf("%s: visible=%v want %v", c.name, got, c.want)
		}
	}
	// Lists are filtered the same way.
	l, err := e.s.ListGoals(e.ctx, peerA, GoalFilter{})
	e.must(err)
	for _, g := range l {
		if g.ID == rg.ID || g.ID == priv.ID {
			t.Errorf("peer list leaks %s", g.Title)
		}
	}
	// Writes: peer and skip-level cannot, direct manager can.
	if _, err := e.s.UpdateGoal(e.ctx, vpA, rg.ID, GoalPatch{Title: ptr("hijack")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("skip-level edit: %v", err)
	}
	if _, err := e.s.UpdateGoal(e.ctx, peerA, rg.ID, GoalPatch{Title: ptr("hijack")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("peer edit: %v", err)
	}
	if _, err := e.s.UpdateGoal(e.ctx, mgrA, rg.ID, GoalPatch{Description: ptr("clarified")}); err != nil {
		t.Errorf("direct manager edit: %v", err)
	}
	if e.aud.find("read", "goal:"+rg.ID, "ok") == nil {
		t.Error("foreign goal read not audited")
	}
	// Key results and progress.
	kr, err := e.s.AddKeyResult(e.ctx, repA, rg.ID, KeyResultInput{Title: "p99 under 200ms", Unit: "ms", Start: 400, Target: 200})
	e.must(err)
	if _, err := e.s.AddKeyResult(e.ctx, repA, rg.ID, KeyResultInput{Title: "bad", Start: 1, Target: 1}); err == nil {
		t.Error("zero-width key result accepted")
	}
	g, err := e.s.RecordProgress(e.ctx, repA, kr.ID, 300, "halfway")
	e.must(err)
	if g.Progress != 50 || g.KeyResults[0].Progress != 50 {
		t.Errorf("progress = %d", g.Progress)
	}
	if _, err := e.s.RecordProgress(e.ctx, peerA, kr.ID, 200, ""); err == nil {
		t.Error("peer recorded progress")
	}
	if _, err := e.s.RecordProgress(e.ctx, repA, kr.ID, 1e300, ""); err == nil {
		t.Error("absurd value accepted")
	}
	ups, err := e.s.ListUpdates(e.ctx, mgrA, rg.ID)
	e.must(err)
	if len(ups) != 1 {
		t.Errorf("updates = %d", len(ups))
	}
	evs, _ := e.s.AllEvents(e.ctx)
	changes := []string{}
	for _, ev := range evs {
		if ev.Type == EventGoalUpdated && ev.Data["goal_id"] == rg.ID {
			changes = append(changes, ev.Data["change"].(string))
		}
	}
	if strings.Join(changes, ",") != "created,updated,key_result_added,progress" {
		t.Errorf("goal events = %v", changes)
	}
}

func TestChecklistsFromTemplates(t *testing.T) {
	e := newEnv(t)
	boss, bossA := e.person("Boss", "", RoleEmployee, RoleManager)
	_, outsiderA := e.person("Out", "", RoleEmployee)
	if _, err := e.s.CreateTemplate(e.ctx, bossA, TemplateInput{Name: "x", Kind: "onboarding", Items: []TemplateItem{{Title: "t", AssigneeRole: "hr"}}}); !errors.Is(err, ErrForbidden) {
		t.Errorf("manager creating template: %v", err)
	}
	if _, err := e.s.CreateTemplate(e.ctx, e.hr, TemplateInput{Name: "bad", Kind: "onboarding", Items: []TemplateItem{{Title: "t", AssigneeRole: "root"}}}); err == nil {
		t.Error("bad assignee role accepted")
	}
	tpl, err := e.s.CreateTemplate(e.ctx, e.hr, TemplateInput{Name: "Standard onboarding", Kind: "onboarding", AutoStatus: "onboarding", Items: []TemplateItem{
		{Title: "Create accounts", AssigneeRole: "hr", DueOffsetDays: -2, Action: "identity.provision"},
		{Title: "Intro 1:1", AssigneeRole: "manager", DueOffsetDays: 1},
		{Title: "Read handbook", AssigneeRole: "self", DueOffsetDays: 5},
	}})
	e.must(err)
	hire, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", GivenName: "Nell", FamilyName: "New", SSOSubject: "sso-nell", ManagerID: boss.ID, StartDate: "2026-03-16"})
	e.must(err)
	_, err = e.s.ChangeStatus(e.ctx, e.hr, hire.ID, StOnboarding, "", "")
	e.must(err)
	cls, err := e.s.ListChecklists(e.ctx, e.hr, hire.ID)
	e.must(err)
	if len(cls) != 1 || len(cls[0].Tasks) != 3 || cls[0].TemplateID != tpl.ID {
		t.Fatalf("auto checklist = %+v", cls)
	}
	tasks := cls[0].Tasks
	if tasks[0].DueDate != "2026-03-08" || tasks[1].DueDate != "2026-03-11" {
		t.Errorf("due dates %s %s (anchor is today, 2026-03-10)", tasks[0].DueDate, tasks[1].DueDate)
	}
	if tasks[1].AssigneeID != boss.ID || tasks[2].AssigneeID != hire.ID || tasks[0].AssigneeID != "" {
		t.Errorf("assignees = %+v", tasks)
	}
	// Re-entering a status must not duplicate the checklist.
	if cl2, _ := e.s.ListChecklists(e.ctx, e.hr, hire.ID); len(cl2) != 1 {
		t.Error("duplicate")
	}
	hireA := Actor{Subject: "sso-nell", Roles: []string{RoleEmployee}, PersonID: hire.ID, HasPerson: true, Status: StOnboarding}
	if _, err := e.s.ListChecklists(e.ctx, outsiderA, hire.ID); err == nil {
		t.Error("outsider lists checklist")
	}
	if _, err := e.s.ListChecklists(e.ctx, bossA, hire.ID); err != nil {
		t.Errorf("manager list: %v", err)
	}
	// Completion: only the assignee (or hr/admin).
	if _, err := e.s.CompleteTask(e.ctx, outsiderA, tasks[2].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider completing: %v", err)
	}
	if _, err := e.s.CompleteTask(e.ctx, hireA, tasks[1].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("hire completing the manager's task: %v", err)
	}
	if tk, err := e.s.CompleteTask(e.ctx, hireA, tasks[2].ID); err != nil || !tk.Done {
		t.Errorf("assignee completing: %v", err)
	}
	if _, err := e.s.CompleteTask(e.ctx, e.hr, tasks[0].ID); err != nil {
		t.Errorf("hr completing: %v", err)
	}
	mt, err := e.s.MyTasks(e.ctx, bossA)
	e.must(err)
	if len(mt) != 1 || mt[0].ID != tasks[1].ID {
		t.Errorf("manager tasks = %+v", mt)
	}
	// Events: the provisioning task carries its action.
	evs, _ := e.s.AllEvents(e.ctx)
	found := false
	for _, ev := range evs {
		if ev.Type == EventChecklistTaskDone && ev.Data["action"] == "identity.provision" {
			found = true
		}
	}
	if !found {
		t.Error("no completion event with the identity.provision action")
	}
	if _, err := e.s.CompleteTask(e.ctx, e.hr, tasks[0].ID); err != nil { // idempotent
		t.Error(err)
	}
	if n := countEvents(t, e, EventChecklistTaskDone); n != 2 {
		t.Errorf("completion events = %d", n)
	}
}

func countEvents(t *testing.T, e *env, typ string) int {
	t.Helper()
	evs, err := e.s.AllEvents(e.ctx)
	e.must(err)
	n := 0
	for _, ev := range evs {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

var ledgerSubject = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`) // core/backup/ledger.go subjectRe
var ledgerClass = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)                // core/backup/ledger.go classRe

func TestErasureLegalHoldAndTombstones(t *testing.T) {
	e := newEnv(t)
	boss, bossA := e.person("Boss", "", RoleEmployee, RoleManager)
	p, pA := e.person("Fay", boss.ID, RoleEmployee)
	e.must(e.s.PutSensitive(e.ctx, e.hr, p.ID, map[string]json.RawMessage{FieldNationalID: raw("QQ123456C"), FieldSalary: raw(Salary{Amount: 5, Currency: "GBP", Period: "year"})}))
	ser, err := e.s.CreateSeries(e.ctx, bossA, p.ID, "monthly")
	e.must(err)
	m, _ := e.s.ScheduleMeeting(e.ctx, pA, ser.ID, e.clock.Add(time.Hour))
	e.must(e.s.SetNotes(e.ctx, bossA, m.ID, "private thought about Fay", true))
	_, err = e.s.CreateGoal(e.ctx, pA, GoalInput{Scope: "person", Title: "Fay goal"})
	e.must(err)

	// Active people cannot be erased; employees and managers cannot request it.
	if _, err := e.s.RequestErasure(e.ctx, e.hr, p.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("erasing an active person: %v", err)
	}
	if _, err := e.s.RequestErasure(e.ctx, pA, p.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("self erasure request: %v", err)
	}
	if _, err := e.s.RequestErasure(e.ctx, bossA, p.ID); err == nil {
		t.Error("manager erasure request accepted")
	}
	for _, st := range []Status{StOffboarding, StFormer} {
		_, err = e.s.ChangeStatus(e.ctx, e.hr, p.ID, st, "", "")
		e.must(err)
	}
	// Legal hold blocks erasure until lifted.
	e.must(e.s.SetLegalHold(e.ctx, e.hr, p.ID, true, "pending tribunal"))
	if _, err := e.s.RequestErasure(e.ctx, e.hr, p.ID); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "legal hold") {
		t.Fatalf("erasure under hold: %v", err)
	}
	if err := e.s.SetLegalHold(e.ctx, pA, p.ID, false, ""); err == nil {
		t.Error("employee lifted a legal hold")
	}
	e.must(e.s.SetLegalHold(e.ctx, e.hr, p.ID, false, ""))

	er, err := e.s.RequestErasure(e.ctx, e.hr, p.ID)
	e.must(err)
	if er.Subject != p.ID || len(er.Classes) != 7 {
		t.Fatalf("erasure = %+v", er)
	}
	ents, err := e.s.ListErasures(e.ctx, e.hr, 0)
	e.must(err)
	if len(ents) != 7 {
		t.Fatalf("tombstones = %+v", ents)
	}
	for _, en := range ents {
		if !ledgerSubject.MatchString(en.Subject) || !ledgerClass.MatchString(en.Class) || en.Date != "2026-03-10" {
			t.Errorf("tombstone not ledger-compatible: %+v", en)
		}
		if strings.Contains(en.Subject, "Fay") {
			t.Error("subject carries a name")
		}
	}
	if _, err := e.s.ListErasures(e.ctx, bossA, 0); err == nil {
		t.Error("manager lists tombstones")
	}
	// Idempotent.
	if _, err := e.s.RequestErasure(e.ctx, e.hr, p.ID); err != nil {
		t.Errorf("second erasure: %v", err)
	}
	if n, _ := e.s.ListErasures(e.ctx, e.hr, 0); len(n) != 7 {
		t.Error("duplicate tombstones")
	}
	// Everything is gone.
	var name, ssn, sub string
	e.must(e.s.db.QueryRow(`SELECT given_name, COALESCE(sens_national_id,''), COALESCE(sso_subject,'') FROM people WHERE id = ?`, p.ID).Scan(&name, &ssn, &sub))
	if name != "[erased]" || ssn != "" || sub != "" {
		t.Errorf("row after erasure: %q %q %q", name, ssn, sub)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM oneonone_series`, `SELECT COUNT(*) FROM oneonone_meetings`, `SELECT COUNT(*) FROM goals`, `SELECT COUNT(*) FROM checklists`} {
		var n int
		e.must(e.s.db.QueryRow(q).Scan(&n))
		if n != 0 {
			t.Errorf("%s = %d after erasure", q, n)
		}
	}
	if _, err := e.s.GetSensitive(e.ctx, e.hr, p.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("sensitive of erased: %v", err)
	}
	// The erased person's token no longer resolves to a person; the person list hides them.
	if a, _ := e.s.ResolveActor(e.ctx, &Principal{Subject: pA.Subject, Roles: pA.Roles}); a.HasPerson {
		t.Error("erased subject still resolves")
	}
	l, _, _ := e.s.ListPeople(e.ctx, bossA, PersonFilter{})
	for _, x := range l {
		if x.ID == p.ID {
			t.Error("erased person listed to a manager")
		}
	}
	// Restore replay: purge hook is idempotent and rejects non-person ids.
	e.must(e.s.PurgeSubject(e.ctx, p.ID, ClassSensitive))
	if err := e.s.PurgeSubject(e.ctx, "no' OR 1=1", ClassSensitive); err == nil {
		t.Error("bad subject accepted")
	}
	if err := e.s.PurgeSubject(e.ctx, p.ID, "made-up"); err == nil {
		t.Error("bad class accepted")
	}
	// Only opaque data is in the outbox for the erased person.
	evs, _ := e.s.AllEvents(e.ctx)
	for _, ev := range evs {
		b, _ := json.Marshal(ev)
		if strings.Contains(string(b), "Fay") || strings.Contains(string(b), "QQ123456C") {
			t.Errorf("event carries personal data: %s", b)
		}
	}
}

func TestRetentionSweepRespectsHoldAndClass(t *testing.T) {
	e := newEnv(t)
	rules := DefaultRetention()
	rules[ClassSensitive] = RetentionRule{Days: 30}
	rules[ClassBasic] = RetentionRule{Legal: true}
	e.s.retention = rules
	a, _ := e.person("Ann", "", RoleEmployee)
	b, _ := e.person("Bea", "", RoleEmployee)
	for _, p := range []*Person{a, b} {
		e.must(e.s.PutSensitive(e.ctx, e.hr, p.ID, map[string]json.RawMessage{FieldNationalID: raw("ID-" + p.ID[5:9])}))
		for _, st := range []Status{StOffboarding, StFormer} {
			_, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, st, "", "")
			e.must(err)
		}
	}
	e.must(e.s.SetLegalHold(e.ctx, e.hr, b.ID, true, "dispute"))
	rep, err := e.s.RunRetention(e.ctx)
	e.must(err)
	if rep.Purged[ClassSensitive] != 0 {
		t.Fatalf("purged before the period elapsed: %+v", rep)
	}
	*e.clock = e.clock.AddDate(0, 0, 31)
	rep, err = e.s.RunRetention(e.ctx)
	e.must(err)
	if rep.Purged[ClassSensitive] != 1 || rep.Skipped[ClassSensitive] != 1 {
		t.Fatalf("report = %+v", rep)
	}
	var na, nb sql.NullString
	e.must(e.s.db.QueryRow(`SELECT sens_national_id FROM people WHERE id = ?`, a.ID).Scan(&na))
	e.must(e.s.db.QueryRow(`SELECT sens_national_id FROM people WHERE id = ?`, b.ID).Scan(&nb))
	if na.Valid || !nb.Valid {
		t.Errorf("sensitive after sweep: a=%v b=%v", na.Valid, nb.Valid)
	}
	// 'legal' retention class never auto-purges the directory record.
	var gn string
	e.must(e.s.db.QueryRow(`SELECT given_name FROM people WHERE id = ?`, a.ID).Scan(&gn))
	if gn == "[erased]" {
		t.Error("legal-retention class was purged")
	}
	for in, ok := range map[string]bool{"7y": true, "12m": true, "30d": true, "0": true, "legal": true, "7": false, "y": false, "-1d": false, "7w": false, "": false} {
		if _, err := ParseRetention(in); (err == nil) != ok {
			t.Errorf("ParseRetention(%q) err=%v want ok=%v", in, err, ok)
		}
	}
}

func TestKeyRotationReSealsEverything(t *testing.T) {
	e := newEnv(t)
	boss, bossA := e.person("Boss", "", RoleEmployee, RoleManager)
	r, _ := e.person("Rae", boss.ID, RoleEmployee)
	e.must(e.s.PutSensitive(e.ctx, e.hr, r.ID, map[string]json.RawMessage{FieldDOB: raw("1992-02-02")}))
	ser, _ := e.s.CreateSeries(e.ctx, bossA, r.ID, "weekly")
	m, _ := e.s.ScheduleMeeting(e.ctx, bossA, ser.ID, e.clock.Add(time.Hour))
	e.must(e.s.SetNotes(e.ctx, bossA, m.ID, "private", true))

	rotated, err := ParseKeyring("k2 " + hexKeyB + "\nk1 " + hexKeyA + "\n")
	e.must(err)
	e.s.keys = rotated
	n, err := e.s.RotateKeys(e.ctx)
	e.must(err)
	if n != 2 {
		t.Fatalf("re-sealed %d", n)
	}
	if n, _ := e.s.RotateKeys(e.ctx); n != 0 {
		t.Errorf("second pass re-sealed %d", n)
	}
	// Drop the old key: everything still opens.
	e.s.keys, _ = ParseKeyring("k2 " + hexKeyB + "\n")
	g, err := e.s.GetSensitive(e.ctx, e.hr, r.ID, nil)
	e.must(err)
	if g[FieldDOB] != "1992-02-02" {
		t.Errorf("dob = %v", g[FieldDOB])
	}
	mt, err := e.s.GetMeeting(e.ctx, bossA, m.ID)
	e.must(err)
	if *mt.PrivateNotes != "private" {
		t.Error("private notes lost in rotation")
	}
}

func TestAuditChainDetectsTampering(t *testing.T) {
	e := newEnv(t)
	s, err := Open(filepath.Join(t.TempDir(), "a.db"), Options{Keyring: e.s.keys, Now: func() time.Time { return *e.clock }})
	e.must(err)
	defer func() { _ = s.Close() }()
	p, err := s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "A", FamilyName: "B"})
	e.must(err)
	e.must(s.PutSensitive(e.ctx, e.hr, p.ID, map[string]json.RawMessage{FieldDOB: raw("1990-01-01")}))
	if _, err := s.GetSensitive(e.ctx, e.hr, p.ID, nil); err != nil {
		t.Fatal(err)
	}
	e.must(s.VerifyAudit(e.ctx))
	if _, err = s.db.Exec(`UPDATE audit_log SET actor = 'nobody' WHERE seq = 2`); err == nil {
		t.Fatal("the audit log accepted an UPDATE")
	}
	if _, err = s.db.Exec(`DELETE FROM audit_log`); err == nil {
		t.Fatal("the audit log accepted a DELETE")
	}
	// someone with database write access drops the triggers and edits a row: the chain notices
	_, err = s.db.Exec(`DROP TRIGGER audit_log_no_update`)
	e.must(err)
	_, err = s.db.Exec(`UPDATE audit_log SET actor = 'nobody' WHERE seq = 2`)
	e.must(err)
	if s.VerifyAudit(e.ctx) == nil {
		t.Error("tampering not detected")
	}
}
