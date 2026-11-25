package people

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F1: erased data must not survive in the main file or the write-ahead log.
func TestErasureLeavesNothingInDatabaseFileOrWAL(t *testing.T) {
	e := newEnv(t)
	boss, _ := e.person("Boss", "", RoleEmployee, RoleManager)
	p, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "Zeldaquux", FamilyName: "Fakenamington", WorkEmail: "zeldaquux@example.com", WorkPhone: "+1 555 0142", ManagerID: boss.ID, SSOSubject: "sso-zeldaquux"})
	e.must(err)
	e.must(e.s.PutSensitive(e.ctx, e.hr, p.ID, map[string]json.RawMessage{FieldAddress: raw("77 Quuxville Lane")}))
	_, err = e.s.ChangeStatus(e.ctx, e.hr, p.ID, StOffboarding, "", "resigned citing burnoutxyzzy")
	e.must(err)
	_, err = e.s.ChangeStatus(e.ctx, e.hr, p.ID, StFormer, "", "")
	e.must(err)

	needles := []string{"Zeldaquux", "Fakenamington", "zeldaquux@example.com", "+1 555 0142", "sso-zeldaquux"}
	scan := func() (hits []string) {
		for _, f := range []string{e.path, e.path + "-wal"} {
			b, err := os.ReadFile(f)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			e.must(err)
			for _, n := range needles {
				if bytes.Contains(b, []byte(n)) {
					hits = append(hits, filepath.Base(f)+": "+n)
				}
			}
		}
		return hits
	}
	if len(scan()) == 0 {
		t.Fatal("test is not meaningful: the personal data is not in the files before erasure")
	}
	_, err = e.s.RequestErasure(e.ctx, e.hr, p.ID)
	e.must(err)
	if hits := scan(); len(hits) != 0 {
		t.Fatalf("erased data still in the database files: %v", hits)
	}
	if st, err := os.Stat(e.path + "-wal"); err == nil && st.Size() != 0 {
		t.Errorf("the WAL was not truncated (%d bytes)", st.Size())
	}
	if n := countEvents(t, e, EventPersonErased); n != 1 {
		t.Errorf("person.erased events = %d", n)
	}
	if _, err := e.s.RequestErasure(e.ctx, e.hr, p.ID); err != nil || countEvents(t, e, EventPersonErased) != 1 {
		t.Error("a repeated erasure must not announce again")
	}
}

func TestRetentionSweepScrubsTheWAL(t *testing.T) {
	e := newEnv(t)
	e.s.retention = map[string]RetentionRule{ClassBasic: {Days: 1}, ClassSensitive: {Days: 1}, ClassChecklists: {Legal: true}, ClassOneOnOne: {Legal: true}, ClassGoals: {Legal: true}}
	p, _ := e.person("Retainquux", "", RoleEmployee)
	for _, st := range []Status{StOffboarding, StFormer} {
		_, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, st, "", "")
		e.must(err)
	}
	*e.clock = e.clock.AddDate(0, 0, 3)
	rep, err := e.s.RunRetention(e.ctx)
	e.must(err)
	if rep.Purged[ClassBasic] != 1 {
		t.Fatalf("report %+v", rep)
	}
	b, _ := os.ReadFile(e.path + "-wal")
	if bytes.Contains(b, []byte("Retainquux")) {
		t.Error("purged data left in the WAL")
	}
}

// F2: private goals publish nothing.
func TestPrivateGoalsEmitNoEvents(t *testing.T) {
	e := newEnv(t)
	_, rA := e.person("Rae", "", RoleEmployee)
	g, err := e.s.CreateGoal(e.ctx, rA, GoalInput{Scope: "person", Title: "secret", Visibility: "private"})
	e.must(err)
	kr, err := e.s.AddKeyResult(e.ctx, rA, g.ID, KeyResultInput{Title: "k", Start: 0, Target: 1})
	e.must(err)
	_, err = e.s.RecordProgress(e.ctx, rA, kr.ID, 1, "")
	e.must(err)
	_, err = e.s.UpdateGoal(e.ctx, rA, g.ID, GoalPatch{Status: ptr("achieved")})
	e.must(err)
	if n := countEvents(t, e, EventGoalUpdated); n != 0 {
		t.Fatalf("%d goal events for a private goal", n)
	}
	// Going private stops events; going visible resumes them.
	pub, err := e.s.CreateGoal(e.ctx, rA, GoalInput{Scope: "person", Title: "open", Visibility: "company"})
	e.must(err)
	before := countEvents(t, e, EventGoalUpdated)
	_, err = e.s.UpdateGoal(e.ctx, rA, pub.ID, GoalPatch{Visibility: ptr("private")})
	e.must(err)
	if countEvents(t, e, EventGoalUpdated) != before {
		t.Error("the change to private published an event")
	}
}

