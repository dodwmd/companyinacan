package people

import (
	"fmt"
	"testing"
)

var allActions = []Action{
	ActDirRead, ActDirCreate, ActDirUpdate, ActDirUpdateSelf, ActDirOrg, ActDirStatus, ActDirHistory,
	ActLegalHold, ActErasure, ActRetention, ActTemplateManage, ActChecklistCreate, ActChecklistRead,
	ActTaskComplete, ActTaskListOwn, ActOneOnOneCreate, ActOneOnOneRead, ActOneOnOneWrite,
	ActOneOnOnePrivate, ActGoalCreate, ActGoalRead, ActGoalWrite, ActGoalListOwn,
}

var allRoles = []string{RoleEmployee, RoleManager, RoleHR, RoleAdmin}
var allStatuses = []Status{StCandidate, StOnboarding, StActive, StLeave, StOffboarding, StFormer, ""}

// every subset of the four roles, with and without a person record, in every status.
func allActors() []Actor {
	var out []Actor
	for mask := 0; mask < 16; mask++ {
		var roles []string
		for i, r := range allRoles {
			if mask&(1<<i) != 0 {
				roles = append(roles, r)
			}
		}
		out = append(out, Actor{Subject: "svc", Roles: roles})
		for _, st := range allStatuses {
			out = append(out, Actor{Subject: "u", Roles: roles, PersonID: "pers_a", HasPerson: true, Status: st})
		}
	}
	return out
}

