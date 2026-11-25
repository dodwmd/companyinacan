package obs

import (
	"strings"
	"time"
)

// DBObserver records query timings and SQLite contention.
type DBObserver struct {
	dur   *HistogramVec
	errs  *CounterVec
	busy  *Counter
	known map[string]bool
}

// NewDBObserver registers can_db_query_duration_seconds{op}, can_db_errors_total{op} and
// can_db_busy_total on reg. op is a small fixed set ("query", "queryrow", "exec", "tx"); the SQL
// text is never a label, because statements can embed identifiers.
func NewDBObserver(reg *Registry) *DBObserver {
	return &DBObserver{
		dur:   reg.Histogram("can_db_query_duration_seconds", "Database statement duration by operation.", []float64{.0005, .001, .005, .01, .05, .1, .5, 1, 5}, "op"),
		errs:  reg.Counter("can_db_errors_total", "Database statement errors by operation.", "op"),
		busy:  reg.Counter("can_db_busy_total", "Statements that failed because SQLite was busy or locked.").With(),
		known: map[string]bool{"query": true, "queryrow": true, "exec": true, "tx": true},
	}
}

// Observe records one statement that started at start and ended with err.
func (o *DBObserver) Observe(op string, start time.Time, err error) {
	if !o.known[op] {
		op = "other"
	}
	o.dur.With(op).Observe(time.Since(start).Seconds())
	if err != nil && err.Error() != "sql: no rows in result set" { // sql.ErrNoRows; compared by text to keep database/sql out of binaries that do not use it
		o.errs.With(op).Inc()
		if IsBusy(err) {
			o.busy.Inc()
		}
	}
}

// IsBusy reports whether err is SQLite's busy or locked condition.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "SQLITE_LOCKED") || strings.Contains(s, "database is locked") || strings.Contains(s, "database table is locked")
}