// F3: no existence oracle through goal creation or lookups.
func TestNoExistenceOracle(t *testing.T) {
	e := newEnv(t)
	_, aA := e.person("Ann", "", RoleEmployee)
	b, _ := e.person("Ben", "", RoleEmployee)
	for name, owner := range map[string]string{"exists but not visible/allowed": b.ID, "does not exist": "pers_0123456789abcdef01234567"} {
		_, err := e.s.CreateGoal(e.ctx, aA, GoalInput{Scope: "person", OwnerID: owner, Title: "x"})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
	cand, _ := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", GivenName: "Cara", FamilyName: "C"})
	for _, id := range []string{cand.ID, "pers_0123456789abcdef01234567", "garbage"} {
		if _, err := e.s.GetPerson(e.ctx, aA, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetPerson(%s) = %v", id, err)
		}
	}
}

// F4: rotation never overwrites a concurrent write; the key loader refuses symlinks.
func TestRotateKeysCompareAndSwap(t *testing.T) {
	e := newEnv(t)
	p, _ := e.person("Rae", "", RoleEmployee)
	e.must(e.s.PutSensitive(e.ctx, e.hr, p.ID, map[string]json.RawMessage{FieldDOB: raw("1990-01-01")}))
	rotated, _ := ParseKeyring("k2 " + hexKeyB + "\nk1 " + hexKeyA + "\n")
	e.s.keys = rotated
	once := true
	e.s.rotateHook = func() {
		if once { // a user changes the value after rotation read it
			once = false
			e.must(e.s.PutSensitive(e.ctx, e.hr, p.ID, map[string]json.RawMessage{FieldDOB: raw("1991-02-03")}))
		}
	}
	if _, err := e.s.RotateKeys(e.ctx); err != nil {
		t.Fatal(err)
	}
	g, err := e.s.GetSensitive(e.ctx, e.hr, p.ID, []string{FieldDOB})
	e.must(err)
	if g[FieldDOB] != "1991-02-03" {
		t.Fatalf("rotation overwrote a newer value with a stale one: %v", g[FieldDOB])
	}
}

func TestKeyringLoaderRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	e := os.WriteFile(real, []byte("k1 "+hexKeyA+"\n"), 0o600)
	if e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here")
	}
	if _, err := LoadKeyring(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked keyring accepted: %v", err)
	}
	if _, err := LoadKeyring(real); err != nil {
		t.Errorf("own regular file refused: %v", err)
	}
	if _, err := LoadKeyring(dir); err == nil {
		t.Error("a directory was accepted as a keyring")
	}
}

// F5: manager tasks follow the live reporting line; identity.* actions need another person.
func TestManagerTasksFollowTheLiveManager(t *testing.T) {
	e := newEnv(t)
	oldM, oldA := e.person("Old", "", RoleEmployee, RoleManager)
	newM, newA := e.person("Nia", "", RoleEmployee, RoleManager)
	tpl, err := e.s.CreateTemplate(e.ctx, e.hr, TemplateInput{Name: "t", Kind: "onboarding", Items: []TemplateItem{{Title: "welcome", AssigneeRole: "manager"}}})
	e.must(err)
	hire, err := e.s.CreatePerson(e.ctx, e.hr, PersonInput{Kind: "employee", Status: StActive, GivenName: "Hy", FamilyName: "Re", ManagerID: oldM.ID})
	e.must(err)
	cl, err := e.s.CreateChecklist(e.ctx, e.hr, hire.ID, tpl.ID, "")
	e.must(err)
	task := cl.Tasks[0]
	_, err = e.s.SetManager(e.ctx, e.hr, hire.ID, newM.ID)
	e.must(err)
	if mt, _ := e.s.MyTasks(e.ctx, oldA); len(mt) != 0 {
		t.Error("the former manager still has the task")
	}
	if mt, _ := e.s.MyTasks(e.ctx, newA); len(mt) != 1 {
		t.Error("the new manager did not inherit the task")
	}
	if _, err := e.s.CompleteTask(e.ctx, oldA, task.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("former manager completing: %v", err)
	}
	if _, err := e.s.CompleteTask(e.ctx, newA, task.ID); err != nil {
		t.Errorf("new manager completing: %v", err)
	}
}

