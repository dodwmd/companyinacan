package jobs

import (
	"context"
	"net/http"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// The `summary` capability (specs/053-business-overview): open requisitions, new applications and
// the caller's own interviews awaiting a scorecard. Counts only, under the same role matrix and
// scope predicates as the lists: no title, name or other candidate field is read, so (like the
// funnel counts) nothing is audited.

// SummaryCounts are one principal's figures; a nil figure is one their role may not have.
type SummaryCounts struct {
	OpenRequisitions, NewApplications, InterviewsDue *int64
}

// Summary counts what the principal may see. A new application is an active one still in the first
// pipeline stage. An interview is due when the caller is assigned to an active application and has
// not submitted a scorecard for it (interviews are not scheduled in this service: there is no date).
func (s *Store) Summary(ctx context.Context, p *Principal) (SummaryCounts, error) {
	var out SummaryCounts
	count := func(q string, args []any) (*int64, error) {
		var n int64
		if err := s.rdb.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			return nil, err
		}
		return &n, nil
	}
	var err error
	if p.Can(ActJobRead) {
		pred, args := p.jobPred("jobs")
		if out.OpenRequisitions, err = count(`SELECT COUNT(*) FROM jobs WHERE status = 'open' AND `+pred, args); err != nil {
			return out, err
		}
	}
	if p.Can(ActAppRead) {
		pred, args := p.appPred("a", "j")
		if out.NewApplications, err = count(`SELECT COUNT(*) FROM applications a JOIN jobs j ON j.id = a.job_id
			WHERE a.status = 'active' AND a.stage IN (SELECT key FROM stages WHERE kind = 'initial') AND `+pred, args); err != nil {
			return out, err
		}
	}
	if p.Can(ActScore) {
		pred, args := p.appPred("a", "j")
		if out.InterviewsDue, err = count(`SELECT COUNT(*) FROM applications a JOIN jobs j ON j.id = a.job_id
			WHERE a.status = 'active'
			AND EXISTS (SELECT 1 FROM interviewers iv WHERE iv.application_id = a.id AND iv.subject = ? AND iv.removed_at IS NULL)
			AND NOT EXISTS (SELECT 1 FROM scorecards sc WHERE sc.application_id = a.id AND sc.author = ? AND sc.status = 'submitted')
			AND `+pred, append([]any{p.Subject, p.Subject}, args...)); err != nil {
			return out, err
		}
	}
	return out, nil
}

// summaryBuilder answers for a person as can-core describes them, with the same mapping to a role
// as their personal access tokens and sessions.
func summaryBuilder(s *Store) modclient.SummaryBuilder {
	return func(ctx context.Context, subject string, permissions []string) ([]modclient.SummaryFigure, error) {
		p := personalPrincipal(subject, permissions)
		if p == nil {
			return nil, nil
		}
		st, err := s.Summary(ctx, p)
		if err != nil {
			return nil, err
		}
		var out []modclient.SummaryFigure
		for _, f := range []struct {
			key string
			n   *int64
		}{{"requisitions.open", st.OpenRequisitions}, {"applications.new", st.NewApplications}, {"interviews.due", st.InterviewsDue}} {
			if f.n != nil {
				out = append(out, modclient.SummaryFigure{Key: f.key, Count: *f.n})
			}
		}
		return out, nil
	}
}

// summaryLimit counts failures on /v1/summary like every other failed authentication.
func (a *api) summaryLimit() modclient.SummaryLimit {
	return modclient.SummaryLimit{
		Client: func(r *http.Request) string { return clientIP(r, a.trustProxy) },
		Failed: func(ip string) bool { ok, _ := a.failures.allow(ip); return ok },
	}
}
