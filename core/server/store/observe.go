package store

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"

	"github.com/dodwmd/companyinacan/core/obs"
)

var observer atomic.Pointer[obs.DBObserver]

// SetObserver attaches (or, with nil, detaches) the statement timing hook. It is process-wide:
// can-core runs one state database.
func SetObserver(o *obs.DBObserver) { observer.Store(o) }

func observe(op string, start time.Time, err error) {
	if o := observer.Load(); o != nil {
		o.Observe(op, start, err)
	}
}

// ExecContext shadows sql.DB.ExecContext to record the statement's duration and outcome.
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	start := time.Now()
	res, err := db.DB.ExecContext(ctx, query, args...)
	observe("exec", start, err)
	return res, err
}

// QueryContext shadows sql.DB.QueryContext. The duration is the time until the rows are available,
// not the time to iterate them.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	start := time.Now()
	rows, err := db.DB.QueryContext(ctx, query, args...)
	observe("query", start, err)
	return rows, err
}