func TestIdentityActionsCannotBeSelfServed(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.CreateTemplate(e.ctx, e.hr, TemplateInput{Name: "bad", Kind: "onboarding", Items: []TemplateItem{{Title: "grant myself", AssigneeRole: "self", Action: "identity.provision"}}}); err == nil {
		t.Error("identity action assignable to self")
	}
	tpl, err := e.s.CreateTemplate(e.ctx, e.hr, TemplateInput{Name: "ok", Kind: "onboarding", Items: []TemplateItem{{Title: "accounts", AssigneeRole: "hr", Action: "identity.provision"}}})
	e.must(err)
	hrP, hrA := e.person("Hana", "", RoleEmployee, RoleHR)
	cl, err := e.s.CreateChecklist(e.ctx, e.hr, hrP.ID, tpl.ID, "")
	e.must(err)
	if _, err := e.s.CompleteTask(e.ctx, hrA, cl.Tasks[0].ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("an hr person completed the provisioning task about themself: %v", err)
	}
	if _, err := e.s.CompleteTask(e.ctx, e.hr, cl.Tasks[0].ID); err != nil {
		t.Errorf("another admin completing: %v", err)
	}
}

// F6: notes sealed, leave masked, history reads audited.
func TestStatusNotesLeaveMaskingAndHistoryAudit(t *testing.T) {
	e := newEnv(t)
	boss, bossA := e.person("Boss", "", RoleEmployee, RoleManager)
	p, pA := e.person("Rae", boss.ID, RoleEmployee)
	_, peerA := e.person("Pat", "", RoleEmployee)
	_, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StLeave, "", "medical appointment series")
	e.must(err)
	var raw string
	e.must(e.s.db.QueryRow(`SELECT note FROM status_history WHERE person_id = ? AND to_status = 'leave'`, p.ID).Scan(&raw))
	if strings.Contains(raw, "medical") || !strings.HasPrefix(raw, "v1.") {
		t.Fatalf("note stored as %q", raw)
	}
	h, err := e.s.History(e.ctx, e.hr, p.ID)
	e.must(err)
	if last := h[len(h)-1]; last.Note != "medical appointment series" {
		t.Errorf("hr sees note %q", last.Note)
	}
	if e.aud.find("read", "history:"+p.ID, "ok") == nil {
		t.Error("history read not audited")
	}
	e.aud.fail = true
	if _, err := e.s.History(e.ctx, pA, p.ID); err == nil {
		t.Error("history returned while the audit hook is down")
	}
	e.aud.fail = false

	status := func(who Actor) Status {
		g, err := e.s.GetPerson(e.ctx, who, p.ID)
		e.must(err)
		return g.Status
	}
	if status(peerA) != StActive {
		t.Error("a colleague can see that someone is on leave")
	}
	for name, who := range map[string]Actor{"self": pA, "manager": bossA, "hr": e.hr} {
		if status(who) != StLeave {
			t.Errorf("%s does not see the real status", name)
		}
	}
	l, _, _ := e.s.ListPeople(e.ctx, peerA, PersonFilter{Status: StLeave})
	if len(l) != 0 {
		t.Error("status=leave filter reveals who is on leave")
	}
	l, _, _ = e.s.ListPeople(e.ctx, peerA, PersonFilter{})
	for _, x := range l {
		if x.Status == StLeave {
			t.Error("list reveals leave")
		}
	}
	if l, _, _ = e.s.ListPeople(e.ctx, e.hr, PersonFilter{Status: StLeave}); len(l) != 1 {
		t.Errorf("hr filter = %d", len(l))
	}
	// rotation keeps notes readable
	rot, _ := ParseKeyring("k2 " + hexKeyB + "\nk1 " + hexKeyA + "\n")
	e.s.keys = rot
	if n, err := e.s.RotateKeys(e.ctx); err != nil || n < 1 {
		t.Fatalf("rotate: %d %v", n, err)
	}
	e.s.keys, _ = ParseKeyring("k2 " + hexKeyB + "\n")
	if h, _ := e.s.History(e.ctx, e.hr, p.ID); h[len(h)-1].Note != "medical appointment series" {
		t.Error("note lost in rotation")
	}
}

