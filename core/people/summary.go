package people

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// The `summary` capability (specs/053-business-overview): headcount and how many people are off
// today. Both are computed with the rules of the directory and of the "who is out" calendar for the
// person asking, so they never count anything that person could not see by name there: someone on
// leave stays masked as active to colleagues, and who is off is limited to the leave calendar's
// scope (everyone for hr and admin, otherwise one's own team). Counts only: no name, no leave type.

// SummaryStats are one actor's figures; a nil figure is one the actor may not have.
type SummaryStats struct {
	Headcount, OffToday *int
}

// Summary computes the figures for the actor.
func (s *Service) Summary(ctx context.Context, a Actor) (SummaryStats, error) {
	var st SummaryStats
	// Without a person record and without hr there is nothing to look at: no figure rather than a
	// zero that reads as "nobody works here" or "everyone is in".
	if a.blocked() || (!a.HasPerson && !a.hrAdmin()) {
		return st, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, status FROM people WHERE erased_at IS NULL ORDER BY seq`)
	if err != nil {
		return st, err
	}
	type rec struct {
		id     string
		status Status
	}
	var all []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.status); err != nil {
			_ = rows.Close()
			return st, err
		}
		all = append(all, r)
	}
	if err := rows.Close(); err != nil {
		return st, err
	}
	n := 0
	for _, r := range all {
		// Employed: not a candidate, not still onboarding, not gone.
		if r.status != StActive && r.status != StLeave && r.status != StOffboarding {
			continue
		}
		f, _, err := s.factFor(ctx, s.db, a, r.id)
		if err != nil {
			return st, err
		}
		if Decide(a, ActDirRead, f) {
			n++
		}
	}
	st.Headcount = &n

	if !Decide(a, ActLeaveCalendar, Fact{}) {
		return st, nil
	}
	today := s.today()
	// The entries whose type the calendar would show are not audited: only person ids are counted.
	entries, _, err := s.leaveCalendar(ctx, a, today, today, "all")
	if err != nil {
		return st, err
	}
	off := map[string]bool{}
	for _, e := range entries {
		if e.Status != "pending" { // approved; colleagues' entries carry no status and are approved
			off[e.PersonID] = true
		}
	}
	m := len(off)
	st.OffToday = &m
	return st, nil
}

// summaryBuilder answers for a person as can-core describes them, with the same mapping to roles as
// their personal access tokens and sessions.
func summaryBuilder(s *Service) modclient.SummaryBuilder {
	return func(ctx context.Context, subject string, permissions []string) ([]modclient.SummaryFigure, error) {
		p := principalFromPermissions(subject, permissions)
		if p == nil {
			return nil, nil
		}
		a, err := s.ResolveActor(ctx, p)
		if err != nil {
			return nil, err
		}
		st, err := s.Summary(ctx, a)
		if err != nil {
			return nil, err
		}
		var out []modclient.SummaryFigure
		if st.Headcount != nil {
			out = append(out, modclient.SummaryFigure{Key: "people.headcount", Count: int64(*st.Headcount)})
		}
		if st.OffToday != nil {
			out = append(out, modclient.SummaryFigure{Key: "people.off_today", Count: int64(*st.OffToday)})
		}
		return out, nil
	}
}

// withSummary adds GET /v1/summary to the handler of an installed module.
func withSummary(next http.Handler, can *modclient.Client, s *Service, log *slog.Logger, limit modclient.SummaryLimit) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(modclient.SummaryPath, modclient.SummaryHandler(can, log, limit, summaryBuilder(s)))
	mux.Handle("/", next)
	return mux
}

// summaryLimit counts failures on /v1/summary like every other failed authentication.
func (a *API) summaryLimit() modclient.SummaryLimit {
	return modclient.SummaryLimit{
		Client: func(r *http.Request) string { return clientIP(r, a.TrustProxy) },
		Failed: func(ip string) bool { a.limiters(); return a.failIP.allow(ip) },
	}
}
