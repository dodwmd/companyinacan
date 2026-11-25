package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// An installation upgraded from before E11a: existing admins with valid names become owners; an
// admin whose name is not a valid RBAC subject gets no role (and is not silently half-granted).
func TestMigration0002BackfillsOwnersOnlyForValidUsernames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	init1, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{string(init1), `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`, `INSERT INTO schema_migrations VALUES (1, 0)`,
		`INSERT INTO admin_users(username, password_hash, totp_secret, created_at) VALUES ('alice','h','s',1), ('bad name!','h','s',1), ('o''brien','h','s',1)`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = raw.Close()
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT subject FROM rbac_assignments WHERE role='owner' ORDER BY subject`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		got = append(got, s)
	}
	if len(got) != 1 || got[0] != "user:alice" {
		t.Fatalf("owners = %v, want only user:alice", got)
	}
}
