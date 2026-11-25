package board

import (
	"sort"
	"testing"
)

// The policy matrix: every role against every relationship, for every action. The expected sets are written
// out by hand (not derived from the policy code) so a change to the policy shows up as a diff here.

const personX = "pers_0000000000000000000000c4"

var allRoles = []string{RoleDirector, RoleOfficer, RoleSecretary, RoleAdmin, RoleObserver, RoleAuditViewer, RoleEmployee, RoleIntegration}

// rel builds the Fact for a relationship name.
func rel(name string) Fact {
	switch name {
	case "none":
		return Fact{}
	case "member":
		return Fact{Member: true}
	case "chair":
		return Fact{Member: true, Chair: true}
	case "attended":
		return Fact{Member: true, Attended: true}
	case "recused":
		return Fact{Member: true, Attended: true, Recused: true}
	case "conflicted":
		return Fact{Member: true, Attended: true, Conflicted: true}
	case "subject":
		return Fact{Member: true, Attended: true, Subject: true}
	case "drafter":
		return Fact{Member: true, Attended: true, Drafter: true}
	case "proposer":
		return Fact{Member: true, Proposer: true}
	case "invited":
		return Fact{Invited: true}
	case "observerof":
		return Fact{ObserverOf: true}
	case "self":
		return Fact{Self: true}
	}
	panic("unknown relationship " + name)
}

var allRels = []string{"none", "member", "chair", "attended", "recused", "conflicted", "subject", "drafter", "proposer", "invited", "observerof", "self"}

// memberRels are the relationships in which the actor is an in-term member of the body.
var memberRels = []string{"member", "chair", "attended", "recused", "conflicted", "subject", "drafter", "proposer"}

type rows map[string][]string

func every(roles ...string) rows {
	m := rows{}
	for _, r := range roles {
		m[r] = []string{"*"}
	}
	return m
}

func (m rows) with(role string, rels ...string) rows {
	m[role] = rels
	return m
}

func allowedMatrix() map[Action]rows {
	self := every(RoleSecretary)
	for _, r := range []string{RoleDirector, RoleOfficer, RoleAdmin, RoleObserver, RoleAuditViewer, RoleEmployee, RoleIntegration} {
		self[r] = []string{"self"}
	}
	actionRead := every(RoleSecretary).with(RoleDirector, append([]string{"self"}, memberRels...)...)
	for _, r := range []string{RoleOfficer, RoleAdmin, RoleObserver, RoleAuditViewer, RoleEmployee, RoleIntegration} {
		actionRead[r] = []string{"self"}
	}
	return map[Action]rows{
		ActBodyManage:         every(RoleSecretary, RoleAdmin),
		ActMemberManage:       every(RoleSecretary),
		ActMeetingSchedule:    every(RoleSecretary).with(RoleDirector, "chair"),
		ActAgendaManage:       every(RoleSecretary).with(RoleDirector, "chair"),
		ActAttendanceRecord:   every(RoleSecretary),
		ActPackManage:         every(RoleSecretary),
		ActPackRead:           every(RoleSecretary).with(RoleDirector, memberRels...).with(RoleOfficer, "invited"),
		ActConflictDeclare:    every(RoleSecretary).with(RoleDirector, memberRels...),
		ActResolutionDraft:    every(RoleSecretary).with(RoleDirector, memberRels...),
		ActResolutionEdit:     rows{}.with(RoleSecretary, "proposer").with(RoleDirector, "proposer"),
		ActResolutionWithdraw: every(RoleSecretary).with(RoleDirector, "proposer"),
		ActVoteOpen:           every(RoleSecretary).with(RoleDirector, "chair"),
		ActVoteCast:           rows{}.with(RoleDirector, "member", "chair", "attended", "conflicted", "drafter", "proposer"),
		ActVoteClose:          every(RoleSecretary).with(RoleDirector, "chair"),
		ActMinutesDraft:       every(RoleSecretary),
		ActMinutesApprove:     rows{}.with(RoleDirector, "attended", "recused", "conflicted", "subject"),
		ActActionManage:       every(RoleSecretary).with(RoleDirector, "chair"),
		ActActionUpdate:       self,
		ActResolutionRequest:  every(RoleIntegration),
		ActResolutionOutcome:  every(RoleIntegration),
		ActLegalHold:          every(RoleAdmin),
		ActErasure:            every(RoleAdmin),
		ActRetention:          every(RoleAdmin),
		ActEntityManage:       every(RoleSecretary, RoleAdmin),
		ActEntityRead:         every(RoleSecretary, RoleAdmin, RoleOfficer, RoleAuditViewer).with(RoleDirector, memberRels...),
		ActRegisterWrite:      every(RoleSecretary),
		ActRegisterAttest: rows{}.with(RoleDirector, "member", "chair", "attended", "recused", "conflicted", "proposer").
			with(RoleOfficer, "none", "member", "chair", "attended", "recused", "conflicted", "proposer", "invited", "observerof", "self"),
		ActRegisterRead:     every(RoleSecretary, RoleOfficer, RoleAuditViewer).with(RoleDirector, memberRels...),
		ActFilingManage:     every(RoleSecretary),
		ActFilingMark:       every(RoleSecretary, RoleOfficer),
		ActBodyRead:         every(RoleSecretary, RoleAdmin, RoleAuditViewer).with(RoleDirector, memberRels...).with(RoleOfficer, "invited").with(RoleObserver, "observerof"),
		ActMeetingRead:      every(RoleSecretary, RoleAdmin, RoleAuditViewer).with(RoleDirector, memberRels...).with(RoleOfficer, "invited").with(RoleObserver, "observerof"),
		ActResolutionRead:   every(RoleSecretary, RoleAdmin, RoleAuditViewer).with(RoleDirector, memberRels...),
		ActMinutesRead:      every(RoleSecretary, RoleAuditViewer).with(RoleDirector, memberRels...).with(RoleOfficer, "invited").with(RoleObserver, "observerof"),
		ActMinutesDraftRead: every(RoleSecretary).with(RoleDirector, "chair", "attended", "recused", "conflicted", "subject", "drafter"),
		ActActionRead:       actionRead,
	}
}

