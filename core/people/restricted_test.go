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
	for _, act := range []Action{ActDirCreate, ActDirUpdate, ActDirOrg, ActDirStatus, ActTemplateManage} {
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
