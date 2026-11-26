package finance

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// seedErasable creates one party with something of every kind a party erasure treats differently,
// and a second party that must never be touched.
func seedErasable(e *env) (gone, kept int64) {
	maker := e.token("maker", rw, nil, nil, false)
	boss := e.token("boss", app, nil, nil, false)
	gone, kept = e.party(maker, "Erase Me Fake", "customer"), e.party(maker, "Keep Me Fake", "customer")
	for _, p := range []int64{gone, kept} {
		issued := e.invoice(maker, p, nil, line("x", 1000, 100))
		e.must(200, boss, "POST", "/v1/invoices/"+id(issued.ID)+"/approve", nil, nil)
		e.invoice(maker, p, nil, line("draft", 1000, 100))
	}
	return gone, kept
}

// snapshot copies the database as a backup would hold it, and opens the copy as a restore does.
func snapshot(e *env) *Store {
	e.t.Helper()
	path := filepath.Join(e.t.TempDir(), "copy.db")
	if _, err := e.app.store.db.Exec(`VACUUM INTO ?`, path); err != nil {
		e.t.Fatal(err)
	}
	st, err := OpenStore(path)
	if err != nil {
		e.t.Fatal(err)
	}
	st.now = e.app.store.now
	e.t.Cleanup(func() { _ = st.Close() })
	return st
}

