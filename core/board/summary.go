package board

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// The `summary` capability of the entity module (specs/053-business-overview): how many filings are
// overdue and how many fall due soon. Counts only: no entity, filing or authority is named. This
// is the only thing this service asks can-core about a person; its API still takes its own tokens.

// filingHorizonDays is how far ahead a filing is "upcoming".
const filingHorizonDays = 30

// FilingCounts counts the pending filings of active obligations that are overdue and that fall due
// within the next 30 days. It answers only to someone who may read the registers of every entity.
func (s *Service) FilingCounts(ctx context.Context, a Actor) (overdue, upcoming int64, err error) {
	if !Decide(a, ActRegisterRead, Fact{}) {
		return 0, 0, ErrNotFound
	}
	ctx, cancel := readCtx(ctx)
	defer cancel()
	today := s.today()
	t, err := time.Parse(dateLayout, today)
	if err != nil {
		return 0, 0, err
	}
	until := t.AddDate(0, 0, filingHorizonDays).Format(dateLayout)
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(f.due_on < ?), 0), COALESCE(SUM(f.due_on >= ? AND f.due_on <= ?), 0)
		FROM filings f JOIN filing_obligations o ON o.id = f.obligation_id WHERE f.status = 'pending' AND o.active = 1`, today, today, until).Scan(&overdue, &upcoming)
	if err != nil {
		return 0, 0, err
	}
	if err := s.auditRead(ctx, a, "legal-entities/filing-counts", nil); err != nil {
		return 0, 0, err
	}
	return overdue, upcoming, nil
}

// summaryBuilder maps can-core's permissions to this module's roles for the one thing it answers:
// entity.write is held by the secretary alone among the manifest roles, who runs the filings
// calendar. Officers and directors share one set of permissions while only officers see every
// entity, so neither gets a figure.
func summaryBuilder(s *Service) modclient.SummaryBuilder {
	return func(ctx context.Context, subject string, permissions []string) ([]modclient.SummaryFigure, error) {
		if !slices.Contains(permissions, "entity.read") || !slices.Contains(permissions, "entity.write") {
			return nil, nil
		}
		overdue, upcoming, err := s.FilingCounts(ctx, Actor{Subject: subject, Roles: []string{RoleSecretary}})
		if err != nil {
			return nil, err
		}
		return []modclient.SummaryFigure{{Key: "filings.overdue", Count: overdue}, {Key: "filings.upcoming", Count: upcoming}}, nil
	}
}

// withSummary adds GET /v1/summary to the handler of an installed module.
func withSummary(next http.Handler, can *modclient.Client, s *Service, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(modclient.SummaryPath, modclient.SummaryHandler(can, log, summaryBuilder(s)))
	mux.Handle("/", next)
	return mux
}
