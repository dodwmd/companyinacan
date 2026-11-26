package equity

import "testing"

func TestSummaryCountsPendingGrantsAndVestingDue(t *testing.T) {
	e := newEnv(t).base()
	build := summaryBuilder(e.s)
	hr := []string{"equity.read", "equity.write", "equity.sensitive-read", "equity.self-read"}
	figures := func(perms ...string) map[string]int64 {
		t.Helper()
		got, err := build(e.ctx, "user:harriet", perms)
		e.must(err)
		out := map[string]int64{}
		for _, f := range got {
			out[f.Key] = f.Count
		}
		return out
	}
	if got := figures(hr...); got["grants.pending_approval"] != 0 || got["vesting.upcoming"] != 0 || len(got) != 2 {
		t.Fatalf("empty: %v", got)
	}
	e.active(e.eveStk.ID, q(4800)) // vests from 2027-01-15 (12 month cliff), monthly to 2030-01-15
	id := e.draft(e.hr, e.founder.ID, q(100))
	m(e.s.SubmitGrant(e.ctx, e.hr, id))
	for date, vesting := range map[string]int64{
		"2026-03-10": 0, // the cliff is months away
		"2026-12-15": 0, // 31 days before it
		"2026-12-16": 1, // 30 days before it
		"2027-01-15": 1, // today counts
		"2029-12-16": 1,
		"2030-01-16": 0, // fully vested
	} {
		e.advance(date)
		if got := figures(hr...); got["grants.pending_approval"] != 1 || got["vesting.upcoming"] != vesting {
			t.Errorf("%s: %v, want 1 pending and %d vesting", date, got, vesting)
		}
	}
	// A holder, a person with only the board's permissions (as with their personal access token)
	// and no permission at all get nothing.
	for _, perms := range [][]string{{"equity.self-read"}, {"equity.read", "equity.approve", "equity.sensitive-read", "equity.admin"}, nil} {
		if got := figures(perms...); len(got) != 0 {
			t.Errorf("%v: %v", perms, got)
		}
	}
	if e.count(`SELECT COUNT(*) FROM audit_log WHERE resource = 'grants/summary-counts'`) == 0 {
		t.Error("the read is not audited")
	}
	e.s.audit = failingAuditor{}
	if _, err := build(e.ctx, "user:harriet", hr); err == nil {
		t.Error("figures without an audit record")
	}
}
