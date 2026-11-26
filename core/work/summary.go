package work

import (
	"context"
	"log/slog"
	"net/http"
	"slices"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// The `summary` capability (specs/053-business-overview): how many P0 and P1 issues are open and
// how many of them are overdue, in the projects the person may see. Counts only.

// Summary counts the open urgent issues of unarchived projects in the principal's scope; overdue
// means a due date before today (UTC).
func (s *Store) Summary(ctx context.Context, p *Principal) (open, overdue int64, err error) {
	q := `SELECT COUNT(*), COALESCE(SUM(i.due_date IS NOT NULL AND i.due_date < ?), 0) FROM issues i JOIN projects p ON p.id = i.project_id
		WHERE i.priority IN ('P0', 'P1') AND i.resolved_at IS NULL AND p.archived = 0`
	args := []any{s.now().UTC().Format("2006-01-02")}
	if len(p.Projects) > 0 {
		q += ` AND p.key IN (` + placeholders(len(p.Projects)) + `)`
		for _, k := range p.Projects {
			args = append(args, k)
		}
	}
	err = s.rdb.QueryRowContext(ctx, q, args...).Scan(&open, &overdue)
	return open, overdue, err
}

// summaryBuilder answers for a person as can-core describes them: the permissions are limited to
// this module's, and without work.read there is nothing to see.
func summaryBuilder(s *Store) modclient.SummaryBuilder {
	return func(ctx context.Context, subject string, permissions []string) ([]modclient.SummaryFigure, error) {
		p := &Principal{Subject: subject, Kind: "personal"}
		for _, perm := range permissions {
			if (perm == PermRead || perm == PermWrite || perm == PermAdmin) && !slices.Contains(p.Permissions, perm) {
				p.Permissions = append(p.Permissions, perm)
			}
		}
		if !p.Can(PermRead) {
			return nil, nil
		}
		open, overdue, err := s.Summary(ctx, p)
		if err != nil {
			return nil, err
		}
		return []modclient.SummaryFigure{{Key: "work.urgent_overdue", Count: overdue}, {Key: "work.urgent_open", Count: open}}, nil
	}
}

// withSummary adds GET /v1/summary to the handler of an installed module.
func withSummary(next http.Handler, can *modclient.Client, store *Store, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(modclient.SummaryPath, modclient.SummaryHandler(can, log, summaryBuilder(store)))
	mux.Handle("/", next)
	return mux
}
