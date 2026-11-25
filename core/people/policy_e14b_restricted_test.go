package people

import (
	"strings"
	"testing"
)

// A restricted token (a personal access token from can-core) needs the matching permission for
// each action on top of its role; a static service token is unaffected.
func TestE14bRestrictedTokensNeedTheirPermissions(t *testing.T) {
	need := map[Action]string{}
	for _, act := range e14bActions {
		p, ok := actionPerm[act]
		if !ok {
			t.Fatalf("%s has no permission in actionPerm", act)
		}
		if !strings.HasPrefix(p, "people.reviews.") && !strings.HasPrefix(p, "people.timeoff.") {
			t.Fatalf("%s needs %q: E14b actions use their own permissions", act, p)
		}
		need[act] = p
	}
	facts := e14bFacts()
	for _, base := range allActors() {
		for i := 0; i < len(facts); i += 7 {
			f := facts[i]
			for _, act := range e14bActions {
				want := Decide(base, act, f)
				none := base
				none.Restricted, none.Perms = true, []string{"people.read", "performance.read", "performance.write", "people.write", "people.admin"}
				if Decide(none, act, f) {
					t.Fatalf("%s allowed without %s: %s", act, need[act], describe(none, f))
				}
				full := base
				full.Restricted, full.Perms = true, []string{need[act]}
				if got := Decide(full, act, f); got != want {
					t.Fatalf("%s with exactly %s = %v, unrestricted = %v: %s", act, need[act], got, want, describe(full, f))
				}
			}
		}
	}
}

func TestRestrictedTokenListsNothingWithoutReviewOrLeavePermissions(t *testing.T) {
	x := newToEnv(t)
	_, id := x.openCycle("attributed")
	x.must(x.s.SaveSelf(x.ctx, x.annA, id, Answers{"q1": {Rating: rating(3)}}, true))
	lr := x.submit(x.annA, x.vac, "2026-04-06", "2026-04-07")
	_ = lr
	for name, a := range map[string]Actor{"ann": x.annA, "hr": x.hr, "manager": x.mgrA} {
		a.Restricted, a.Perms = true, []string{"people.read", "performance.read"}
		if l, _ := x.s.ListReviews(x.ctx, a, ""); len(l) != 0 {
			t.Errorf("%s: reviews listed without people.reviews.read", name)
		}
		if l, _ := x.s.ListCycles(x.ctx, a); len(l) != 0 {
			t.Errorf("%s: cycles listed without people.reviews.read", name)
		}
		if l, _ := x.s.ListLeaveRequests(x.ctx, a, LeaveFilter{}); len(l) != 0 {
			t.Errorf("%s: leave requests listed without people.timeoff.read", name)
		}
		if _, err := x.s.GetReview(x.ctx, a, id); err == nil {
			t.Errorf("%s: review read without people.reviews.read", name)
		}
		if _, err := x.s.GetLeaveRequest(x.ctx, a, lr.ID); err == nil && name != "x" {
			t.Errorf("%s: leave request read without people.timeoff.read", name)
		}
	}
}
