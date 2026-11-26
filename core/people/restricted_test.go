package people

import "testing"

func restricted(roles []string, perms ...string) Actor {
	return Actor{Subject: "user:tok", Roles: roles, Perms: perms, Restricted: true}
}

// A personal access token carries permissions, not just a role: the hr role it maps to must not
// reach sensitive fields unless the token has the sensitive permissions.
func TestRestrictedTokenCannotReachSensitiveFieldsWithoutThePermission(t *testing.T) {
	hrNoSens := restricted([]string{RoleEmployee, RoleHR}, "people.read", "people.write")
	for _, f := range SensitiveFields {
		if FieldAccess(hrNoSens, false, f, Fact{}) || FieldAccess(hrNoSens, true, f, Fact{}) {
			t.Errorf("people.write alone reached %s", f)
		}
	}
	readOnly := restricted([]string{RoleEmployee, RoleHR}, "people.read", "people.write", "people.sensitive-read")
	if !FieldAccess(readOnly, false, FieldSalary, Fact{}) || FieldAccess(readOnly, true, FieldSalary, Fact{}) {
		t.Error("sensitive-read must read but not write")
	}
	full := restricted([]string{RoleEmployee, RoleHR}, "people.read", "people.write", "people.sensitive-read", "people.sensitive-write")
	if !FieldAccess(full, true, FieldSalary, Fact{}) {
		t.Error("sensitive-write must write")
	}
	// the unrestricted service account (static token) is unchanged
	svc := Actor{Subject: "svc", Roles: []string{RoleHR, RoleAdmin}}
	if !FieldAccess(svc, false, FieldDOB, Fact{}) {
		t.Error("the service account lost sensitive access")
	}
}

func TestRestrictedAdminRoleNeedsTheAdminPermissionForAdminActions(t *testing.T) {
	adminOnly := restricted([]string{RoleEmployee, RoleAdmin}, "people.read", "people.admin")
	for _, act := range []Action{ActErasure, ActLegalHold, ActRetention} {
		if !Decide(adminOnly, act, Fact{}) {
			t.Errorf("people.admin must allow %s", act)
		}
	}
	for _, act := range []Action{ActDirCreate, ActDirUpdate, ActDirImport, ActDirOrg, ActDirStatus, ActTemplateManage} {
		if Decide(adminOnly, act, Fact{}) {
			t.Errorf("people.admin without people.write allowed %s", act)
		}
	}
	if FieldAccess(adminOnly, false, FieldNationalID, Fact{}) {
		t.Error("people.admin alone read a sensitive field")
	}
	hr := restricted([]string{RoleEmployee, RoleHR}, "people.read", "people.write")
	if Decide(hr, ActErasure, Fact{}) || Decide(hr, ActLegalHold, Fact{}) {
		t.Error("people.write alone reached erasure or legal hold")
	}
	if !Decide(hr, ActDirCreate, Fact{}) {
		t.Error("people.write must create people")
	}
}

func TestResolveActorCarriesTheTokenPermissions(t *testing.T) {
	e := newEnv(t)
	a, err := e.s.ResolveActor(e.ctx, &Principal{Subject: "user:x", Roles: []string{RoleHR}, Perms: []string{"people.read"}})
	if err != nil || !a.Restricted || len(a.Perms) != 1 {
		t.Fatalf("%+v %v", a, err)
	}
	a, _ = e.s.ResolveActor(e.ctx, &Principal{Subject: "svc", Roles: []string{RoleHR}})
	if a.Restricted {
		t.Fatal("a static service token must not be restricted")
	}
}

func TestRestrictedPerformanceNeedsPerformancePermissions(t *testing.T) {
	m := restricted([]string{RoleEmployee, RoleManager}, "people.read")
	m.HasPerson, m.Status = true, StActive
	f := Fact{Direct: true, Chain: true, Scope: "person", Visibility: "manager"}
	if Decide(m, ActOneOnOneRead, f) || Decide(m, ActGoalRead, f) || Decide(m, ActGoalWrite, f) {
		t.Error("performance data without a performance permission")
	}
	m.Perms = []string{"people.read", "performance.read"}
	if !Decide(m, ActOneOnOneRead, f) || Decide(m, ActGoalWrite, f) {
		t.Error("performance.read must read, not write")
	}
	m.Perms = []string{"people.read", "performance.read", "performance.write"}
	if !Decide(m, ActGoalWrite, f) {
		t.Error("performance.write must write")
	}
}

// The hr and admin paths of checklist creation and task completion are writes of other people's
// records: a restricted token needs people.write for them. The self-service paths (an assigned
// employee completing their own task, a manager starting a checklist for a report) stay open to
// the manifest roles that have no people.write.
func TestRestrictedChecklistWritesNeedPeopleWriteOnTheHRPath(t *testing.T) {
	cases := []struct {
		name  string
		actor Actor
		act   Action
		fact  Fact
		want  bool
	}{
		{"hr with people.write creates", restricted([]string{RoleEmployee, RoleHR}, "people.read", "people.write"), ActChecklistCreate, Fact{}, true},
		{"admin without people.write cannot create", restricted([]string{RoleEmployee, RoleAdmin}, "people.read", "people.admin"), ActChecklistCreate, Fact{}, false},
		{"admin without people.write cannot complete someone's task", restricted([]string{RoleEmployee, RoleAdmin}, "people.read", "people.admin"), ActTaskComplete, Fact{}, false},
		{"hr with people.write completes", restricted([]string{RoleEmployee, RoleHR}, "people.read", "people.write"), ActTaskComplete, Fact{}, true},
		{"employee completes their own assigned task", restricted([]string{RoleEmployee}, "people.read"), ActTaskComplete, Fact{Assigned: true}, true},
		{"employee cannot complete an unassigned task", restricted([]string{RoleEmployee}, "people.read"), ActTaskComplete, Fact{}, false},
		{"unrestricted service account unchanged", Actor{Subject: "svc", Roles: []string{RoleHR}}, ActChecklistCreate, Fact{}, true},
	}
	for _, c := range cases {
		c.actor.HasPerson, c.actor.Status = true, StActive
		if c.name == "unrestricted service account unchanged" {
			c.actor.HasPerson = false
		}
		if got := Decide(c.actor, c.act, c.fact); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
	mgr := restricted([]string{RoleEmployee, RoleManager}, "people.read", "performance.read", "performance.write")
	mgr.HasPerson, mgr.Status = true, StActive
	if !Decide(mgr, ActChecklistCreate, Fact{Direct: true}) {
		t.Error("a manager lost checklist creation for a direct report")
	}
}

// Self-service writes (own preferred_name/work_phone) are deliberately not gated by people.write:
// the employee role in modules/people/module.yaml does not carry it, so gating would lock every
// employee out of their own entry. A token with people.read still maps to an employee.
func TestSelfServiceWriteIsNotGatedByPeopleWrite(t *testing.T) {
	emp := restricted([]string{RoleEmployee}, "people.read")
	emp.HasPerson, emp.Status = true, StActive
	if !Decide(emp, ActDirUpdateSelf, Fact{Self: true}) {
		t.Error("an employee token cannot edit their own entry")
	}
}
