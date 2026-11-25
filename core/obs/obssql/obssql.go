// Package obssql wires a service's SQLite handle into readiness and metrics. It is separate from
// core/obs so binaries without a database (can-static) do not link database/sql.
package obssql

import (
	"context"
	"database/sql"
	"errors"

	"github.com/dodwmd/companyinacan/core/obs"
)

// WatchSQL wires a service's SQLite handle into readiness and metrics: the "database" check
// (reachable) and the "migrations" check (schema_migrations has at least one applied version,
// both critical), plus pool gauges read from sql.DB.Stats at scrape time. Calling it again for the
// same registry replaces the earlier registration.
func WatchSQL(reg *obs.Registry, h *obs.Health, db *sql.DB) {
	h.Add("database", true, func(ctx context.Context) error { return db.PingContext(ctx) })
	h.Add("migrations", true, func(ctx context.Context) error {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
			return err
		}
		if !v.Valid || v.Int64 < 1 {
			return errors.New("no migration applied")
		}
		return nil
	})
	reg.GaugeFunc("can_db_connections_open", "Open database connections.", func() float64 { return float64(db.Stats().OpenConnections) })
	reg.GaugeFunc("can_db_connections_in_use", "Database connections in use.", func() float64 { return float64(db.Stats().InUse) })
	reg.CounterFunc("can_db_wait_total", "Times a query waited for a free connection.", func() float64 { return float64(db.Stats().WaitCount) })
	reg.CounterFunc("can_db_wait_seconds_total", "Total time spent waiting for a free connection.", func() float64 { return db.Stats().WaitDuration.Seconds() })
}

// WatchDefault is WatchSQL on the obs.Default registry and obs.DefaultHealth.
func WatchDefault(db *sql.DB) { WatchSQL(obs.Default, obs.DefaultHealth, db) }
