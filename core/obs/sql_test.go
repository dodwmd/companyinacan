package obs

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestObserveDBClassifiesBusy(t *testing.T) {
	r := NewRegistry()
	o := NewDBObserver(r)
	o.Observe("query", time.Now().Add(-10*time.Millisecond), nil)
	o.Observe("exec", time.Now(), errors.New("database is locked (5) (SQLITE_BUSY)"))
	o.Observe("exec", time.Now(), errors.New("constraint failed: UNIQUE email=alice@example.com"))
	o.Observe("bogus-op-"+strings.Repeat("x", 50), time.Now(), nil)
	out := expose(t, r)
	for _, want := range []string{
		`can_db_query_duration_seconds_count{op="query"} 1`,
		`can_db_busy_total 1`,
		`can_db_errors_total{op="exec"} 2`,
		`can_db_query_duration_seconds_count{op="other"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "alice") || strings.Contains(out, "UNIQUE") {
		t.Fatal("error text leaked into metrics")
	}
}
