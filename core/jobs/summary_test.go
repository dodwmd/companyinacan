package jobs

import (
	"context"
	"reflect"
	"testing"
)

func TestSummaryCountsFollowRoleAndScope(t *testing.T) {
	s := newScenario(t)
	ctx := context.Background()
	st := s.app.Store()
	build := summaryBuilder(st)
	figures := func(subject string, perms ...string) map[string]int64 {
		t.Helper()
		got, err := build(ctx, subject, perms)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, f := range got {
			out[f.Key] = f.Count
			if f.Amounts != nil {
				t.Errorf("%s carries amounts", f.Key)
			}
		}
		return out
	}
	recruiter := []string{"jobs.read", "jobs.write", "ats.read", "ats.write", "ats.hire", "ats.interview"}
	manager := []string{"jobs.read", "ats.read", "ats.write", "ats.interview"}
	want := func(got, exp map[string]int64) {
		t.Helper()
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("figures %v, want %v", got, exp)
		}
	}
	want(figures("user:rita", recruiter...), map[string]int64{"requisitions.open": 2, "applications.new": 2, "interviews.due": 0})
	// A hiring manager sees the jobs they manage, nobody else's.
	if got := figures("user:nobody", manager...); got["requisitions.open"] != 0 || got["applications.new"] != 0 {
		t.Errorf("a manager of nothing: %v", got)
	}
	// ivy interviews one application: no requisition figure at all, and only that application.
	s.want(s.do(s.recruiter, "PUT", "/v1/applications/"+s.a1+"/interviewers/user:ivy", nil), 204)
	want(figures("user:ivy", "ats.interview"), map[string]int64{"applications.new": 1, "interviews.due": 1})

	rita := &Principal{Subject: "user:rita", Role: RoleRecruiter}
	s.want(s.do(s.recruiter, "PUT", "/v1/applications/"+s.a1+"/interviewers/user:rita", nil), 204)
	if _, err := st.SaveScorecard(ctx, rita, s.a1, ScorecardInput{Rating: 3, Recommendation: "yes"}); err != nil {
		t.Fatal(err)
	}
	if got := figures("user:rita", recruiter...); got["interviews.due"] != 1 {
		t.Errorf("a draft scorecard does not finish an interview: %v", got)
	}
	if _, err := st.SubmitScorecard(ctx, rita, s.a1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MoveApplication(ctx, rita, s.a1, "screen"); err != nil {
		t.Fatal(err)
	}
	want(figures("user:rita", recruiter...), map[string]int64{"requisitions.open": 2, "applications.new": 1, "interviews.due": 0})
	// Permissions that make no role, and a subject that is no user account, get nothing.
	want(figures("user:rita", "jobs.read"), map[string]int64{})
	want(figures("svc:static", recruiter...), map[string]int64{})
}