func allFacts() []Fact {
	var out []Fact
	bools := []bool{false, true}
	for _, self := range bools {
		for _, direct := range bools {
			for _, chain := range bools {
				for _, team := range bools {
					for _, lead := range bools {
						for _, asg := range bools {
							for _, st := range allStatuses {
								for _, scope := range []string{"", "person", "team", "company"} {
									for _, vis := range []string{"", "private", "manager", "team", "company"} {
										out = append(out, Fact{Self: self, Direct: direct, Chain: chain || direct, SameTeam: team, TeamLead: lead, Assigned: asg, Status: st, Scope: scope, Visibility: vis})
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return out
}

func describe(a Actor, f Fact) string {
	return fmt.Sprintf("actor%+v fact%+v", a, f)
}

// Invariants that must hold for every combination of role, person status and relationship.
func TestPolicyInvariantsExhaustive(t *testing.T) {
	actors, facts := allActors(), allFacts()
	n := 0
	for _, a := range actors {
		for _, f := range facts {
			for _, act := range allActions {
				n++
				got := Decide(a, act, f)
				// 1. Fail closed: no recognised role, or a candidate/former/unknown person, gets nothing.
				if a.blocked() && got {
					t.Fatalf("%s allowed for blocked %s", act, describe(a, f))
				}
				// 2. Elevated roles need an active person (or a service account for hr/admin).
				if got && a.HasPerson && a.Status != StActive {
					switch act {
					case ActDirRead, ActDirHistory, ActChecklistRead, ActTaskComplete, ActTaskListOwn, ActDirUpdateSelf,
						ActOneOnOneRead, ActOneOnOneWrite, ActGoalRead, ActGoalWrite, ActGoalCreate, ActGoalListOwn:
						// self-service and staff-level actions stay available to onboarding/leave/offboarding staff
					default:
						t.Fatalf("%s allowed for non-active person %s", act, describe(a, f))
					}
				}
				// 3. Skip-level and manager roles never get private 1:1 notes without being the direct manager.
				if act == ActOneOnOnePrivate && got && (!f.Direct || !a.has(RoleManager) || a.Status != StActive) {
					t.Fatalf("private notes allowed: %s", describe(a, f))
				}
				// 4. Private goals are visible to their owner only.
				if act == ActGoalRead && f.Visibility == "private" && got && (!f.Self || f.Scope != "person") {
					t.Fatalf("private goal readable by non-owner: %s", describe(a, f))
				}
				// 5. A manager role without a relationship to the subject gets no manager powers.
				if (act == ActOneOnOneCreate || act == ActOneOnOnePrivate) && !f.Direct && got {
					t.Fatalf("%s without direct relationship: %s", act, describe(a, f))
				}
				// 6. HR/admin-only actions are denied to everyone else.
				switch act {
				case ActDirCreate, ActDirUpdate, ActDirOrg, ActDirStatus, ActLegalHold, ActErasure, ActRetention, ActTemplateManage:
					if got && !a.has(RoleHR) && !a.has(RoleAdmin) {
						t.Fatalf("%s allowed without hr/admin: %s", act, describe(a, f))
					}
				}
				// 7. HR and admin cannot read 1:1 content or private goals by role alone.
				if (act == ActOneOnOneRead || act == ActOneOnOneWrite || act == ActOneOnOneCreate) && got && !f.Self && !f.Direct {
					t.Fatalf("1:1 access without participation: %s", describe(a, f))
				}
			}
		}
	}
	for _, act := range []Action{"", "dir.delete", "root", "dir.read ", "DIR.READ"} {
		for _, a := range actors {
			if Decide(a, act, Fact{Self: true, Direct: true, Chain: true}) {
				t.Fatalf("unknown action %q allowed", act)
			}
		}
	}
	t.Logf("%d policy combinations checked", n)
}

func TestFieldAccessInvariantsExhaustive(t *testing.T) {
	for _, a := range allActors() {
		for _, f := range allFacts() {
			for _, field := range append(append([]string{}, SensitiveFields...), "", "name", "salary ", "SALARY") {
				for _, write := range []bool{false, true} {
					got := FieldAccess(a, write, field, f)
					if !IsSensitiveField(field) && got {
						t.Fatalf("non-sensitive/unknown field %q allowed", field)
					}
					if a.blocked() && got {
						t.Fatalf("blocked actor allowed on %s: %s", field, describe(a, f))
					}
					if got && !a.hrAdmin() {
						// the only way in for anyone else is being the person themself
						if !f.Self || !a.has(RoleEmployee) {
							t.Fatalf("%s write=%v allowed to non-self non-hr: %s", field, write, describe(a, f))
						}
						if write && field != FieldAddress && field != FieldPhone {
							t.Fatalf("self may not write %s", field)
						}
					}
					// managers, direct or skip-level, never see sensitive fields via the manager role.
					if got && !f.Self && !a.hrAdmin() {
						t.Fatalf("manager path to %s: %s", field, describe(a, f))
					}
				}
			}
		}
	}
}

type scenario struct {
	name  string
	actor Actor
	act   Action
	fact  Fact
	want  bool
}

func staff(roles ...string) Actor {
	return Actor{Subject: "u", Roles: roles, PersonID: "pers_a", HasPerson: true, Status: StActive}
}

func TestPolicyScenarios(t *testing.T) {
	former := staff(RoleEmployee, RoleManager, RoleHR)
	former.Status = StFormer
	onLeaveMgr := staff(RoleEmployee, RoleManager)
	onLeaveMgr.Status = StLeave
	offboardingHR := staff(RoleEmployee, RoleHR)
	offboardingHR.Status = StOffboarding
	svcAdmin := Actor{Subject: "svc", Roles: []string{RoleAdmin}}
	svcMgr := Actor{Subject: "svc", Roles: []string{RoleManager}}

	cases := []scenario{
		// IDOR: an employee reaching for someone else's records.
		{"employee reads colleague basic", staff(RoleEmployee), ActDirRead, Fact{Status: StActive}, true},
		{"employee cannot read candidate", staff(RoleEmployee), ActDirRead, Fact{Status: StCandidate}, false},
		{"employee cannot read former", staff(RoleEmployee), ActDirRead, Fact{Status: StFormer}, false},
		{"employee cannot read colleague history", staff(RoleEmployee), ActDirHistory, Fact{Status: StActive}, false},
		{"employee reads own history", staff(RoleEmployee), ActDirHistory, Fact{Self: true, Status: StActive}, true},
		{"employee cannot read colleague 1:1", staff(RoleEmployee), ActOneOnOneRead, Fact{Status: StActive}, false},
		{"employee cannot read colleague checklist", staff(RoleEmployee), ActChecklistRead, Fact{Status: StActive}, false},
		{"employee cannot complete unassigned task", staff(RoleEmployee), ActTaskComplete, Fact{}, false},
		{"employee completes own task", staff(RoleEmployee), ActTaskComplete, Fact{Assigned: true}, true},
		{"employee reads colleague private goal", staff(RoleEmployee), ActGoalRead, Fact{Scope: "person", Visibility: "private", SameTeam: true}, false},
		{"employee reads colleague company goal", staff(RoleEmployee), ActGoalRead, Fact{Scope: "person", Visibility: "company"}, true},
		{"employee reads teammate team goal", staff(RoleEmployee), ActGoalRead, Fact{Scope: "person", Visibility: "team", SameTeam: true}, true},
		{"employee cannot read other team's team goal", staff(RoleEmployee), ActGoalRead, Fact{Scope: "person", Visibility: "team"}, false},
		{"employee cannot read manager-visibility goal of peer", staff(RoleEmployee), ActGoalRead, Fact{Scope: "person", Visibility: "manager", SameTeam: true}, false},

		// Privilege escalation: roles that were not granted.
		{"employee cannot create person", staff(RoleEmployee), ActDirCreate, Fact{}, false},
		{"employee cannot change status", staff(RoleEmployee), ActDirStatus, Fact{Self: true}, false},
		{"employee cannot place legal hold", staff(RoleEmployee), ActLegalHold, Fact{Self: true}, false},
		{"employee cannot request erasure of self", staff(RoleEmployee), ActErasure, Fact{Self: true}, false},
		{"manager cannot create person", staff(RoleEmployee, RoleManager), ActDirCreate, Fact{Direct: true}, false},
		{"manager cannot change report status", staff(RoleEmployee, RoleManager), ActDirStatus, Fact{Direct: true, Chain: true}, false},
		{"manager cannot run retention", staff(RoleEmployee, RoleManager), ActRetention, Fact{}, false},
		{"unknown role only", staff("superuser"), ActDirRead, Fact{Self: true, Status: StActive}, false},
		{"no roles", staff(), ActDirRead, Fact{Self: true, Status: StActive}, false},
		{"hr may create", staff(RoleEmployee, RoleHR), ActDirCreate, Fact{}, true},
		{"admin may erase", staff(RoleEmployee, RoleAdmin), ActErasure, Fact{}, true},
		{"service admin may run retention", svcAdmin, ActRetention, Fact{}, true},
		{"service account cannot read basic as plain employee", Actor{Subject: "svc", Roles: []string{RoleEmployee}}, ActDirRead, Fact{Status: StActive}, false},
		{"service account never gets manager powers", svcMgr, ActOneOnOneCreate, Fact{Direct: true}, false},

		// Managers and managers of managers.
		{"manager reads direct report basic", staff(RoleEmployee, RoleManager), ActDirRead, Fact{Direct: true, Chain: true, Status: StOnboarding}, true},
		{"skip-level reads report basic", staff(RoleEmployee, RoleManager), ActDirRead, Fact{Chain: true, Status: StActive}, true},
		{"skip-level cannot read 1:1", staff(RoleEmployee, RoleManager), ActOneOnOneRead, Fact{Chain: true}, false},
		{"skip-level cannot read private notes", staff(RoleEmployee, RoleManager), ActOneOnOnePrivate, Fact{Chain: true}, false},
		{"skip-level cannot create 1:1", staff(RoleEmployee, RoleManager), ActOneOnOneCreate, Fact{Chain: true}, false},
		{"skip-level reads manager-visibility goal", staff(RoleEmployee, RoleManager), ActGoalRead, Fact{Scope: "person", Visibility: "manager", Chain: true}, true},
		{"skip-level cannot edit report goal", staff(RoleEmployee, RoleManager), ActGoalWrite, Fact{Scope: "person", Chain: true}, false},
		{"direct manager edits report goal", staff(RoleEmployee, RoleManager), ActGoalWrite, Fact{Scope: "person", Direct: true, Chain: true}, true},
		{"direct manager creates 1:1", staff(RoleEmployee, RoleManager), ActOneOnOneCreate, Fact{Direct: true, Chain: true}, true},
		{"direct manager reads private notes", staff(RoleEmployee, RoleManager), ActOneOnOnePrivate, Fact{Direct: true, Chain: true}, true},
		{"report cannot read private notes", staff(RoleEmployee), ActOneOnOnePrivate, Fact{Self: true}, false},
		{"hr cannot read private notes", staff(RoleEmployee, RoleHR), ActOneOnOnePrivate, Fact{}, false},
		{"admin cannot read 1:1 shared parts", staff(RoleEmployee, RoleAdmin), ActOneOnOneRead, Fact{}, false},
		{"report reads shared 1:1", staff(RoleEmployee), ActOneOnOneRead, Fact{Self: true}, true},
		{"manager role but not the manager", staff(RoleEmployee, RoleManager), ActOneOnOneRead, Fact{}, false},
		{"hr reads company goal", staff(RoleHR), ActGoalRead, Fact{Scope: "person", Visibility: "company"}, true},
		{"hr cannot read private goal", staff(RoleHR), ActGoalRead, Fact{Scope: "person", Visibility: "private"}, false},
		{"owner reads own private goal", staff(RoleEmployee), ActGoalRead, Fact{Scope: "person", Visibility: "private", Self: true}, true},
		{"team lead writes team goal", staff(RoleEmployee, RoleManager), ActGoalWrite, Fact{Scope: "team", TeamLead: true}, true},
		{"manager not leading cannot write team goal", staff(RoleEmployee, RoleManager), ActGoalWrite, Fact{Scope: "team"}, false},
		{"only hr/admin create company goal", staff(RoleEmployee, RoleManager), ActGoalCreate, Fact{Scope: "company", TeamLead: true}, false},
		{"hr creates company goal", staff(RoleEmployee, RoleHR), ActGoalCreate, Fact{Scope: "company"}, true},
		{"employee creates own goal", staff(RoleEmployee), ActGoalCreate, Fact{Scope: "person", Self: true}, true},
		{"employee cannot create goal for peer", staff(RoleEmployee), ActGoalCreate, Fact{Scope: "person"}, false},
		{"bad scope", staff(RoleEmployee, RoleHR), ActGoalCreate, Fact{Scope: "galaxy"}, false},

		// Terminated, absent or restricted managers.
		{"terminated manager cannot read former report basic", former, ActDirRead, Fact{Chain: true, Direct: true, Status: StActive}, false},
		{"terminated manager cannot read 1:1", former, ActOneOnOneRead, Fact{Direct: true}, false},
		{"terminated manager cannot read own record", former, ActDirRead, Fact{Self: true}, false},
		{"terminated hr cannot create", former, ActDirCreate, Fact{}, false},
		{"manager on leave has no manager powers", onLeaveMgr, ActOneOnOnePrivate, Fact{Direct: true}, false},
		{"manager on leave keeps self service", onLeaveMgr, ActDirRead, Fact{Self: true}, true},
		{"manager on leave cannot read report", onLeaveMgr, ActOneOnOneRead, Fact{Direct: true}, false},
		{"offboarding hr loses hr powers", offboardingHR, ActDirCreate, Fact{}, false},
		{"offboarding hr keeps self service", offboardingHR, ActDirRead, Fact{Self: true}, true},
	}
	for _, c := range cases {
		if got := Decide(c.actor, c.act, c.fact); got != c.want {
			t.Errorf("%s: Decide(%s) = %v, want %v", c.name, c.act, got, c.want)
		}
	}
}

func TestFieldAccessScenarios(t *testing.T) {
	former := staff(RoleEmployee, RoleHR)
	former.Status = StFormer
	cases := []struct {
		name  string
		actor Actor
		write bool
		field string
		fact  Fact
		want  bool
	}{
		{"self reads salary", staff(RoleEmployee), false, FieldSalary, Fact{Self: true}, true},
		{"self cannot write salary", staff(RoleEmployee), true, FieldSalary, Fact{Self: true}, false},
		{"self cannot write dob", staff(RoleEmployee), true, FieldDOB, Fact{Self: true}, false},
		{"self cannot write national id", staff(RoleEmployee), true, FieldNationalID, Fact{Self: true}, false},
		{"self writes address", staff(RoleEmployee), true, FieldAddress, Fact{Self: true}, true},
		{"self writes phone", staff(RoleEmployee), true, FieldPhone, Fact{Self: true}, true},
		{"employee cannot read peer salary", staff(RoleEmployee), false, FieldSalary, Fact{}, false},
		{"direct manager cannot read salary", staff(RoleEmployee, RoleManager), false, FieldSalary, Fact{Direct: true, Chain: true}, false},
		{"skip-level cannot read dob", staff(RoleEmployee, RoleManager), false, FieldDOB, Fact{Chain: true}, false},
		{"hr reads national id", staff(RoleEmployee, RoleHR), false, FieldNationalID, Fact{}, true},
		{"hr writes salary", staff(RoleEmployee, RoleHR), true, FieldSalary, Fact{}, true},
		{"admin reads salary", staff(RoleAdmin), false, FieldSalary, Fact{}, true},
		{"service admin reads", Actor{Subject: "svc", Roles: []string{RoleAdmin}}, false, FieldSalary, Fact{}, true},
		{"terminated hr denied", former, false, FieldSalary, Fact{}, false},
		{"unknown field denied", staff(RoleHR), false, "blood_type", Fact{}, false},
	}
	for _, c := range cases {
		if got := FieldAccess(c.actor, c.write, c.field, c.fact); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
