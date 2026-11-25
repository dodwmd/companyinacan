package forms

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "forms.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newLead(form, email string, at time.Time) *Lead {
	return &Lead{FormID: form, Email: email, Name: "N", Fields: map[string]string{"email": email, "message": "hello"}, CreatedAt: at, RetentionUntil: at.Add(24 * time.Hour)}
}

func TestStoreInsertGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	l := newLead("contact", "a@example.com", at)
	if err := s.Insert(ctx, l); err != nil {
		t.Fatal(err)
	}
	if l.ID == "" || l.Seq == 0 || l.Status != "new" {
		t.Fatalf("insert must fill id/seq/status: %+v", l)
	}
	got, err := s.Get(ctx, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "a@example.com" || got.Fields["message"] != "hello" || !got.CreatedAt.Equal(at) || !got.RetentionUntil.Equal(at.Add(24*time.Hour)) {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if _, err := s.Get(ctx, "ld_missing"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStoreListFiltersAndPaging(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		form := "contact"
		if i%2 == 1 {
			form = "quote"
		}
		if err := s.Insert(ctx, newLead(form, "a@example.com", base.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.List(ctx, ListFilter{Limit: 2})
	if err != nil || len(all) != 2 || all[0].Seq < all[1].Seq {
		t.Fatalf("newest first expected: %v %+v", err, all)
	}
	next, _ := s.List(ctx, ListFilter{Limit: 10, BeforeSeq: all[1].Seq})
	if len(next) != 3 {
		t.Fatalf("paging: got %d", len(next))
	}
	q, _ := s.List(ctx, ListFilter{FormID: "quote", Limit: 10})
	if len(q) != 2 {
		t.Fatalf("form filter: %d", len(q))
	}
	if _, err := s.Update(ctx, q[0].ID, "contacted", nil); err != nil {
		t.Fatal(err)
	}
	c, _ := s.List(ctx, ListFilter{Status: "contacted", Limit: 10})
	if len(c) != 1 || c[0].ID != q[0].ID {
		t.Fatalf("status filter: %+v", c)
	}
	since, _ := s.List(ctx, ListFilter{Since: base.Add(3 * time.Hour), Limit: 10})
	if len(since) != 2 {
		t.Fatalf("since filter: %d", len(since))
	}
}

func TestStoreUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	l := newLead("contact", "a@example.com", time.Now())
	_ = s.Insert(ctx, l)
	note := "called them"
	got, err := s.Update(ctx, l.ID, "qualified", &note)
	if err != nil || got.Status != "qualified" || got.Note != note || got.UpdatedAt.IsZero() {
		t.Fatalf("%v %+v", err, got)
	}
	got, err = s.Update(ctx, l.ID, "", &note) // note only
	if err != nil || got.Status != "qualified" {
		t.Fatalf("empty status must keep existing: %v %+v", err, got)
	}
	if _, err := s.Update(ctx, l.ID, "bogus", nil); err != ErrInvalidStatus {
		t.Fatalf("want ErrInvalidStatus, got %v", err)
	}
	if _, err := s.Update(ctx, "ld_nope", "new", nil); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStorePurgeExpired(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := newLead("contact", "old@example.com", at)
	fresh := newLead("contact", "new@example.com", at.Add(100*time.Hour))
	_ = s.Insert(ctx, old)
	_ = s.Insert(ctx, fresh)
	n, err := s.PurgeExpired(ctx, at.Add(48*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := s.Get(ctx, old.ID); err != ErrNotFound {
		t.Fatal("expired lead must be gone")
	}
	if _, err := s.Get(ctx, fresh.ID); err != nil {
		t.Fatal("fresh lead must remain")
	}
}

func TestMigrationsIdempotentAndVersioned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.SchemaVersion(context.Background())
	if err != nil || v1 < 1 {
		t.Fatalf("version %d err %v", v1, err)
	}
	_ = s.Insert(context.Background(), newLead("c", "a@example.com", time.Now()))
	_ = s.Close()
	s, err = OpenStore(path) // reopen: must not re-run or fail
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v2, _ := s.SchemaVersion(context.Background()); v2 != v1 {
		t.Fatalf("version changed %d -> %d", v1, v2)
	}
	if l, _ := s.List(context.Background(), ListFilter{Limit: 5}); len(l) != 1 {
		t.Fatal("data lost across reopen")
	}
}

func TestStoreIndexesExist(t *testing.T) {
	s := openTestStore(t)
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='leads'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		have[n] = true
	}
	for _, want := range []string{"leads_form_seq", "leads_status_seq", "leads_retention"} {
		if !have[want] {
			t.Errorf("missing index %s (have %v)", want, have)
		}
	}
}

func TestValidStatus(t *testing.T) {
	for _, st := range []string{"new", "contacted", "qualified", "closed", "spam"} {
		if !ValidStatus(st) {
			t.Errorf("%s should be valid", st)
		}
	}
	if ValidStatus("x") || ValidStatus("") {
		t.Error("invalid accepted")
	}
}
