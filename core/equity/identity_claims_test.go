package equity

import "testing"

// Specs/051 says which claims hold only in directory mode. These tests pin both sides, so the
// document and the service cannot drift apart.

// Attestation mode (no directory, what can-core installs): one administrator with a board seat and
// the service account token gives themself a grant, without break-glass. Nothing connects their
// account to the holder record.
func TestAttestationModeOneAdministratorCanGrantThemself(t *testing.T) {
	path := grantPath{creator: "svc", confirmer: "ann", drafter: "svc", approver: "ann"}
	e := smallCompany(t, ModeAttestation, false)
	if !tryGrant(e, "", path) {
		t.Fatal("the documented attestation-mode path is gone: update specs/051-holder-identity and docs/equity.md")
	}
	// their own founder shares need no break-glass either (tryGrant made a new entity)
	e.class = m(e.s.CreateShareClass(e.ctx, Actor{Subject: "board-setup", Roles: []string{RoleBoard}}, e.ent.ID, ShareClassInput{Name: "Founder", Kind: "common", ParMinor: 1, Currency: "USD", Authorised: q(10_000)}))
	st := m(e.holder(e.svc, "", "Ann herself"))
	m(e.s.AttestExternal(e.ctx, e.staff("ann"), st.ID))
	e.must(e.issue(e.staff("ann"), st.ID))

	// the same steps are refused with a directory
	if tryGrant(smallCompany(t, ModeDirectory, false), personAnn, path) {
		t.Fatal("directory mode let an administrator approve their own grant")
	}
}

// Directory mode, two founders: whoever drafted and whoever approved an employee's grant are the
// only two people, so neither can record a change to what vested. The service account does it
// under break-glass.
func TestTwoFoundersNeedBreakGlassForVestingFacts(t *testing.T) {
	const personEmp = "pers_0000000000000000000000e9"
	for _, breakGlass := range []bool{false, true} {
		e := smallCompany(t, ModeDirectory, breakGlass)
		e.dir.subjects["user:emp"] = personEmp
		boss := Actor{Subject: "board-setup", Roles: []string{RoleBoard}}
		plan := m(e.s.CreatePlan(e.ctx, boss, e.ent.ID, PlanInput{Name: "Plan", ClassID: e.class.ID, Authorised: q(10_000), AdoptedOn: "2026-01-01"}))
		stk := m(e.holder(e.ann, personEmp, "Employee"))
		m(e.s.VerifyPerson(e.ctx, e.ben, stk.ID))
		in := e.standardInput(stk.ID, q(4800))
		in.Schedule = Schedule{Kind: "tranches", Tranches: []Tranche{{Label: "launch", BasisPoints: 10_000, Milestone: true}},
			Acceleration: []Acceleration{{Name: "sale", Type: "single", Triggers: []string{"change_of_control"}, PercentBP: 10_000}}}
		id, _, err := e.s.CreateGrant(e.ctx, e.ann, plan.ID, in, "", nil)
		e.must(err)
		m(e.s.SubmitGrant(e.ctx, e.ann, id))
		m(e.s.ApproveGrant(e.ctx, e.boardSeat("ben"), id))
		m(e.s.ActivateGrant(e.ctx, e.ann, id, ""))
		today := e.s.today()
		steps := []struct {
			name string
			do   func(Actor) error
		}{
			{"milestone", func(a Actor) error { return e.s.RecordMilestone(e.ctx, a, id, 0, today) }},
			{"acceleration", func(a Actor) error { return e.s.RecordAcceleration(e.ctx, a, id, "change_of_control", today) }},
			{"termination", func(a Actor) error {
				_, err := e.s.TerminateGrant(e.ctx, a, id, today, false)
				return err
			}},
		}
		for _, s := range steps {
			wantErr(t, s.do(e.ann), ErrSeparation, s.name+" by the drafter")
			wantErr(t, s.do(e.ben), ErrSeparation, s.name+" by the approver")
			if !breakGlass {
				wantErr(t, s.do(e.svc), ErrIdentityRequired, s.name+" by the service account")
			} else if err := s.do(e.svc); err != nil {
				t.Fatalf("%s under break-glass: %v", s.name, err)
			}
		}
	}
}
