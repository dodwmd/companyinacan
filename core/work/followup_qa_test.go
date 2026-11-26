package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Quoting a person term in a saved filter does not hide the name from an erasure, or from its
// replay on a restore.
func TestErasureFindsQuotedPersonTerms(t *testing.T) {
	queries := []string{`"assignee:ann"`, `"-assignee:ann"`, `-"assignee:ann"`, `assignee:"ann,bob"`, `ass"ignee:ann"`, `-Reporter="ann"`}
	for _, q := range queries {
		if got := filterPeople(q); !slices.Contains(got, "ann") {
			t.Errorf("%s: %v", q, got)
		}
	}
	for _, q := range []string{`"ann"`, `"text:assignee:ann"`, `assignee:me`, `"a note about assignee:ann"`, `label:ann`} {
		if got := filterPeople(q); len(got) != 0 {
			t.Errorf("%s names nobody: %v", q, got)
		}
	}

	s, _ := newTestStore(t)
	ctx := context.Background()
	for i, q := range queries[:3] {
		if _, err := s.CreateFilter(ctx, person("owner"), NewFilter{Name: fmt.Sprint(i), Query: q, Shared: true}); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// The name is left only in the filters: the replay has to find it there.
	restored := copyStore(t, s)
	root := &Principal{Subject: "root", Permissions: []string{PermAdmin}}
	count := func(st *Store) (n int) {
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM saved_filters`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := s.ErasePerson(ctx, root, "ann"); err != nil || count(s) != 0 {
		t.Fatalf("%v, %d filters left", err, count(s))
	}
	if res, err := restored.PurgeSubject(ctx, erasureSubject("ann"), ClassPerson, time.Time{}); err != nil || res == nil || count(restored) != 0 {
		t.Fatalf("replay: %+v %v, %d filters left", res, err, count(restored))
	}
}

func TestSharedFiltersDoNotCrowdOutYourOwn(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for i := range 500 {
		if _, err := s.db.Exec(`INSERT INTO saved_filters (owner, name, shared, created_at) VALUES ('other', ?, 1, 0)`, fmt.Sprintf("a%03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	mine, err := s.CreateFilter(ctx, alice, NewFilter{Name: "zzz mine", Query: "is:open"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ListFilters(ctx, alice)
	if err != nil || len(got) != 500 || got[0].ID != mine.ID || got[1].Name != "a000" {
		t.Fatalf("%v: %d filters, first %+v", err, len(got), got[:min(2, len(got))])
	}
}

func TestArchivedProjectSettingsAndMembersAreFrozen(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	if err := s.SetMember(ctx, admin, "ACME", "ann", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProject(ctx, admin, "ACME", ProjectPatch{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	name, wf := "renamed", DefaultWorkflow()
	for what, patch := range map[string]ProjectPatch{
		"name": {Name: &name}, "description": {Description: &name}, "sla": {SLA: DefaultSLA()}, "workflow": {Workflow: &wf},
		"name while archiving again": {Name: &name, Archived: ptr(true)},
	} {
		if _, err := s.UpdateProject(ctx, admin, "ACME", patch); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: %v", what, err)
		}
	}
	for _, on := range []bool{true, false} {
		subject := map[bool]string{true: "bob", false: "ann"}[on]
		if err := s.SetMember(ctx, admin, "ACME", subject, on); !errors.Is(err, ErrConflict) {
			t.Errorf("member %v: %v", on, err)
		}
	}
	if m, err := s.ListMembers(ctx, admin, "ACME"); err != nil || len(m) != 1 || m[0].Subject != "ann" {
		t.Fatalf("members %+v %v", m, err)
	}
	if _, err := s.UpdateProject(ctx, admin, "ACME", ProjectPatch{Archived: ptr(true)}); err != nil {
		t.Errorf("archiving again: %v", err)
	}
	p, err := s.UpdateProject(ctx, admin, "ACME", ProjectPatch{Archived: ptr(false), Name: &name})
	if err != nil || p.Archived || p.Name != name {
		t.Fatalf("restoring: %+v %v", p, err)
	}
	if err := s.SetMember(ctx, admin, "ACME", "bob", true); err != nil {
		t.Fatal(err)
	}
}

// Migration 005 dates the current priority of existing issues from the activity history: the last
// change of the priority, else the creation.
func TestPrioritySinceBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`)
	names, err := fs.Glob(migrationFS, "migrations/00[1-4]_*.sql")
	if err != nil || len(names) != 4 {
		t.Fatalf("%v %v", names, err)
	}
	for i, name := range names {
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
		exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, 0)`, i+1)
	}
	exec(`INSERT INTO projects (id, key, name, description, workflow, sla, created_at, updated_at) VALUES (1, 'ACME', 'a', '', '{}', '{}', 0, 0)`)
	for i := 1; i <= 3; i++ {
		exec(`INSERT INTO issues (id, project_id, number, type, title, description, priority, status, reporter, rank, created_at, updated_at)
			VALUES (?, 1, ?, 'task', 't', '', 'P1', 'todo', 'r', ?, 100, 900)`, i, i, fmt.Sprint(i))
	}
	for _, a := range []struct {
		issue      int
		kind, data string
		at         int
	}{
		{1, "updated", `{"priority":{"from":"P3","to":"P2"}}`, 200},
		{1, "updated", `{"priority":{"from":"P2","to":"P1"}}`, 300},
		{1, "updated", `{"title":{"from":"a","to":"b"}}`, 400}, // not a priority change
		{2, "updated", `{"title":{"from":"a","to":"b"}}`, 500},
		{2, "commented", `{"priority":"mentioned"}`, 600},
	} {
		exec(`INSERT INTO activity (issue_id, actor, kind, data, at) VALUES (?, 'r', ?, ?, ?)`, a.issue, a.kind, a.data, a.at)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for id, want := range map[int]int64{1: 300, 2: 100, 3: 100} {
		var got int64
		if err := s.db.QueryRow(`SELECT priority_since FROM issues WHERE id = ?`, id).Scan(&got); err != nil || got != want {
			t.Errorf("issue %d: priority_since %d, want %d (%v)", id, got, want, err)
		}
	}
}
