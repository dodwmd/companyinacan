package people

import (
	"fmt"
	"testing"
)

// E14b policy tables: every role subset x person status x relationship x cycle/review state,
// checked against an oracle that is written independently of Decide (flat boolean rules taken
// from specs/022-reviews-timeoff/spec.md), plus named scenarios and invariants.

var e14bActions = []Action{
	ActRevManage, ActRevMeta, ActRevSelfRead, ActRevSelfWrite, ActRevMgrRead, ActRevMgrWrite,
	ActRevCalibRead, ActRevCalibWrite, ActRevOutcomeRead, ActRevPeerRead, ActRevPeerRaw,
	ActRevNominate, ActRevNominationDecide, ActRevFeedbackGive, ActRevRelease, ActRevReleaseDecide, ActRevAck,
	ActLeaveConfig, ActLeaveAdjust, ActLeaveWrite, ActLeaveRead, ActLeaveDecide, ActLeaveCancel, ActLeaveBalance, ActLeaveCalendar,
}

func e14bFacts() []Fact {
	var out []Fact
	bools := []bool{false, true}
	for _, self := range bools {
		for _, direct := range bools {
			for _, chain := range bools {
				if self && (direct || chain) {
					continue // nobody is their own manager
				}
				for _, asg := range bools {
					for _, state := range []string{"", "draft", "open", "calibration", "closed"} {
						for _, selfDone := range bools {
							for _, mgrDone := range bools {
								for _, rel := range bools {
									for _, relReq := range bools {
										out = append(out, Fact{Self: self, Direct: direct, Chain: chain || direct, Assigned: asg, Status: StActive,
											CycleState: state, SelfDone: selfDone, MgrDone: mgrDone, Released: rel, ReleaseRequested: relReq})
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

// oracle restates the rules without using Actor helpers.
func oracle(a Actor, act Action, f Fact) bool {
	has := func(r string) bool {
		for _, x := range a.Roles {
			if x == r {
				return true
			}
		}
		return false
	}
	if !has(RoleEmployee) && !has(RoleManager) && !has(RoleHR) && !has(RoleAdmin) {
		return false
	}
	if a.HasPerson && a.Status != StOnboarding && a.Status != StActive && a.Status != StLeave && a.Status != StOffboarding {
		return false
	}
	activeOrSvc := !a.HasPerson || a.Status == StActive
	hr := (has(RoleHR) || has(RoleAdmin)) && activeOrSvc
	hrOther := hr && !f.Self
	staff := a.HasPerson && has(RoleEmployee)
	self := staff && f.Self
	mgr := has(RoleManager) && a.HasPerson && a.Status == StActive
	direct := mgr && f.Direct
	chain := mgr && f.Chain
	open := f.CycleState == "open"
	switch act {
	case ActRevManage, ActLeaveConfig:
		return hr
	case ActRevMeta:
		return self || direct || hrOther || (chain && f.Released)
	case ActRevSelfRead:
		return self || (direct && f.SelfDone) || hrOther
	case ActRevSelfWrite:
		return self && open && !f.SelfDone
	case ActRevMgrRead:
		return direct || (self && f.Released) || hrOther
	case ActRevMgrWrite:
		return direct && open && !f.MgrDone
	case ActRevCalibRead:
		return hrOther
	case ActRevCalibWrite:
		return hrOther && f.CycleState == "calibration" && !f.Released && !f.ReleaseRequested
	case ActRevOutcomeRead:
		return ((self || direct || chain) && f.Released) || hrOther
	case ActRevPeerRead:
		return (self && f.Released) || direct || hrOther
	case ActRevPeerRaw:
		return direct || hrOther
	case ActRevNominate:
		return open && (self || direct || hrOther)
	case ActRevNominationDecide:
		return open && (direct || hrOther)
	case ActRevFeedbackGive:
		return open && staff && f.Assigned && !f.Self
	case ActRevRelease:
		return hrOther && f.CycleState == "calibration" && f.MgrDone && !f.Released && !f.ReleaseRequested
	case ActRevReleaseDecide:
		return hrOther && f.CycleState == "calibration" && f.ReleaseRequested && !f.Released
	case ActRevAck:
		return self && f.Released
	case ActLeaveAdjust:
		return hrOther
	case ActLeaveWrite:
		return self
	case ActLeaveRead, ActLeaveBalance:
		return self || direct || hr
	case ActLeaveDecide:
		return direct || hrOther
	case ActLeaveCancel:
		return self || hrOther
	case ActLeaveCalendar:
		return staff || hr
	}
	return false
}

func TestE14bPolicyAgainstOracle(t *testing.T) {
	n := 0
	for _, a := range allActors() {
		for _, f := range e14bFacts() {
			for _, act := range e14bActions {
				n++
				if got, want := Decide(a, act, f), oracle(a, act, f); got != want {
					t.Fatalf("%s: Decide = %v, oracle = %v for %s", act, got, want, describe(a, f))
				}
			}
		}
	}
	t.Logf("%d E14b policy combinations checked", n)
}

// Rules that must hold whatever the oracle says (guards against a wrong oracle too).
func TestE14bPolicyInvariants(t *testing.T) {
	for _, a := range allActors() {
		for _, f := range e14bFacts() {
			// Calibration notes are never visible to the subject, whatever their other roles.
			if f.Self && Decide(a, ActRevCalibRead, f) {
				t.Fatalf("subject reads calibration notes: %s", describe(a, f))
			}
			// The subject never reads raw peer comments, and never anything before release except their own self part.
			if f.Self {
				if Decide(a, ActRevPeerRaw, f) {
					t.Fatalf("subject reads raw feedback: %s", describe(a, f))
				}
				if !f.Released && (Decide(a, ActRevMgrRead, f) || Decide(a, ActRevOutcomeRead, f) || Decide(a, ActRevPeerRead, f)) {
					t.Fatalf("subject reads unreleased content: %s", describe(a, f))
				}
				// No review power over oneself, even for hr/admin.
				for _, act := range []Action{ActRevCalibWrite, ActRevRelease, ActRevReleaseDecide, ActLeaveAdjust, ActLeaveDecide} {
					if Decide(a, act, f) {
						t.Fatalf("%s allowed on oneself: %s", act, describe(a, f))
					}
				}
			}
			// Skip-level managers see released outcomes only.
			if f.Chain && !f.Direct && !f.Self && !a.hrAdmin() {
				for _, act := range []Action{ActRevSelfRead, ActRevMgrRead, ActRevCalibRead, ActRevPeerRead, ActRevPeerRaw, ActLeaveRead, ActLeaveBalance, ActLeaveDecide} {
					if Decide(a, act, f) {
						t.Fatalf("skip-level got %s: %s", act, describe(a, f))
					}
				}
				if !f.Released && (Decide(a, ActRevMeta, f) || Decide(a, ActRevOutcomeRead, f)) {
					t.Fatalf("skip-level reads an unreleased review: %s", describe(a, f))
				}
			}
			// No writes outside an open cycle, except calibration, release and acknowledgement.
			for _, act := range []Action{ActRevSelfWrite, ActRevMgrWrite, ActRevNominate, ActRevNominationDecide, ActRevFeedbackGive} {
				if f.CycleState != "open" && Decide(a, act, f) {
					t.Fatalf("%s allowed while cycle is %q", act, f.CycleState)
				}
			}
			// Roles that are not hr/admin never configure anything.
			if !a.hrAdmin() {
				for _, act := range []Action{ActRevManage, ActLeaveConfig, ActLeaveAdjust, ActRevCalibRead, ActRevCalibWrite, ActRevRelease, ActRevReleaseDecide} {
					if Decide(a, act, f) {
						t.Fatalf("%s allowed to a non-hr actor: %s", act, describe(a, f))
					}
				}
			}
			if a.blocked() {
				for _, act := range e14bActions {
					if Decide(a, act, f) {
						t.Fatalf("blocked actor allowed %s", act)
					}
				}
			}
		}
	}
}

func TestE14bPolicyScenarios(t *testing.T) {
	onLeaveMgr := staff(RoleEmployee, RoleManager)
	onLeaveMgr.Status = StLeave
	hiring := staff("hiring") // a role from another module is not a role here
	cases := []scenario{
		{"employee reads own released outcome", staff(RoleEmployee), ActRevOutcomeRead, Fact{Self: true, Released: true}, true},
		{"employee cannot read own unreleased outcome", staff(RoleEmployee), ActRevOutcomeRead, Fact{Self: true}, false},
		{"employee cannot read colleague review", staff(RoleEmployee), ActRevMeta, Fact{Released: true}, false},
		{"hr cannot read own calibration notes", staff(RoleEmployee, RoleHR), ActRevCalibRead, Fact{Self: true}, false},
		{"hr reads other's calibration notes", staff(RoleEmployee, RoleHR), ActRevCalibRead, Fact{}, true},
		{"manager of someone else reads nothing", staff(RoleEmployee, RoleManager), ActRevMgrRead, Fact{}, false},
		{"skip-level reads released summary", staff(RoleEmployee, RoleManager), ActRevOutcomeRead, Fact{Chain: true, Released: true}, true},
		{"skip-level cannot read manager part", staff(RoleEmployee, RoleManager), ActRevMgrRead, Fact{Chain: true, Released: true}, false},
		{"manager on leave cannot write", onLeaveMgr, ActRevMgrWrite, Fact{Direct: true, Chain: true, CycleState: "open"}, false},
		{"hiring role gets nothing", hiring, ActRevMeta, Fact{Self: true, Released: true}, false},
		{"hiring role gets no leave", hiring, ActLeaveRead, Fact{Self: true}, false},
		{"employee requests leave", staff(RoleEmployee), ActLeaveWrite, Fact{Self: true}, true},
		{"employee cannot request for a colleague", staff(RoleEmployee), ActLeaveWrite, Fact{}, false},
		{"manager decides report leave", staff(RoleEmployee, RoleManager), ActLeaveDecide, Fact{Direct: true, Chain: true}, true},
		{"manager cannot decide own leave via hr path", staff(RoleEmployee, RoleManager), ActLeaveDecide, Fact{Self: true}, false},
		{"colleague cannot read leave", staff(RoleEmployee), ActLeaveRead, Fact{}, false},
		{"svc admin reads leave", Actor{Subject: "svc", Roles: []string{RoleAdmin}}, ActLeaveRead, Fact{}, true},
		{"svc cannot request leave", Actor{Subject: "svc", Roles: []string{RoleAdmin}}, ActLeaveWrite, Fact{Self: true}, false},
		{"reviewer gives feedback", staff(RoleEmployee), ActRevFeedbackGive, Fact{Assigned: true, CycleState: "open"}, true},
		{"unassigned cannot", staff(RoleEmployee), ActRevFeedbackGive, Fact{CycleState: "open"}, false},
		{"subject cannot review self as peer", staff(RoleEmployee), ActRevFeedbackGive, Fact{Assigned: true, Self: true, CycleState: "open"}, false},
	}
	for _, c := range cases {
		if got := Decide(c.actor, c.act, c.fact); got != c.want {
			t.Errorf("%s: Decide(%s) = %v, want %v", c.name, c.act, got, c.want)
		}
	}
	_ = fmt.Sprint
}
