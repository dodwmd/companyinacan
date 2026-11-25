package jobs

import (
	"bytes"
	"os"
	"testing"
)

// Migration 003 drops the per-response diversity table. The store must then compact the file and
// truncate the write-ahead log so the old rows are not left behind.
func TestMigration003ScrubsDroppedDiversityRows(t *testing.T) {
	e := newEnv(t, nil)
	st := e.app.store
	for _, q := range []string{
		`ALTER TABLE interviewers DROP COLUMN removed_at`,
		`DROP TABLE eeo_counts`,
		`CREATE TABLE eeo_responses (id INTEGER PRIMARY KEY AUTOINCREMENT, gender TEXT)`,
		`INSERT INTO eeo_responses (gender) VALUES ('OLD-DIVERSITY-MARKER-ROW')`,
		`ALTER TABLE stage_history DROP COLUMN note_sealed`,
		`DELETE FROM schema_migrations WHERE version >= 3`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := st.migrate(ctxBG()); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'eeo_responses'`).Scan(&n)
	if n != 0 {
		t.Error("eeo_responses survived the migration")
	}
	if fi, err := os.Stat(e.dbPath + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("write-ahead log not truncated: %d bytes", fi.Size())
	}
	for _, f := range []string{e.dbPath, e.dbPath + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte("OLD-DIVERSITY-MARKER-ROW")) {
			t.Errorf("%s still holds old diversity rows", f)
		}
	}
}
