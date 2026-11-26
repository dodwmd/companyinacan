package board

import "testing"

func TestSummaryCountsFilingsForTheSecretaryOnly(t *testing.T) {
	e := newEnv(t).base() // today is 2026-03-10
	le := e.entity()
	for _, due := range []string{"2026-02-28", "2026-03-10", "2026-04-09", "2026-06-30"} {
		m(e.s.CreateObligation(e.ctx, e.sec, le.ID, ObligationInput{Name: "Return", FirstDueOn: due, Recurrence: "once"}))
	}
	overdue, upcoming, err := e.s.FilingCounts(e.ctx, e.sec)
	e.must(err)
	if overdue != 1 || upcoming != 2 {
		t.Fatalf("overdue %d upcoming %d", overdue, upcoming)
	}
	for _, a := range []Actor{e.adm, e.emp, e.obs, e.d1, e.integ} {
		if _, _, err := e.s.FilingCounts(e.ctx, a); err == nil {
			t.Errorf("%s got the counts", a.Subject)
		}
	}
	build := summaryBuilder(e.s)
	for _, perms := range [][]string{nil, {"entity.read"}, {"entity.write"}, {"entity.read", "entity.admin"}, {"entity.read", "entity.attest", "entity.sensitive-read"}} {
		if f, err := build(e.ctx, "user:alice", perms); err != nil || len(f) != 0 {
			t.Errorf("%v: %v %v", perms, f, err)
		}
	}
	f, err := build(e.ctx, "user:alice", []string{"entity.read", "entity.write", "entity.sensitive-read"})
	e.must(err)
	if len(f) != 2 || f[0].Count != 1 || f[1].Count != 2 {
		t.Fatalf("%+v", f)
	}
}
