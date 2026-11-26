package files

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// The `summary` capability (specs/053-business-overview): storage in use and files whose retention
// date ends soon. A number of MiB and a count only: no space or file is named. An administrator
// (who already lists every file's size and retention date by id) gets the figures of everything;
// anyone else gets those of the spaces they manage, the role the file view shows retention dates
// to. The read is audited.

// retentionHorizonDays is how far ahead the end of a retention date is reported.
const retentionHorizonDays = 30

// SummaryCounts are one principal's figures.
type SummaryCounts struct {
	// All says the figures cover every space rather than the ones the principal manages.
	All             bool
	Bytes           int64
	RetentionEnding int64
}

// Summary computes the figures; nil when there is none to give (someone who is no administrator and
// manages no space gets none, rather than a zero).
func (s *Store) Summary(ctx context.Context, p *Principal) (*SummaryCounts, error) {
	if !p.Has(PermRead) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	out := &SummaryCounts{All: p.Has(PermAdmin)}
	scope, args := "(1)", []any{}
	if !out.All {
		keys := p.memberKeys()
		args = anyArgs(keys)
		managed := `SELECT space_id FROM space_members WHERE (role = 'manager') AND (principal IN (` + placeholders(len(keys)) + `))`
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+managed+`)`, args...).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, nil
		}
		scope = `(f.space_id IN (` + managed + `))`
	}
	if err := s.auditRead(ctx, p.Subject, "summary.read", "summary"); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(v.size), 0) FROM versions v JOIN files f ON (f.id = v.file_id) WHERE (v.purged_at IS NULL) AND `+scope, args...).Scan(&out.Bytes); err != nil { //nolint:gosec // fixed clauses
		return nil, err
	}
	now := s.now()
	window := append([]any{ts(now), ts(now.AddDate(0, 0, retentionHorizonDays))}, args...)
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files f WHERE (f.status IN ('active','deleted')) AND (f.retain_until > ?) AND (f.retain_until <= ?) AND `+scope, window...).Scan(&out.RetentionEnding); err != nil { //nolint:gosec // fixed clauses
		return nil, err
	}
	return out, nil
}

// summaryBuilder answers for a person as can-core describes them, with the same mapping as their
// personal access tokens and sessions. groups may be nil (only direct grants then, as everywhere).
func summaryBuilder(s *Store, groups GroupResolver) modclient.SummaryBuilder {
	return func(ctx context.Context, subject string, permissions []string) ([]modclient.SummaryFigure, error) {
		p, ok := personalPrincipal(subject, permissions)
		if !ok {
			return nil, nil
		}
		if groups != nil {
			gs, err := groups.Groups(ctx, p.Subject)
			if err != nil {
				return nil, err
			}
			p.Groups = gs
		}
		st, err := s.Summary(ctx, p)
		if err != nil || st == nil {
			return nil, err
		}
		key := "storage.managed"
		if st.All {
			key = "storage.company"
		}
		// Whole MiB, rounded up: something stored never reads as nothing.
		return []modclient.SummaryFigure{{Key: key, Count: (st.Bytes + 1<<20 - 1) >> 20}, {Key: "files.retention_ending", Count: st.RetentionEnding}}, nil
	}
}

// withSummary adds GET /v1/summary to what the listener serves. api.Handler has been called before
// (newHandler does), so the API's limiter on failed authentications exists.
func withSummary(next http.Handler, can *modclient.Client, api *API, log *slog.Logger) http.Handler {
	limit := modclient.SummaryLimit{
		Client: func(r *http.Request) string { return clientIP(r, api.TrustProxy) },
		Failed: func(ip string) bool { api.authFails.fail(ip); return !api.authFails.blocked(ip) },
	}
	mux := http.NewServeMux()
	mux.Handle(modclient.SummaryPath, modclient.SummaryHandler(can, log, limit, summaryBuilder(api.Store, api.Groups)))
	mux.Handle("/", next)
	return mux
}
