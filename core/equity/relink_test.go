package equity

import (
	"net/http"
	"testing"
)

// A holder record relinked to another person shows that person nothing until somebody else has
// confirmed the new link: until then it still carries the previous holder's identity, grants and
// exercises.

const (
	personHR    = "pers_00000000000000000000d001"
	personBoard = "pers_00000000000000000000d002"
	personOld   = "pers_00000000000000000000d003"
	personNew   = "pers_00000000000000000000d004"
)

// relinkEnv is a directory-mode company with an active grant for the old holder and one exercise
// they requested.
type relinkEnv struct {
	*idEnv
	hr, approver, old, new Actor
	stk, grant, exercise   string
}

func (e *relinkEnv) person(sub, person string, roles ...string) Actor {
	e.dir.subjects[sub] = person
	return e.actor(Principal{Subject: sub, Roles: roles})
}

func (e *relinkEnv) build() *relinkEnv {
	e.hr = e.person("user:hr", personHR, RoleHR)
	e.approver = e.person("user:chair", personBoard, RoleBoard)
	e.old = e.person("user:old", personOld, RoleEmployee)
	e.new = e.person("user:new", personNew, RoleEmployee)
	boss := Actor{Subject: "board-x", Roles: []string{RoleBoard}}
	e.plan = m(e.s.CreatePlan(e.ctx, boss, e.ent.ID, PlanInput{Name: "2026 Plan", ClassID: e.class.ID, Authorised: q(1_000_000), AdoptedOn: "2026-01-01"}))
	e.stk = m(e.s.CreateStakeholder(e.ctx, e.hr, e.ent.ID, StakeholderInput{Kind: "employee", PersonID: personOld,
		Identity: Identity{Name: "Old Holder", Email: "old@example.test", TaxID: "TAX-OLD-1"}})).ID
	m(e.s.VerifyPerson(e.ctx, e.approver, e.stk))
	e.grant = e.draft(e.hr, e.stk, q(4800))
	m(e.s.SubmitGrant(e.ctx, e.hr, e.grant))
	m(e.s.ApproveGrant(e.ctx, e.approver, e.grant))
	m(e.s.ActivateGrant(e.ctx, e.hr, e.grant, ""))
	e.advance("2027-02-01")
	e.s.subjects.m = nil
	id, _, err := e.s.RequestExercise(e.ctx, e.old, e.grant, q(10), "", nil)
	e.must(err)
	e.exercise = id
	return e
}

func newRelinkEnv(t *testing.T, opts ...optFn) *relinkEnv {
	t.Helper()
	return (&relinkEnv{idEnv: newIDEnv(t, opts...)}).build()
}

func TestRelinkedRecordShowsNothingUntilConfirmed(t *testing.T) {
	e := newRelinkEnv(t)
	m(e.s.LinkPerson(e.ctx, e.hr, e.stk, personNew))

	nothing := func(who Actor, name string) {
		t.Helper()
		if p, err := e.s.MyGrants(e.ctx, who, GrantFilter{}); err != nil || len(p.Items) != 0 {
			t.Errorf("%s: MyGrants = %+v %v", name, p, err)
		}
		if l, err := e.s.MyExercises(e.ctx, who, 0); err != nil || len(l.Items) != 0 {
			t.Errorf("%s: MyExercises = %+v %v", name, l, err)
		}
		_, err := e.s.GetGrant(e.ctx, who, e.grant)
		if !isErr(err, ErrNotFound) {
			t.Errorf("%s: GetGrant: %v", name, err)
		}
		if _, err = e.s.Vesting(e.ctx, who, e.grant, ""); !isErr(err, ErrNotFound) {
			t.Errorf("%s: Vesting: %v", name, err)
		}
		if idn, err := e.s.GetIdentity(e.ctx, who, e.stk); !isErr(err, ErrNotFound) {
			t.Errorf("%s: GetIdentity = %+v %v", name, idn, err)
		}
		if _, err = e.s.GetStakeholder(e.ctx, who, e.stk); !isErr(err, ErrNotFound) {
			t.Errorf("%s: GetStakeholder: %v", name, err)
		}
		if _, err = e.s.GetExercise(e.ctx, who, e.exercise); !isErr(err, ErrNotFound) {
			t.Errorf("%s: GetExercise: %v", name, err)
		}
		if _, _, err = e.s.RequestExercise(e.ctx, who, e.grant, q(1), "", nil); !isErr(err, ErrNotFound) {
			t.Errorf("%s: RequestExercise: %v", name, err)
		}
		if _, err = e.s.CancelExercise(e.ctx, who, e.exercise); !isErr(err, ErrNotFound) {
			t.Errorf("%s: CancelExercise: %v", name, err)
		}
		if p, err := e.s.MyPositions(e.ctx, who, 0); err != nil || len(p.Items) != 0 {
			t.Errorf("%s: MyPositions = %+v %v", name, p, err)
		}
	}
	nothing(e.new, "the new person before confirmation")
	nothing(e.old, "the previous holder")
	if n := e.count(`SELECT COUNT(*) FROM exercises WHERE id = ? AND status = 'requested'`, e.exercise); n != 1 {
		t.Fatalf("the exercise was changed")
	}

	// The same through the web UI's administration route, which any signed-in caller can post to.
	st := &StaticTokens{}
	ui := &UI{Svc: e.s, Auth: MultiAuth{st}, Now: e.s.now}
	u := &uiEnv{env: e.env, st: st, ui: ui, h: ui.Handler()}
	c := u.login(t, Actor{Subject: "user:new", Roles: []string{RoleEmployee}})
	c.post("/company/exercises/"+e.exercise+"/cancel", http.StatusNotFound)
	if n := e.count(`SELECT COUNT(*) FROM exercises WHERE id = ? AND status = 'requested'`, e.exercise); n != 1 {
		t.Fatalf("the UI cancelled the exercise")
	}

	// Confirmed by somebody else, the record is the new person's.
	m(e.s.VerifyPerson(e.ctx, e.approver, e.stk))
	if p := m(e.s.MyGrants(e.ctx, e.new, GrantFilter{})); len(p.Items) != 1 {
		t.Errorf("after confirmation: %+v", p)
	}
	if l := m(e.s.MyExercises(e.ctx, e.new, 0)); len(l.Items) != 1 {
		t.Errorf("after confirmation: %+v", l)
	}
	m(e.s.GetIdentity(e.ctx, e.new, e.stk))
	m(e.s.CancelExercise(e.ctx, e.new, e.exercise))
}
