package obssql

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/obs"
	_ "modernc.org/sqlite"
)

func memDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestWatchSQLReadinessAndMigrations(t *testing.T) {
	db := memDB(t)
	r, hl := obs.NewRegistry(), obs.NewHealth()
	hl.SetTTL(0)
	WatchSQL(r, hl, db)
	if rep := hl.Check(context.Background()); rep.Status != obs.StatusDown {
		t.Fatalf("no schema_migrations table must not be ready: %+v", rep)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if rep := hl.Check(context.Background()); rep.Status != obs.StatusDown {
		t.Fatalf("zero migrations applied must not be ready: %+v", rep)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations VALUES (1, 0)`); err != nil {
		t.Fatal(err)
	}
	rep := hl.Check(context.Background())
	if rep.Status != obs.StatusOK || len(rep.Components) != 2 {
		t.Fatalf("want ok with database+migrations: %+v", rep)
	}
	var sb strings.Builder
	_, _ = r.WriteTo(&sb)
	out := sb.String()
	for _, want := range []string{"can_db_connections_open ", "can_db_connections_in_use ", "can_db_wait_total ", "can_db_wait_seconds_total "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	_ = db.Close()
	if rep := hl.Check(context.Background()); rep.Status != obs.StatusDown {
		t.Fatalf("closed db must be down: %+v", rep)
	}
}
