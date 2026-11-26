package forms

import (
	"context"
	"log/slog"
	"net/http"
	"slices"

	"github.com/dodwmd/companyinacan/core/modclient"
)

// The `summary` capability (specs/053-business-overview): how many leads are still new. A count
// only: no name, address or submitted value leaves the module.

// summaryFailuresPerMin is how many failed requests to /v1/summary an address may make a minute.
const summaryFailuresPerMin = 30

// NewLeads counts the leads nobody has picked up.
func (s *Store) NewLeads(ctx context.Context) (n int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM leads WHERE status = 'new'`).Scan(&n)
	return n, err
}

func summaryBuilder(store *Store) modclient.SummaryBuilder {
	return func(ctx context.Context, _ string, permissions []string) ([]modclient.SummaryFigure, error) {
		if !slices.Contains(permissions, PermLeadsRead) {
			return nil, nil
		}
		n, err := store.NewLeads(ctx)
		if err != nil {
			return nil, err
		}
		return []modclient.SummaryFigure{{Key: "leads.new", Count: n}}, nil
	}
}

// withSummary adds GET /v1/summary to the API of an installed module.
func withSummary(next http.Handler, can *modclient.Client, store *Store, log *slog.Logger, limit modclient.SummaryLimit) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(modclient.SummaryPath, modclient.SummaryHandler(can, log, limit, summaryBuilder(store)))
	mux.Handle("/", next)
	return mux
}