func actorFor(role string) Actor {
	return Actor{Subject: "sso-" + role, Roles: []string{role}, PersonID: personX}
}

func TestPolicyMatrix(t *testing.T) {
	matrix := allowedMatrix()
	var acts []string
	for a := range matrix {
		acts = append(acts, string(a))
	}
	sort.Strings(acts)
	for _, name := range acts {
		act := Action(name)
		for _, role := range allRoles {
			allowed := map[string]bool{}
			for _, r := range matrix[act][role] {
				if r == "*" {
					for _, x := range allRels {
						allowed[x] = true
					}
				} else {
					allowed[r] = true
				}
			}
			for _, r := range allRels {
				if got := Decide(actorFor(role), act, rel(r)); got != allowed[r] {
					t.Errorf("Decide(%s, %s, %s) = %v, want %v", role, act, r, got, allowed[r])
				}
			}
		}
	}
}

func TestPolicyEveryActionIsInTheMatrix(t *testing.T) {
	m := allowedMatrix()
	for _, a := range AllActions {
		if _, ok := m[a]; !ok {
			t.Errorf("action %s has no row in the policy matrix test", a)
		}
	}
	if len(m) != len(AllActions) {
		t.Errorf("matrix has %d rows, AllActions has %d", len(m), len(AllActions))
	}
}

func TestPolicyDeniesUnknownRolesAndMissingPersonLink(t *testing.T) {
	for _, act := range AllActions {
		for _, r := range allRels {
			if Decide(Actor{Subject: "x", Roles: []string{"owner"}, PersonID: personX}, act, rel(r)) {
				t.Errorf("a reserved role was allowed %s (%s)", act, r)
			}
			if Decide(Actor{Subject: "x"}, act, rel(r)) {
				t.Errorf("an actor with no role was allowed %s", act)
			}
		}
	}
	// membership comes from the person link: a director token with no person id has no body access
	for _, act := range AllActions {
		for _, r := range []string{"member", "chair", "attended", "proposer"} {
			if Decide(Actor{Subject: "sso-d", Roles: []string{RoleDirector}}, act, rel(r)) {
				t.Errorf("a director without a person link was allowed %s as %s", act, r)
			}
		}
	}
}