// F7
func TestRateLimitsDenialAuditCapAndGoalListing(t *testing.T) {
	e := newAPIEnv(t)
	for i := 0; i < 40; i++ {
		e.do("bad-token", "GET", "/v1/people", nil)
	}
	if code, _, _ := e.do("bad-token", "GET", "/v1/people", nil); code != 429 {
		t.Errorf("credential guessing is not throttled: %d", code)
	}
	if code, _, _ := e.do(hrTok, "GET", "/v1/people", nil); code != 429 {
		t.Errorf("a throttled address can still call: %d", code) // same IP in tests
	}

	e2 := newEnv(t)
	p, _ := e2.person("Ann", "", RoleEmployee)
	_, peerA := e2.person("Pat", "", RoleEmployee)
	for i := 0; i < 100; i++ {
		_, _ = e2.s.GetSensitive(e2.ctx, peerA, p.ID, []string{FieldSalary})
	}
	denied := 0
	throttled := 0
	for _, ev := range e2.aud.evs {
		if ev.Outcome == "denied" && ev.Actor == peerA.PersonID {
			denied++
			if ev.Action == "deny-throttled" {
				throttled++
			}
		}
	}
	if denied > denyAuditPerMinute+1 || throttled != 1 {
		t.Errorf("denial audit records = %d (throttle notices %d)", denied, throttled)
	}
	*e2.clock = e2.clock.Add(2 * time.Minute)
	n := len(e2.aud.evs)
	_, _ = e2.s.GetSensitive(e2.ctx, peerA, p.ID, []string{FieldSalary})
	if len(e2.aud.evs) != n+1 {
		t.Error("denial auditing did not resume in the next window")
	}

	// invisible goals must not crowd visible ones out of the list
	for i := 0; i < 520; i++ {
		_, err := e2.s.db.Exec(`INSERT INTO goals (id, scope, owner_id, title, visibility, status, created_by, created_at, updated_at) VALUES (?, 'person', ?, 'hidden', 'private', 'active', 'x', 1, 1)`, newID("goal"), p.ID)
		e2.must(err)
	}
	co, err := e2.s.CreateGoal(e2.ctx, e2.hr, GoalInput{Scope: "company", Title: "Visible"})
	e2.must(err)
	l, err := e2.s.ListGoals(e2.ctx, peerA, GoalFilter{})
	e2.must(err)
	found := false
	for _, g := range l {
		found = found || g.ID == co.ID
	}
	if !found {
		t.Error("a visible goal was crowded out by invisible ones")
	}
}

func TestIntrospectNegativeCacheLRUAndTransport(t *testing.T) {
	calls := 0
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(401)
	}))
	defer idp.Close()
	now := time.Now()
	a := &IntrospectAuthenticator{URL: idp.URL, Now: func() time.Time { return now }}
	req := func(tok string) *http.Request {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return r
	}
	for i := 0; i < 5; i++ {
		if _, err := a.Authenticate(req("bad")); err == nil {
			t.Fatal("accepted")
		}
	}
	if calls != 1 {
		t.Errorf("negative answer not cached: %d calls", calls)
	}
	now = now.Add(10 * time.Second)
	_, _ = a.Authenticate(req("bad"))
	if calls != 2 {
		t.Errorf("negative cache did not expire: %d", calls)
	}
	// bounded
	for i := 0; i < maxIntrospectCache+50; i++ {
		_, _ = a.Authenticate(req("t" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('A'+(i/26)%26)) + string(rune('0'+i/676))))
	}
	if a.cache.ll.Len() > maxIntrospectCache {
		t.Errorf("cache grew to %d", a.cache.ll.Len())
	}

	for url, ok := range map[string]bool{"https://core.example/introspect": true, "http://127.0.0.1:9/i": true, "http://localhost/i": true, "http://core.can-system.svc/i": false} {
		_, err := ParseConfig([]byte("keyring_file: /k\nintrospect:\n  url: " + url + "\n"))
		if (err == nil) != ok {
			t.Errorf("%s: err=%v want ok=%v", url, err, ok)
		}
	}
	if _, err := ParseConfig([]byte("keyring_file: /k\nintrospect:\n  url: http://core.can-system.svc/i\n  allow_insecure_http: true\n")); err != nil {
		t.Errorf("explicit opt-in refused: %v", err)
	}
}