// rows is every row of the tables a party erasure touches.
func rows(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var out []string
	for _, table := range []string{"parties", "invoices", "invoice_lines", "payments", "contracts", "contract_versions", "recurring", "purchase_orders", "erasures"} {
		rs, err := db.Query(`SELECT * FROM ` + table + ` ORDER BY 1, 2`) //nolint:gosec // test
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rs.Columns()
		for rs.Next() {
			vals, ptrs := make([]any, len(cols)), make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rs.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s %v", table, vals))
		}
		if err := rs.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestReplayDoesWhatTheLiveErasureDid(t *testing.T) {
	e := newEnv(t)
	gone, kept := seedErasable(e)
	restored := snapshot(e) // the backup, taken before the erasure
	e.advance(time.Hour)

	var live ErasureReport
	e.must(200, adminTok, "POST", "/v1/parties/"+id(gone)+"/erase", nil, &live)
	var feed struct{ Items []ErasureEntry }
	e.must(200, adminTok, "GET", "/v1/erasures", nil, &feed)
	if len(feed.Items) != 1 || feed.Items[0].Class != ClassPartyContact || feed.Items[0].Subject != live.Subject ||
		!erasureSubjectRe.MatchString(live.Subject) || feed.Items[0].At == "" {
		t.Fatalf("feed %+v report %+v", feed, live)
	}

	replayed, err := restored.PurgeSubject(t.Context(), live.Subject, ClassPartyContact, 7)
	if err != nil || replayed == nil {
		t.Fatalf("replay: %v %v", replayed, err)
	}
	if replayed.DeletedDrafts != live.DeletedDrafts || !slices.Equal(replayed.Retained, live.Retained) || len(live.Retained) == 0 {
		t.Fatalf("legal retention differs: live %+v replay %+v", live, replayed)
	}
	if got, want := rows(t, restored.db), rows(t, e.app.store.db); !slices.Equal(got, want) {
		t.Fatalf("the replay left other data than the live erasure:\n%s\n---\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var drafts int
	if err := restored.db.QueryRow(`SELECT COUNT(*) FROM invoices WHERE party_id = ? AND status = 'draft'`, kept).Scan(&drafts); err != nil || drafts != 1 {
		t.Fatalf("the other party's draft: %d %v", drafts, err)
	}

	// Again, later: nothing more is removed and the tombstone keeps its time.
	before := rows(t, restored.db)
	e.advance(time.Hour)
	if again, err := restored.PurgeSubject(t.Context(), live.Subject, ClassPartyContact, 7); err != nil || again.DeletedDrafts != 0 || !again.AlreadyErased {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	if !slices.Equal(before, rows(t, restored.db)) {
		t.Fatal("a repeat changed data")
	}
}

func TestReplayLeavesOtherData(t *testing.T) {
	e := newEnv(t)
	gone, _ := seedErasable(e)
	var created int64
	if err := e.app.store.db.QueryRow(`SELECT created_at FROM parties WHERE id = ?`, gone).Scan(&created); err != nil {
		t.Fatal(err)
	}
	before := rows(t, e.app.store.db)
	ctx := t.Context()
	for name, subject := range map[string]string{
		"the same id given to a later party": erasureSubject(gone, time.Unix(created-5, 0)),
		"a party not in this database":       erasureSubject(999, time.Unix(created, 0)),
	} {
		rep, err := e.app.store.PurgeSubject(ctx, subject, ClassPartyContact, 7)
		if err != nil || rep != nil {
			t.Errorf("%s: %+v %v", name, rep, err)
		}
	}
	real := erasureSubject(gone, time.Unix(created, 0))
	for _, class := range []string{"finance-invoice", "finance-bank", "all", ""} {
		if _, err := e.app.store.PurgeSubject(ctx, real, class, 7); err == nil {
			t.Errorf("class %q was accepted", class)
		}
	}
	for _, subject := range []string{"", id(gone), "party-1", "party-1-2 OR 1", "Erase Me Fake"} {
		if _, err := e.app.store.PurgeSubject(ctx, subject, ClassPartyContact, 7); err == nil {
			t.Errorf("subject %q was accepted", subject)
		}
	}
	if !slices.Equal(before, rows(t, e.app.store.db)) {
		t.Fatal("a refused or unmatched replay changed data")
	}
}

func TestErasureFeedAccessAndBackfill(t *testing.T) {
	e := newEnv(t)
	gone, kept := seedErasable(e)
	e.must(200, adminTok, "POST", "/v1/parties/"+id(gone)+"/erase", nil, nil)
	e.must(403, e.token("writer", rw, nil, nil, false), "GET", "/v1/erasures", nil, nil)
	e.must(403, e.token("scoped-admin", []string{PermAdmin}, []int64{kept}, nil, false), "GET", "/v1/erasures", nil, nil)
	e.must(401, "", "GET", "/v1/erasures", nil, nil)
	e.must(400, adminTok, "GET", "/v1/erasures?after=-1", nil, nil)
	var first, again, rest struct{ Items []ErasureEntry }
	e.must(200, adminTok, "GET", "/v1/erasures", nil, &first)
	e.advance(time.Hour)
	e.must(200, adminTok, "POST", "/v1/parties/"+id(gone)+"/erase", nil, nil)
	e.must(200, adminTok, "GET", "/v1/erasures", nil, &again)
	if !slices.Equal(first.Items, again.Items) {
		t.Fatalf("a repeat that erased nothing moved the tombstone: %+v %+v", first, again)
	}
	e.must(200, adminTok, "GET", "/v1/erasures?after="+id(first.Items[0].Seq), nil, &rest)
	if len(rest.Items) != 0 {
		t.Fatalf("cursor %+v", rest)
	}

	// A party erased by an earlier release gets its tombstone from the migration.
	sqlText, err := migrationFS.ReadFile("migrations/007_erasures.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.store.db.Exec(`DROP TABLE erasures`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.store.db.Exec(string(sqlText)); err != nil {
		t.Fatal(err)
	}
	backfilled, err := e.app.store.ListErasures(context.Background(), 0)
	if err != nil || len(backfilled) != 1 || backfilled[0].Subject != first.Items[0].Subject {
		t.Fatalf("backfill %+v %v", backfilled, err)
	}
}

func TestPurgeCommand(t *testing.T) {
	e := newEnv(t)
	gone, _ := seedErasable(e)
	st := snapshot(e)
	var rep ErasureReport
	e.must(200, adminTok, "POST", "/v1/parties/"+id(gone)+"/erase", nil, &rep)
	var path string
	if err := st.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "finance.yaml")
	if err := os.WriteFile(cfg, []byte("db_path: "+path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CAN_FINANCE_CONFIG": cfg, "CAN_FINANCE_ADMIN_TOKEN": adminTok, "CAN_FINANCE_LINK_SECRET": linkSec}
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Main(args, func(k string) string { return env[k] }, &out, &errOut)
		return code, out.String() + errOut.String()
	}
	// The command can-core runs in a Job (core/server/dumps RunPurgeCLI).
	if code, out := run("purge", "-subject", rep.Subject, "-class", ClassPartyContact); code != 0 || !strings.Contains(out, "purged") {
		t.Fatalf("%d %s", code, out)
	}
	var erased int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM parties WHERE id = ? AND erased_at IS NOT NULL AND email = ''`, gone).Scan(&erased); err != nil || erased != 1 {
		t.Fatalf("not erased: %d %v", erased, err)
	}
	if code, _ := run("purge", "-subject", rep.Subject, "-class", "finance-invoice"); code != 1 {
		t.Fatalf("class: %d", code)
	}
	if code, _ := run("purge"); code != 2 {
		t.Fatalf("no flags: %d", code)
	}
}