func TestFieldAccess(t *testing.T) {
	final := func(f Fact) Fact { f.Final = true; return f }
	secret := func(f Fact) Fact { f.Secret = true; f.Final = true; return f }
	cases := []struct {
		role  string
		write bool
		field string
		rel   Fact
		want  bool
	}{
		{RoleSecretary, false, FieldResolutionText, rel("none"), true},
		{RoleDirector, false, FieldResolutionText, rel("member"), true},
		{RoleDirector, false, FieldResolutionText, rel("recused"), false},
		{RoleDirector, false, FieldResolutionText, rel("subject"), false},
		{RoleDirector, false, FieldResolutionText, rel("none"), false},
		{RoleAdmin, false, FieldResolutionText, rel("none"), false},
		{RoleAuditViewer, false, FieldResolutionText, rel("none"), false},
		{RoleAuditViewer, false, FieldResolutionText, final(rel("none")), true},
		{RoleDirector, true, FieldResolutionText, rel("proposer"), true},
		{RoleDirector, true, FieldResolutionText, rel("member"), false},
		{RoleSecretary, false, FieldVoteDetail, rel("none"), false}, // not while voting is open
		{RoleSecretary, false, FieldVoteDetail, final(rel("none")), true},
		{RoleSecretary, false, FieldVoteDetail, secret(rel("none")), false},
		{RoleDirector, false, FieldVoteDetail, rel("member"), false}, // not while voting is open
		{RoleDirector, false, FieldVoteDetail, final(rel("member")), true},
		{RoleDirector, false, FieldVoteDetail, final(rel("recused")), false},
		{RoleDirector, false, FieldVoteDetail, secret(rel("member")), false},
		{RoleAuditViewer, false, FieldVoteDetail, final(rel("none")), true},
		{RoleObserver, false, FieldVoteDetail, final(rel("observerof")), false},
		{RoleSecretary, false, FieldMinutesDraft, rel("none"), true},
		{RoleDirector, false, FieldMinutesDraft, rel("attended"), true},
		{RoleDirector, false, FieldMinutesDraft, rel("member"), false},
		{RoleAdmin, false, FieldMinutesDraft, rel("none"), false},
		{RoleSecretary, true, FieldMinutesDraft, rel("none"), true},
		{RoleDirector, true, FieldMinutesDraft, rel("chair"), false},
		{RoleSecretary, false, FieldConflictNature, rel("none"), true},
		{RoleDirector, false, FieldConflictNature, rel("chair"), true},
		{RoleDirector, false, FieldConflictNature, rel("member"), false},
		{RoleDirector, false, FieldConflictNature, Fact{Member: true, Self: true}, true},
		{RoleSecretary, false, FieldRegNumber, rel("none"), true},
		{RoleAuditViewer, false, FieldRegNumber, rel("none"), true},
		{RoleAdmin, false, FieldRegNumber, rel("none"), false},
		{RoleEmployee, false, FieldRegNumber, rel("none"), false},
		{RoleSecretary, true, FieldRegNumber, rel("none"), true},
		{RoleOfficer, true, FieldRegNumber, rel("none"), false},
		{RoleSecretary, false, FieldPersonPrivate, rel("none"), true},
		{RoleDirector, false, FieldPersonPrivate, rel("member"), false},
		{RoleDirector, false, FieldPersonPrivate, Fact{Self: true}, true},
		{RoleAuditViewer, false, FieldPersonPrivate, rel("none"), false},
		{RoleSecretary, false, FieldRegisterDetail, rel("none"), true},
		{RoleAuditViewer, false, FieldRegisterDetail, rel("none"), true},
		{RoleObserver, false, FieldRegisterDetail, rel("observerof"), false},
		{RoleAdmin, false, FieldRegisterDetail, rel("none"), false},
		{RoleSecretary, false, "no-such-field", rel("none"), false},
	}
	for _, x := range cases {
		if got := FieldAccess(actorFor(x.role), x.write, x.field, x.rel); got != x.want {
			t.Errorf("FieldAccess(%s, write=%v, %s, %+v) = %v, want %v", x.role, x.write, x.field, x.rel, got, x.want)
		}
	}
}

func TestSeparationOfDuties(t *testing.T) {
	a := Actor{Subject: "Alice@Example.test", PersonID: personX}
	same := []string{"alice@example.test", "ALICE@EXAMPLE.TEST ", "Alice@Example.test", "al\u200bice@example.test", "ａｌｉｃｅ@example.test", personX}
	for _, o := range same {
		if sodOK(a, o) {
			t.Errorf("%q should be the same person as the actor", o)
		}
	}
	if !sodOK(a, "bob@example.test", "", "sso-carol") {
		t.Error("different people must pass")
	}
	if sodOK(a, "bob", "alice@example.test") {
		t.Error("one match among several must fail")
	}
}

func TestRolesAreKnownAndNotReserved(t *testing.T) {
	for _, r := range allRoles {
		if !(Actor{Roles: []string{r}}).known() {
			t.Errorf("%s is not a known role", r)
		}
		if r == "owner" || r == "operator" || r == "auditor" {
			t.Errorf("%s is reserved for can-core", r)
		}
	}
}