// F9: access ends on the effective date even if the hourly tick has not run.
func TestPendingLeaveEndsAccessOnTheEffectiveDate(t *testing.T) {
	e := newEnv(t)
	p, pA := e.person("Rae", "", RoleEmployee)
	_, err := e.s.ChangeStatus(e.ctx, e.hr, p.ID, StOffboarding, "", "")
	e.must(err)
	_, err = e.s.ChangeStatus(e.ctx, e.hr, p.ID, StFormer, "2026-03-12", "")
	e.must(err)
	a, _ := e.s.ResolveActor(e.ctx, &Principal{Subject: pA.Subject, Roles: pA.Roles})
	if a.Status != StOffboarding {
		t.Fatalf("status before the date = %s", a.Status)
	}
	*e.clock = e.clock.AddDate(0, 0, 3)
	a, err = e.s.ResolveActor(e.ctx, &Principal{Subject: pA.Subject, Roles: pA.Roles}) // no ApplyDue call
	e.must(err)
	if a.Status != StFormer {
		t.Fatalf("status after the date = %s: a leaver keeps access until the tick", a.Status)
	}
	if _, err := e.s.GetPerson(e.ctx, a, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("former person still reads: %v", err)
	}
}

// F11: tombstone residue is scrubbed; erasing a manager deletes their series.
func TestErasureScrubsAttributionAndManagerSeries(t *testing.T) {
	e := newEnv(t)
	boss, bossA := e.person("Boss", "", RoleEmployee, RoleManager)
	r, _ := e.person("Rae", boss.ID, RoleEmployee)
	tpl, _ := e.s.CreateTemplate(e.ctx, e.hr, TemplateInput{Name: "t", Kind: "onboarding", Items: []TemplateItem{{Title: "x", AssigneeRole: "manager"}}})
	cl, err := e.s.CreateChecklist(e.ctx, bossA, r.ID, tpl.ID, "")
	e.must(err)
	_, err = e.s.CompleteTask(e.ctx, bossA, cl.Tasks[0].ID)
	e.must(err)
	ser, _ := e.s.CreateSeries(e.ctx, bossA, r.ID, "weekly")
	m, _ := e.s.ScheduleMeeting(e.ctx, bossA, ser.ID, e.clock.Add(time.Hour))
	_, err = e.s.AddAgendaItem(e.ctx, bossA, m.ID, "topic")
	e.must(err)
	for _, st := range []Status{StOffboarding, StFormer} {
		_, err = e.s.ChangeStatus(e.ctx, e.hr, boss.ID, st, "", "")
		e.must(err)
	}
	_, err = e.s.RequestErasure(e.ctx, e.hr, boss.ID)
	e.must(err)
	for _, q := range []string{
		`SELECT COUNT(*) FROM checklists WHERE created_by = '` + boss.ID + `'`,
		`SELECT COUNT(*) FROM checklist_tasks WHERE done_by = '` + boss.ID + `'`,
		`SELECT COUNT(*) FROM status_history WHERE recorded_by = '` + boss.ID + `'`,
		`SELECT COUNT(*) FROM oneonone_series`,
		`SELECT COUNT(*) FROM agenda_items`,
	} {
		var n int
		e.must(e.s.db.QueryRow(q).Scan(&n))
		if n != 0 {
			t.Errorf("%s = %d after erasing the manager", q, n)
		}
	}
	// the other participant's series history went with it (documented)
	if sl, _ := e.s.ListSeries(e.ctx, Actor{Subject: "sso-rae", Roles: []string{RoleEmployee}, PersonID: r.ID, HasPerson: true, Status: StActive}); len(sl) != 0 {
		t.Error("series survived")
	}
}

// F12: alignment cycle detection is not limited to 20 levels.
func TestAlignmentCycleDetectedBeyondTwentyLevels(t *testing.T) {
	e := newEnv(t)
	p, pA := e.person("Rae", "", RoleEmployee)
	var ids []string
	for i := 0; i < 40; i++ {
		id := newID("goal")
		parent := any(nil)
		if i > 0 {
			parent = ids[i-1]
		}
		_, err := e.s.db.Exec(`INSERT INTO goals (id, scope, owner_id, title, visibility, status, parent_id, created_by, created_at, updated_at) VALUES (?, 'person', ?, 'g', 'company', 'active', ?, 'x', 1, 1)`, id, p.ID, parent)
		e.must(err)
		ids = append(ids, id)
	}
	_, err := e.s.UpdateGoal(e.ctx, pA, ids[0], GoalPatch{ParentID: ptr(ids[39])})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("a 40-level alignment cycle was not detected: %v", err)
	}
}

func TestAPIDoesNotLeakInternalsOnLargeInput(t *testing.T) {
	e := newAPIEnv(t)
	if code, _, b := e.do(hrTok, "POST", "/v1/people", map[string]any{"kind": "employee", "given_name": strings.Repeat("a", 5000), "family_name": "B"}); code != 400 || strings.Contains(b, "sqlite") {
		t.Errorf("%d %s", code, b)
	}
}
