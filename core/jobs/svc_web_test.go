package jobs

import (
	"context"
	"strings"
	"testing"
)

func (e *env) principal(tok string) *Principal {
	e.t.Helper()
	r := e.want(e.do(tok, "GET", "/v1/me", nil), 200).m(e.t)
	p := &Principal{Subject: r["subject"].(string), Role: r["role"].(string)}
	for _, j := range r["jobs"].([]any) {
		p.Jobs = append(p.Jobs, j.(string))
	}
	return p
}

func TestFunnelCountsAreScopedAndAggregate(t *testing.T) {
	s := newScenario(t)
	st := s.app.Store()
	ctx := context.Background()
	f, err := st.FunnelCounts(ctx, s.principal(s.recruiter), "")
	if err != nil || f.Active["applied"] != 2 {
		t.Fatalf("recruiter funnel = %+v, %v", f, err)
	}
	f, err = st.FunnelCounts(ctx, s.principal(s.hmAlice), "")
	if err != nil || f.Active["applied"] != 1 {
		t.Fatalf("hiring manager sees only own job: %+v, %v", f, err)
	}
	if _, err := st.FunnelCounts(ctx, s.principal(s.hmAlice), s.j2); err != ErrNotFound {
		t.Errorf("another manager's job = %v, want not found", err)
	}
	if _, err := st.FunnelCounts(ctx, s.principal(s.ivy), ""); err != ErrForbidden {
		t.Errorf("interviewer funnel = %v, want forbidden", err)
	}
}

func TestListMyInterviewsOnlyActiveAssignments(t *testing.T) {
	s := newScenario(t)
	st := s.app.Store()
	ctx := context.Background()
	ivy := s.principal(s.ivy)
	l, _, err := st.ListMyInterviews(ctx, ivy, 0, 0)
	if err != nil || len(l) != 1 || l[0].ApplicationID != s.a1 || l[0].Scorecard != "" {
		t.Fatalf("ivy interviews = %+v, %v", l, err)
	}
	if l, _, _ := st.ListMyInterviews(ctx, s.principal(s.ian), 0, 0); len(l) != 0 {
		t.Errorf("ian was never assigned but sees %+v", l)
	}
	s.want(s.do(s.recruiter, "DELETE", "/v1/applications/"+s.a1+"/interviewers/ivy", nil), 204)
	if l, _, _ := st.ListMyInterviews(ctx, ivy, 0, 0); len(l) != 0 {
		t.Errorf("a removed assignment is still listed: %+v", l)
	}
	if ok, err := st.IsAssigned(ctx, s.principal(s.recruiter), s.a1); err != nil || ok {
		t.Errorf("IsAssigned(recruiter) = %v, %v", ok, err)
	}
}

func TestMoveApplicationNoteSealedAndBounded(t *testing.T) {
	s := newScenario(t)
	st := s.app.Store()
	ctx := context.Background()
	rec := s.principal(s.recruiter)
	if _, err := st.MoveApplicationNote(ctx, rec, s.a1, "screen", ""); err == nil {
		t.Error("a move without a reason was accepted")
	}
	if _, err := st.MoveApplicationNote(ctx, rec, s.a1, "screen", strings.Repeat("x", MoveNoteMax+1)); err == nil {
		t.Error("an over-long reason was accepted")
	}
	v, err := st.MoveApplicationNote(ctx, rec, s.a1, "screen", "strong portfolio, secret-reason")
	if err != nil {
		t.Fatal(err)
	}
	last := v.History[len(v.History)-1]
	if last.Note == nil || *last.Note != "strong portfolio, secret-reason" {
		t.Errorf("recruiter does not see the note: %+v", last)
	}
	if strings.Contains(string(s.rawDBBytes()), "secret-reason") {
		t.Error("the move reason is stored in plaintext")
	}
	hv, err := st.GetApplication(ctx, s.principal(s.hmAlice), s.a1)
	if err != nil {
		t.Fatal(err)
	}
	if n := hv.History[len(hv.History)-1].Note; n != nil {
		t.Errorf("the hiring manager sees the move reason %q", *n)
	}
	// the reason goes with the candidate on erasure
	s.want(s.do(adminTok, "POST", "/v1/candidates/"+s.c1+"/erasure", nil), 200)
	if _, err := st.GetApplication(ctx, s.principal(adminTok), s.a1); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(s.rawDBBytes()), "secret-reason") {
		t.Error("the move reason survived erasure")
	}
}
