package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/obs"
)

func TestObserverRecordsStatements(t *testing.T) {
	reg := obs.NewRegistry()
	db := open(t) // migrations run before the observer is attached
	SetObserver(obs.NewDBObserver(reg))
	t.Cleanup(func() { SetObserver(nil) })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (999, 0)`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO nope VALUES (1)`); err == nil {
		t.Fatal("expected error")
	}
	if err := db.InTx(ctx, func(tx *sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	_, _ = reg.WriteTo(&b)
	out := b.String()
	for _, want := range []string{
		`can_db_query_duration_seconds_count{op="exec"} 2`,
		`can_db_query_duration_seconds_count{op="query"} 1`,
		`can_db_query_duration_seconds_count{op="tx"} 1`,
		`can_db_errors_total{op="exec"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "schema_migrations") || strings.Contains(out, "nope") {
		t.Fatal("SQL text leaked into metrics")
	}
}

func TestNoObserverIsFine(t *testing.T) {
	SetObserver(nil)
	db := open(t)
	if _, err := db.ExecContext(context.Background(), `SELECT 1`); err != nil {
		t.Fatal(err)
	}
}
