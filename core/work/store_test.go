package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var (
	admin  = &Principal{Subject: "root", Permissions: []string{PermAdmin}}
	alice  = &Principal{Subject: "alice@example.com", Permissions: []string{PermWrite}}
	acmeRW = &Principal{Subject: "bot", Permissions: []string{PermWrite}, Projects: []string{"ACME"}}
)

func newTestStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}
	s.now = clk.Now
	s.events = true
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

func mustProject(t *testing.T, s *Store, key string) {
	t.Helper()
	if _, err := s.CreateProject(context.Background(), admin, NewProject{Key: key, Name: key + " project"}); err != nil {
		t.Fatal(err)
	}
}

func mustIssue(t *testing.T, s *Store, a *Principal, project string, in CreateIssue) *Issue {
	t.Helper()
	if in.Title == "" {
		in.Title = "A thing to do"
	}
	if in.Type == "" {
		in.Type = "task"
	}
	is, _, err := s.CreateIssue(context.Background(), a, project, in, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return is
}

func isInvalid(err error) bool {
	var ie *InvalidError
	return errors.As(err, &ie)
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = OpenStore(path) // second open re-runs migrate
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v, _ := s.SchemaVersion(context.Background()); v != 3 {
		t.Fatalf("schema version %d", v)
	}
}

func TestIssueKeysAutoIncrementPerProject(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	mustProject(t, s, "OPS")
	a1 := mustIssue(t, s, alice, "ACME", CreateIssue{})
	a2 := mustIssue(t, s, alice, "ACME", CreateIssue{})
	o1 := mustIssue(t, s, alice, "OPS", CreateIssue{})
	if a1.Key != "ACME-1" || a2.Key != "ACME-2" || o1.Key != "OPS-1" {
		t.Fatalf("keys %s %s %s", a1.Key, a2.Key, o1.Key)
	}
	if a1.Status != "todo" || a1.Priority != "P2" || a1.Reporter != alice.Subject || a1.Version != 1 {
		t.Fatalf("defaults wrong: %+v", a1)
	}
	if a1.SLAHint.RespondHours == 0 || a1.StatusCategory != CategoryTodo {
		t.Fatalf("hint/category missing: %+v", a1)
	}
	if a1.Rank >= a2.Rank {
		t.Fatalf("new issues rank last: %q %q", a1.Rank, a2.Rank)
	}
}

func TestCreateValidation(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	bad := []CreateIssue{
		{Type: "task"},                             // no title
		{Type: "nope", Title: "x"},                 // bad type
		{Type: "task", Title: "x", Priority: "P9"}, // bad priority
		{Type: "task", Title: "x\ny"},              // multi-line title
		{Type: "task", Title: strings.Repeat("x", 201)},
		{Type: "task", Title: "x", DueDate: ptr("31/12/2026")},
		{Type: "task", Title: "x", Labels: []string{"Bad Label"}},
		{Type: "task", Title: "x", Parent: ptr("nope")},
		{Type: "subtask", Title: "x"}, // subtask needs a parent
		{Type: "epic", Title: "x", Parent: ptr("ACME-1")},
		{Type: "task", Title: "x", IdempotencyKey: "short"},
	}
	for i, in := range bad {
		if _, _, err := s.CreateIssue(context.Background(), alice, "ACME", in, ""); !isInvalid(err) {
			t.Errorf("case %d: want validation error, got %v", i, err)
		}
	}
	if _, _, err := s.CreateIssue(context.Background(), alice, "NOPE", CreateIssue{Type: "task", Title: "x"}, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestHierarchyRules(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	mustProject(t, s, "OPS")
	ctx := context.Background()
	epic := mustIssue(t, s, alice, "ACME", CreateIssue{Type: "epic", Title: "Launch"})
	story := mustIssue(t, s, alice, "ACME", CreateIssue{Type: "story", Title: "Signup", Parent: &epic.Key})
	sub := mustIssue(t, s, alice, "ACME", CreateIssue{Type: "subtask", Title: "Form", Parent: &story.Key})
	if story.Parent == nil || *story.Parent != epic.Key || *sub.Parent != story.Key {
		t.Fatal("parents not stored")
	}
	for name, in := range map[string]CreateIssue{
		"story under story":     {Type: "story", Title: "x", Parent: &story.Key},
		"subtask under epic":    {Type: "subtask", Title: "x", Parent: &epic.Key},
		"subtask under subtask": {Type: "subtask", Title: "x", Parent: &sub.Key},
		"parent missing":        {Type: "task", Title: "x", Parent: ptr("ACME-99")},
		"cross project":         {Type: "task", Title: "x", Parent: ptr("OPS-1")},
	} {
		if _, _, err := s.CreateIssue(ctx, alice, "ACME", in, ""); !isInvalid(err) {
			t.Errorf("%s: want validation error, got %v", name, err)
		}
	}
	// Re-parenting through update goes through the same rules.
	if _, err := s.UpdateIssue(ctx, alice, epic.Key, &Patch{Parent: Opt[string]{Set: true, V: story.Key}}, nil); !isInvalid(err) {
		t.Fatalf("epic under story: %v", err)
	}
	if _, err := s.UpdateIssue(ctx, alice, sub.Key, &Patch{Parent: Opt[string]{Set: true, Null: true}}, nil); !isInvalid(err) {
		t.Fatalf("subtask orphaned: %v", err)
	}
	d, err := s.GetIssue(ctx, alice, epic.Key)
	if err != nil || !slices.Equal(d.Children, []string{story.Key}) {
		t.Fatalf("children: %v %v", d, err)
	}
}

func TestIdempotentCreate(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	in := CreateIssue{Type: "story", Title: "Plan Q1", Priority: "P1"}
	first, replayed, err := s.CreateIssue(ctx, alice, "ACME", in, "retry-key-0001")
	if err != nil || replayed {
		t.Fatalf("first: %v %v", replayed, err)
	}
	again, replayed, err := s.CreateIssue(ctx, alice, "ACME", in, "retry-key-0001")
	if err != nil || !replayed || again.Key != first.Key {
		t.Fatalf("retry: %v %v %+v", replayed, err, again)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM issues`).Scan(&n)
	if n != 1 {
		t.Fatalf("retry created %d issues", n)
	}
	diff := in
	diff.Title = "Different"
	if _, _, err := s.CreateIssue(ctx, alice, "ACME", diff, "retry-key-0001"); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("different body, same key: %v", err)
	}
	// Keys are per caller: another subject may reuse the string.
	other, replayed, err := s.CreateIssue(ctx, admin, "ACME", diff, "retry-key-0001")
	if err != nil || replayed || other.Key == first.Key {
		t.Fatalf("other subject: %v %v", replayed, err)
	}
	// The body field works like the header.
	in.IdempotencyKey = "body-key-00001"
	a, _, _ := s.CreateIssue(ctx, alice, "ACME", in, "")
	b, replayed, _ := s.CreateIssue(ctx, alice, "ACME", in, "")
	if !replayed || a.Key != b.Key {
		t.Fatal("body idempotency_key not honoured")
	}
}

func TestFailedCreateDoesNotBurnKey(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	in := CreateIssue{Type: "task", Title: "x", Parent: ptr("ACME-77")}
	if _, _, err := s.CreateIssue(ctx, alice, "ACME", in, "key-for-retry1"); !isInvalid(err) {
		t.Fatalf("want invalid: %v", err)
	}
	in.Parent = nil
	if _, replayed, err := s.CreateIssue(ctx, alice, "ACME", in, "key-for-retry1"); err != nil || replayed {
		t.Fatalf("corrected retry: %v %v", replayed, err)
	}
}

func TestBulkCreateIsAtomicAndIdempotent(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	items := []CreateIssue{{Type: "epic", Title: "Q1"}, {Type: "story", Title: "A"}, {Type: "story", Title: "B", Priority: "P0"}}
	out, replayed, err := s.BulkCreate(ctx, alice, "ACME", items, "bulk-key-0001")
	if err != nil || replayed || len(out) != 3 || out[2].Key != "ACME-3" {
		t.Fatalf("bulk: %v %v %+v", replayed, err, out)
	}
	if _, replayed, err = s.BulkCreate(ctx, alice, "ACME", items, "bulk-key-0001"); err != nil || !replayed {
		t.Fatalf("bulk retry: %v %v", replayed, err)
	}
	bad := []CreateIssue{{Type: "task", Title: "ok"}, {Type: "task", Title: "bad", Parent: ptr("ACME-99")}}
	if _, _, err := s.BulkCreate(ctx, alice, "ACME", bad, ""); !isInvalid(err) || !strings.Contains(err.Error(), "issues[1].parent") {
		t.Fatalf("want indexed problem, got %v", err)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM issues`).Scan(&n)
	if n != 3 {
		t.Fatalf("failed bulk left %d issues behind", n)
	}
	if _, _, err := s.BulkCreate(ctx, alice, "ACME", nil, ""); !isInvalid(err) {
		t.Fatalf("empty bulk: %v", err)
	}
}

func TestBulkCreateBatchLocalParents(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	items := []CreateIssue{
		{Type: "epic", Title: "Q1"},
		{Type: "story", Title: "A", Parent: ptr("$0")},
		{Type: "subtask", Title: "A1", Parent: ptr("$1")},
	}
	out, _, err := s.BulkCreate(ctx, alice, "ACME", items, "")
	if err != nil {
		t.Fatal(err)
	}
	if *out[1].Parent != out[0].Key || *out[2].Parent != out[1].Key {
		t.Fatalf("parents not resolved: %v %v", *out[1].Parent, *out[2].Parent)
	}
	for _, bad := range [][]CreateIssue{
		{{Type: "story", Title: "A", Parent: ptr("$0")}},                             // refers to itself
		{{Type: "story", Title: "A", Parent: ptr("$1")}, {Type: "epic", Title: "E"}}, // refers forward
		{{Type: "epic", Title: "E"}, {Type: "story", Title: "A", Parent: ptr("$x")}},
	} {
		if _, _, err := s.BulkCreate(ctx, alice, "ACME", bad, ""); !isInvalid(err) {
			t.Errorf("%v: want validation error, got %v", *bad[0].Parent, err)
		}
	}
	// $N means nothing in a single create.
	if _, _, err := s.CreateIssue(ctx, alice, "ACME", CreateIssue{Type: "story", Title: "x", Parent: ptr("$0")}, ""); !isInvalid(err) {
		t.Fatalf("single create: %v", err)
	}
}

func TestUpdateOptimisticConcurrency(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	is := mustIssue(t, s, alice, "ACME", CreateIssue{})
	v1 := is.Version
	up, err := s.UpdateIssue(ctx, alice, is.Key, &Patch{Title: Opt[string]{Set: true, V: "Renamed"}}, &v1)
	if err != nil || up.Version != v1+1 || up.Title != "Renamed" {
		t.Fatalf("update: %v %+v", err, up)
	}
	if _, err := s.UpdateIssue(ctx, alice, is.Key, &Patch{Priority: Opt[string]{Set: true, V: "P0"}}, &v1); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("stale If-Match: %v", err)
	}
	stale := Opt[int64]{Set: true, V: v1}
	if _, err := s.UpdateIssue(ctx, alice, is.Key, &Patch{Priority: Opt[string]{Set: true, V: "P0"}, Version: stale}, nil); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("stale body version: %v", err)
	}
	// An update that changes nothing does not bump the version or emit an event.
	same, err := s.UpdateIssue(ctx, alice, is.Key, &Patch{Title: Opt[string]{Set: true, V: "Renamed"}}, nil)
	if err != nil || same.Version != up.Version {
		t.Fatalf("no-op update: %v %+v", err, same)
	}
}

func TestPatchSemantics(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	is := mustIssue(t, s, alice, "ACME", CreateIssue{Assignee: ptr("bob@example.com"), Labels: []string{"b", "a"}, DueDate: ptr("2026-12-01")})
	var pt Patch
	if err := json.Unmarshal([]byte(`{"assignee":null,"due_date":null,"estimate":3.5,"labels_add":["c"],"labels_remove":["a"]}`), &pt); err != nil {
		t.Fatal(err)
	}
	up, err := s.UpdateIssue(ctx, alice, is.Key, &pt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if up.Assignee != nil || up.DueDate != nil || up.Estimate == nil || *up.Estimate != 3.5 || !slices.Equal(up.Labels, []string{"b", "c"}) {
		t.Fatalf("patch result: %+v", up)
	}
	// Unknown fields are rejected by the decoder (mass assignment).
	dec := json.NewDecoder(strings.NewReader(`{"reporter":"mallory","status":"done","version":1,"key":"X-1"}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&Patch{}); err == nil {
		t.Fatal("reporter/status/key must not be patchable")
	}
	if _, err := s.UpdateIssue(ctx, alice, is.Key, &Patch{Priority: Opt[string]{Set: true, Null: true}}, nil); !isInvalid(err) {
		t.Fatalf("null priority: %v", err)
	}
}

func TestTransitionsFollowWorkflow(t *testing.T) {
	s, clk := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	is := mustIssue(t, s, alice, "ACME", CreateIssue{})
	if _, err := s.Transition(ctx, alice, is.Key, "done", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("todo->done must be refused: %v", err)
	}
	if _, err := s.Transition(ctx, alice, is.Key, "ghost", nil); !isInvalid(err) {
		t.Fatalf("unknown status: %v", err)
	}
	for _, to := range []string{"in_progress", "in_review"} {
		if is, err := s.Transition(ctx, alice, is.Key, to, nil); err != nil || is.Status != to || is.ResolvedAt != nil {
			t.Fatalf("to %s: %v %+v", to, err, is)
		}
	}
	clk.Advance(time.Hour)
	done, err := s.Transition(ctx, alice, is.Key, "done", nil)
	if err != nil || done.ResolvedAt == nil || done.StatusCategory != CategoryDone {
		t.Fatalf("done: %v %+v", err, done)
	}
	reopened, err := s.Transition(ctx, alice, is.Key, "in_progress", nil)
	if err != nil || reopened.ResolvedAt != nil {
		t.Fatalf("reopen: %v %+v", err, reopened)
	}
	stale := int64(1)
	if _, err := s.Transition(ctx, alice, is.Key, "in_review", &stale); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("stale transition: %v", err)
	}
	next, _ := s.AllowedTransitions(ctx, alice, is.Key)
	if !slices.Contains(next, "in_review") {
		t.Fatalf("allowed: %v", next)
	}
}

func TestCustomWorkflow(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	wf := Workflow{
		Statuses:    []Status{{"open", CategoryTodo}, {"qa", CategoryActive}, {"shipped", CategoryDone}},
		Initial:     "open",
		Transitions: map[string][]string{"open": {"qa"}, "qa": {"open", "shipped"}},
	}
	if _, err := s.CreateProject(ctx, admin, NewProject{Key: "REL", Name: "Releases", Workflow: &wf}); err != nil {
		t.Fatal(err)
	}
	is := mustIssue(t, s, alice, "REL", CreateIssue{})
	if is.Status != "open" {
		t.Fatalf("initial %q", is.Status)
	}
	if _, err := s.Transition(ctx, alice, is.Key, "qa", nil); err != nil {
		t.Fatal(err)
	}
	// A workflow that drops a status in use is refused.
	wf2 := Workflow{Statuses: []Status{{"open", CategoryTodo}, {"shipped", CategoryDone}}, Initial: "open", Transitions: map[string][]string{"open": {"shipped"}}}
	if _, err := s.UpdateProject(ctx, admin, "REL", ProjectPatch{Workflow: &wf2}); !isInvalid(err) {
		t.Fatalf("dropping qa in use: %v", err)
	}
	// Changing a category re-derives resolved_at.
	if _, err := s.Transition(ctx, alice, is.Key, "shipped", nil); err != nil {
		t.Fatal(err)
	}
	wf3 := wf
	wf3.Statuses = []Status{{"open", CategoryTodo}, {"qa", CategoryActive}, {"shipped", CategoryActive}, {"closed", CategoryDone}}
	wf3.Transitions = map[string][]string{"open": {"qa"}, "qa": {"open", "shipped"}, "shipped": {"closed"}}
	if _, err := s.UpdateProject(ctx, admin, "REL", ProjectPatch{Workflow: &wf3}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetIssue(ctx, alice, is.Key)
	if got.ResolvedAt != nil {
		t.Fatal("shipped is no longer a done status; resolved_at must clear")
	}
}

func TestLinksAndCycles(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	a := mustIssue(t, s, alice, "ACME", CreateIssue{}).Key
	b := mustIssue(t, s, alice, "ACME", CreateIssue{}).Key
	c := mustIssue(t, s, alice, "ACME", CreateIssue{}).Key
	if _, err := s.AddLink(ctx, alice, a, "blocks", b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLink(ctx, alice, b, "blocks", c); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"direct cycle":     func() error { _, err := s.AddLink(ctx, alice, b, "blocks", a); return err },
		"transitive cycle": func() error { _, err := s.AddLink(ctx, alice, c, "blocks", a); return err },
		"duplicate":        func() error { _, err := s.AddLink(ctx, alice, a, "blocks", b); return err },
	} {
		if err := call(); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: want conflict, got %v", name, err)
		}
	}
	if _, err := s.AddLink(ctx, alice, a, "blocks", a); !isInvalid(err) {
		t.Fatalf("self link: %v", err)
	}
	if _, err := s.AddLink(ctx, alice, a, "blocks", "ACME-99"); !isInvalid(err) {
		t.Fatalf("missing target: %v", err)
	}
	if _, err := s.AddLink(ctx, alice, a, "mentions", b); !isInvalid(err) {
		t.Fatalf("bad type: %v", err)
	}
	// relates is symmetric; relating in the reverse order is a repeat, and loops are fine.
	if _, err := s.AddLink(ctx, alice, a, "relates", b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLink(ctx, alice, b, "relates", a); !errors.Is(err, ErrConflict) {
		t.Fatalf("reverse relates: %v", err)
	}
	if _, err := s.AddLink(ctx, alice, a, "duplicates", c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLink(ctx, alice, c, "duplicates", a); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicates cycle: %v", err)
	}
	d, _ := s.GetIssue(ctx, alice, b)
	var inbound bool
	for _, l := range d.Links {
		if l.Type == "blocks" && l.Direction == "inbound" && l.Key == a {
			inbound = true
		}
	}
	if !inbound {
		t.Fatalf("b should show a blocking it: %+v", d.Links)
	}
	// Removal frees the edge, so the reverse link is now legal.
	ab := d.Links[0].ID
	if err := s.RemoveLink(ctx, alice, b, ab); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveLink(ctx, alice, b, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove missing link: %v", err)
	}
}

func TestCommentsActivityWatchersAttachments(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	is := mustIssue(t, s, alice, "ACME", CreateIssue{})
	c, err := s.AddComment(ctx, admin, is.Key, "Looks good")
	if err != nil || c.ID == 0 {
		t.Fatal(err)
	}
	if _, err := s.AddComment(ctx, admin, is.Key, "   "); !isInvalid(err) {
		t.Fatalf("blank comment: %v", err)
	}
	cs, _, _ := s.ListComments(ctx, alice, is.Key, 0, 10)
	if len(cs) != 1 || cs[0].Body != "Looks good" {
		t.Fatalf("comments: %+v", cs)
	}
	if err := s.SetWatcher(ctx, alice, is.Key, "carol@example.com", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a writer subscribing someone else: %v", err)
	}
	if err := s.SetWatcher(ctx, alice, is.Key, alice.Subject, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWatcher(ctx, admin, is.Key, "carol@example.com", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAttachment(ctx, alice, is.Key, NewAttachment{Filename: "spec.pdf", SizeBytes: 1024, SHA256: strings.Repeat("a", 64), URL: "https://files.example.com/spec.pdf"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []NewAttachment{
		{Filename: "../etc/passwd"}, {Filename: "a/b"}, {Filename: ""}, {Filename: "x", SizeBytes: -1},
		{Filename: "x", SHA256: "zz"}, {Filename: "x", URL: "javascript:alert(1)"}, {Filename: "x", URL: "file:///etc/passwd"},
	} {
		if _, err := s.AddAttachment(ctx, alice, is.Key, bad); !isInvalid(err) {
			t.Errorf("%+v should be rejected, got %v", bad, err)
		}
	}
	d, _ := s.GetIssue(ctx, alice, is.Key)
	if len(d.Attachments) != 1 || !slices.Contains(d.Watchers, "carol@example.com") || !slices.Contains(d.Watchers, alice.Subject) {
		t.Fatalf("detail: %+v", d)
	}
	acts, _, _ := s.ListActivity(ctx, alice, is.Key, 0, 50)
	var kinds []string
	for _, a := range acts {
		kinds = append(kinds, a.Kind)
	}
	if !slices.Equal(kinds, []string{"created", "commented", "attachment_added"}) {
		t.Fatalf("activity kinds %v", kinds)
	}
}

func TestSprintsBoardAndRank(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	sp, err := s.CreateSprint(ctx, alice, "ACME", NewSprint{Name: "Sprint 1", StartsOn: ptr("2026-11-30"), EndsOn: ptr("2026-12-13")})
	if err != nil || sp.State != "planned" {
		t.Fatal(err, sp)
	}
	sp2, _ := s.CreateSprint(ctx, alice, "ACME", NewSprint{Name: "Sprint 2"})
	if _, err := s.CreateSprint(ctx, alice, "ACME", NewSprint{Name: "bad", StartsOn: ptr("2026-12-13"), EndsOn: ptr("2026-11-30")}); !isInvalid(err) {
		t.Fatalf("end before start: %v", err)
	}
	a := mustIssue(t, s, alice, "ACME", CreateIssue{Title: "A", SprintID: &sp.ID})
	b := mustIssue(t, s, alice, "ACME", CreateIssue{Title: "B", SprintID: &sp.ID})
	c := mustIssue(t, s, alice, "ACME", CreateIssue{Title: "C"}) // backlog
	if _, err := s.StartSprint(ctx, alice, sp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSprint(ctx, alice, sp2.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("two active sprints: %v", err)
	}
	if _, err := s.Transition(ctx, alice, a.Key, "in_progress", nil); err != nil {
		t.Fatal(err)
	}
	// Drag C above A: only C's rank changes.
	if _, err := s.Rank(ctx, alice, c.Key, RankRequest{Before: a.Key}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rank(ctx, alice, b.Key, RankRequest{Position: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rank(ctx, alice, b.Key, RankRequest{Before: a.Key, After: c.Key}); !isInvalid(err) {
		t.Fatalf("both before and after: %v", err)
	}
	board, err := s.GetBoard(ctx, alice, "ACME", "active")
	if err != nil {
		t.Fatal(err)
	}
	if board.Sprint == nil || board.Sprint.ID != sp.ID || len(board.Columns) != 5 {
		t.Fatalf("board: %+v", board)
	}
	var todo, prog []string
	for _, col := range board.Columns {
		for _, is := range col.Issues {
			switch col.Status {
			case "todo":
				todo = append(todo, is.Key)
			case "in_progress":
				prog = append(prog, is.Key)
			}
		}
	}
	if !slices.Equal(todo, []string{b.Key}) || !slices.Equal(prog, []string{a.Key}) {
		t.Fatalf("board columns todo=%v progress=%v", todo, prog)
	}
	backlog, _ := s.GetBoard(ctx, alice, "ACME", "backlog")
	if backlog.Columns[0].Count != 1 || backlog.Columns[0].Issues[0].Key != c.Key {
		t.Fatalf("backlog: %+v", backlog.Columns[0])
	}
	// Ordering within the whole project: b, c, a.
	all, _, _ := s.ListIssues(ctx, alice, "ACME", "", "rank", 50, 0)
	if all[0].Key != b.Key || all[1].Key != c.Key || all[2].Key != a.Key {
		t.Fatalf("rank order: %v %v %v", all[0].Key, all[1].Key, all[2].Key)
	}
	// Completing moves unresolved issues to the next sprint.
	if _, err := s.Transition(ctx, alice, a.Key, "in_review", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(ctx, alice, a.Key, "done", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteSprint(ctx, alice, sp.ID, &sp2.ID); err != nil {
		t.Fatal(err)
	}
	gb, _ := s.GetIssue(ctx, alice, b.Key)
	ga, _ := s.GetIssue(ctx, alice, a.Key)
	if gb.SprintID == nil || *gb.SprintID != sp2.ID || ga.SprintID == nil || *ga.SprintID != sp.ID {
		t.Fatalf("carry-over: b=%v a=%v", gb.SprintID, ga.SprintID)
	}
	// A closed sprint cannot take issues.
	if _, err := s.UpdateIssue(ctx, alice, c.Key, &Patch{SprintID: Opt[int64]{Set: true, V: sp.ID}}, nil); !isInvalid(err) {
		t.Fatalf("closed sprint: %v", err)
	}
}

func TestSearchAndPagination(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		in := CreateIssue{Title: "Task number " + string(rune('A'+i)), Priority: "P2", Labels: []string{"backend"}}
		if i%2 == 0 {
			in.Priority, in.Assignee = "P0", ptr("bob@example.com")
		}
		mustIssue(t, s, alice, "ACME", in)
	}
	mustIssue(t, s, alice, "ACME", CreateIssue{Title: "100%_off banner", Type: "bug"})
	list := func(q string) []Issue {
		items, _, err := s.ListIssues(ctx, alice, "", q, "", 50, 0)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		return items
	}
	if n := len(list("priority:P0")); n != 4 {
		t.Fatalf("P0 = %d", n)
	}
	if n := len(list("priority:P0 assignee:bob@example.com label:backend")); n != 4 {
		t.Fatalf("combined = %d", n)
	}
	if n := len(list("type:bug")); n != 1 {
		t.Fatalf("bug = %d", n)
	}
	if n := len(list("assignee:none")); n != 4 {
		t.Fatalf("unassigned = %d", n)
	}
	if n := len(list("-priority:P0 label:backend")); n != 3 {
		t.Fatalf("negation = %d", n)
	}
	if n := len(list(`"100%_off"`)); n != 1 {
		t.Fatalf("literal percent = %d", n)
	}
	if n := len(list("100%")); n != 1 { // % is a literal, not a wildcard
		t.Fatalf("percent = %d", n)
	}
	if n := len(list("%")); n != 1 {
		t.Fatalf("bare percent matched %d (wildcard leak)", n)
	}
	if n := len(list(`"x'; DROP TABLE issues; --"`)); n != 0 {
		t.Fatalf("injection string matched %d", n)
	}
	if n := len(list("")); n != 8 {
		t.Fatalf("all = %d (table dropped?)", n)
	}
	// Pagination walks everything exactly once.
	var seen []string
	off := 0
	for off >= 0 {
		items, next, err := s.ListIssues(ctx, alice, "", "", "created", 3, off)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			seen = append(seen, it.Key)
		}
		off = next
	}
	if len(seen) != 8 || seen[0] != "ACME-1" || seen[7] != "ACME-8" {
		t.Fatalf("pagination saw %v", seen)
	}
	if _, _, err := s.ListIssues(ctx, alice, "", "bogus:1", "", 10, 0); !isInvalid(err) {
		t.Fatalf("bad query: %v", err)
	}
	if _, _, err := s.ListIssues(ctx, alice, "", "", "id; DROP", 10, 0); !isInvalid(err) {
		t.Fatalf("bad sort: %v", err)
	}
	if _, _, err := s.ListIssues(ctx, alice, "", "", "", 10, -5); !isInvalid(err) {
		t.Fatalf("bad offset: %v", err)
	}
}

func TestBulkUpdate(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	a := mustIssue(t, s, alice, "ACME", CreateIssue{})
	b := mustIssue(t, s, alice, "ACME", CreateIssue{})
	res, err := s.BulkUpdate(ctx, alice, []string{a.Key, b.Key, "ACME-99", a.Key}, &Patch{Priority: Opt[string]{Set: true, V: "P1"}, LabelsAdd: []string{"urgent"}}, "in_progress")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 || !res[0].OK || !res[1].OK || res[2].OK || res[2].Code != "not_found" {
		t.Fatalf("results: %+v", res)
	}
	got, _ := s.GetIssue(ctx, alice, a.Key)
	if got.Priority != "P1" || got.Status != "in_progress" || !slices.Contains(got.Labels, "urgent") {
		t.Fatalf("bulk effect: %+v", got.Issue)
	}
	if _, err := s.BulkUpdate(ctx, alice, []string{a.Key}, &Patch{Version: Opt[int64]{Set: true, V: 1}}, ""); !isInvalid(err) {
		t.Fatalf("version in bulk: %v", err)
	}
	if _, err := s.BulkUpdate(ctx, alice, nil, &Patch{}, "done"); !isInvalid(err) {
		t.Fatalf("no keys: %v", err)
	}
}

func TestProjectScopeIDOR(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	mustProject(t, s, "SEC")
	ctx := context.Background()
	secret := mustIssue(t, s, admin, "SEC", CreateIssue{Title: "layoffs plan"})
	own := mustIssue(t, s, acmeRW, "ACME", CreateIssue{Title: "mine"})

	notFound := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want not found (not forbidden, no existence oracle), got %v", name, err)
		}
	}
	_, err := s.GetIssue(ctx, acmeRW, secret.Key)
	notFound("get", err)
	_, err = s.UpdateIssue(ctx, acmeRW, secret.Key, &Patch{Title: Opt[string]{Set: true, V: "x"}}, nil)
	notFound("update", err)
	_, err = s.Transition(ctx, acmeRW, secret.Key, "in_progress", nil)
	notFound("transition", err)
	_, err = s.AddComment(ctx, acmeRW, secret.Key, "hi")
	notFound("comment", err)
	_, _, err = s.ListComments(ctx, acmeRW, secret.Key, 0, 10)
	notFound("list comments", err)
	_, _, err = s.ListActivity(ctx, acmeRW, secret.Key, 0, 10)
	notFound("activity", err)
	_, err = s.AddAttachment(ctx, acmeRW, secret.Key, NewAttachment{Filename: "a"})
	notFound("attach", err)
	notFound("watch", s.SetWatcher(ctx, acmeRW, secret.Key, "x", true))
	_, err = s.Rank(ctx, acmeRW, secret.Key, RankRequest{Position: "first"})
	notFound("rank", err)
	_, _, err = s.CreateIssue(ctx, acmeRW, "SEC", CreateIssue{Type: "task", Title: "x"}, "")
	notFound("create in other project", err)
	_, err = s.GetProject(ctx, acmeRW, "SEC")
	notFound("get project", err)
	_, err = s.GetBoard(ctx, acmeRW, "SEC", "all")
	notFound("board", err)
	_, err = s.CreateSprint(ctx, acmeRW, "SEC", NewSprint{Name: "x"})
	notFound("sprint", err)
	// Linking to an out-of-scope issue looks like a missing target.
	if _, err := s.AddLink(ctx, acmeRW, own.Key, "blocks", secret.Key); !isInvalid(err) {
		t.Errorf("link to other project: %v", err)
	}
	// Search never returns it, even when asked by key or project.
	for _, q := range []string{"", "key:" + secret.Key, "project:SEC", "layoffs"} {
		items, _, err := s.ListIssues(ctx, acmeRW, "", q, "", 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Project == "SEC" {
				t.Errorf("query %q leaked %s", q, it.Key)
			}
		}
	}
	if items, _, _ := s.ListIssues(ctx, acmeRW, "SEC", "", "", 50, 0); len(items) != 0 {
		t.Error("project filter leaked")
	}
	ps, _ := s.ListProjects(ctx, acmeRW)
	if len(ps) != 1 || ps[0].Key != "ACME" {
		t.Fatalf("project list: %+v", ps)
	}
	// A scoped principal cannot create projects.
	if _, err := s.CreateProject(ctx, acmeRW, NewProject{Key: "NEW", Name: "n"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("scoped create project: %v", err)
	}
}

func TestArchivedProjectIsReadOnly(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	is := mustIssue(t, s, alice, "ACME", CreateIssue{})
	if _, err := s.UpdateProject(ctx, admin, "ACME", ProjectPatch{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateIssue(ctx, alice, "ACME", CreateIssue{Type: "task", Title: "x"}, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Transition(ctx, alice, is.Key, "in_progress", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("transition: %v", err)
	}
	if _, err := s.GetIssue(ctx, alice, is.Key); err != nil {
		t.Fatalf("read must still work: %v", err)
	}
}

func TestProjectValidationAndDuplicate(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for _, key := range []string{"a", "acme", "1ABC", "TOOLONGKEY1X", "AC ME", "AC-ME"} {
		if _, err := s.CreateProject(ctx, admin, NewProject{Key: key, Name: "x"}); !isInvalid(err) {
			t.Errorf("key %q: %v", key, err)
		}
	}
	mustProject(t, s, "ACME")
	if _, err := s.CreateProject(ctx, admin, NewProject{Key: "ACME", Name: "again"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	bad := DefaultSLA()
	bad["P0"] = SLATarget{RespondHours: 0, ResolveHours: 1}
	if _, err := s.CreateProject(ctx, admin, NewProject{Key: "SLA", Name: "x", SLA: bad}); !isInvalid(err) {
		t.Fatalf("bad sla: %v", err)
	}
	audit, _ := s.ListAudit(ctx, 0, 10)
	if len(audit) != 1 || audit[0].Action != "project.create" || audit[0].Actor != "root" {
		t.Fatalf("audit: %+v", audit)
	}
}

func TestSLAOverrideShowsOnIssues(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	sla := DefaultSLA()
	sla["P0"] = SLATarget{RespondHours: 2, ResolveHours: 8}
	if _, err := s.CreateProject(ctx, admin, NewProject{Key: "ACME", Name: "x", SLA: sla}); err != nil {
		t.Fatal(err)
	}
	is := mustIssue(t, s, alice, "ACME", CreateIssue{Priority: "P0"})
	if is.SLAHint.RespondHours != 2 || is.SLAHint.ResolveHours != 8 {
		t.Fatalf("hint %+v", is.SLAHint)
	}
	up, _ := s.UpdateIssue(ctx, alice, is.Key, &Patch{Priority: Opt[string]{Set: true, V: "P3"}}, nil)
	if up.SLAHint.ResolveHours != DefaultSLA()["P3"].ResolveHours {
		t.Fatalf("hint after reprioritise %+v", up.SLAHint)
	}
}

func TestPurgeRetention(t *testing.T) {
	s, clk := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	old := mustIssue(t, s, alice, "ACME", CreateIssue{})
	open := mustIssue(t, s, alice, "ACME", CreateIssue{})
	for _, to := range []string{"in_progress", "done"} {
		if _, err := s.Transition(ctx, alice, old.Key, to, nil); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = s.AddComment(ctx, alice, old.Key, "bye")
	clk.Advance(400 * 24 * time.Hour)
	n, err := s.Purge(ctx, 365, 0)
	if err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if _, err := s.GetIssue(ctx, alice, old.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old issue survived: %v", err)
	}
	if _, err := s.GetIssue(ctx, alice, open.Key); err != nil {
		t.Fatalf("open issue purged: %v", err)
	}
	var c int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM comments`).Scan(&c)
	if c != 0 {
		t.Fatal("comments must go with the issue")
	}
	if n, _ := s.Purge(ctx, 0, 0); n != 0 {
		t.Fatal("retention 0 keeps everything")
	}
}

// Regression (QA blocker): a multi-value key: term used to bind only its last alternative to
// the caller's project scope. No query-language combination may return an out-of-scope issue.
func TestSearchNeverLeavesTokenScope(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	mustProject(t, s, "SEC")
	ctx := context.Background()
	secEpic := mustIssue(t, s, admin, "SEC", CreateIssue{Type: "epic", Title: "layoffs"})
	sec := mustIssue(t, s, admin, "SEC", CreateIssue{Title: "secret plan", Priority: "P0", Assignee: ptr("ceo"), Labels: []string{"x"}, Parent: &secEpic.Key})
	own := mustIssue(t, s, admin, "ACME", CreateIssue{Title: "mine", Priority: "P0", Assignee: ptr("ceo"), Labels: []string{"x"}})
	if _, err := s.AddLink(ctx, admin, sec.Key, "blocks", own.Key); err != nil {
		t.Fatal(err)
	}
	pool := []string{
		"key:SEC-1,ACME-1", "key:ACME-1,SEC-1", "key:SEC-2", "key:SEC-1,SEC-2,ACME-1", "-key:ACME-1", "project:SEC,ACME", "project:SEC",
		"-project:ACME", "is:blocked", "-is:blocked", "is:open", "-is:resolved", "priority:P0", "-priority:P3", "assignee:ceo", "label:x",
		"-label:none", "parent:SEC-1", "parent:none", "-parent:none", "sprint:none", "layoffs", "secret", "text:plan", "type:epic,task",
		"status:todo", "-status:done", "created>=-7d", "updated>2000-01-01", "due<2100-01-01", "-assignee:none", "reporter:root",
		// hostile and odd shapes
		"(", ")", "((key:SEC-1))", ") OR (1=1", "' OR 1=1 --", `"unbalanced`, `"quoted \" text"`, `"project:SEC"`, "OR", "AND", "NOT",
		"ｋｅｙ:ＳＥＣ-1", "ｐｒｏｊｅｃｔ:SEC", "日本語", "\u0000", "key:SEC-1\u200b", "KEY:SEC-1", "Project:SEC", "-Key:ACME-1",
		"label:" + strings.Repeat("a", 200), "assignee:" + strings.Repeat("ü", 100), strings.Repeat("z", 400),
		"project:ACME,SEC", "project:SEC,ACME,OPS", "-project:SEC", "key:SEC-1,SEC-2,SEC-3,ACME-1,ACME-2,SEC-9",
		"sprint:1", "sprint:2", "sprint:active", "-sprint:none", "sprint:none", "parent:SEC-1,ACME-1", "-parent:SEC-1",
		"is:blocked is:open", "is:overdue", "text:ACME", "%", "_", "1=1", "created:2026-11-25", "resolved<2100-01-01",
	}
	rng := rand.New(rand.NewSource(11))
	check := func(q string) {
		t.Helper()
		items, _, err := s.ListIssues(ctx, acmeRW, "", q, "", 100, 0)
		if err != nil {
			return // a rejected query returns nothing
		}
		for _, it := range items {
			if it.Project != "ACME" {
				t.Fatalf("query %q leaked %s to an ACME-only token", q, it.Key)
			}
		}
	}
	for _, q := range pool {
		check(q)
		check("project:ACME " + q)
	}
	sp, err := s.CreateSprint(ctx, admin, "SEC", NewSprint{Name: "sec sprint"})
	if err != nil || sp.ID != 1 {
		t.Fatalf("sprint 1 should be SEC's: %+v %v", sp, err)
	}
	if _, err := s.UpdateIssue(ctx, admin, sec.Key, &Patch{SprintID: Opt[int64]{Set: true, V: sp.ID}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSprint(ctx, admin, sp.ID); err != nil {
		t.Fatal(err)
	}
	// Seeded and deterministic. 2000 random combinations on top of the exhaustive pool above keep this
	// under a few seconds with -race; CAN_TEST_THOROUGH=1 restores the original 6000.
	combos := 2000
	if os.Getenv("CAN_TEST_THOROUGH") == "1" {
		combos = 6000
	}
	for i := 0; i < combos; i++ {
		var parts []string
		for j := 0; j < 1+rng.Intn(4); j++ {
			parts = append(parts, pool[rng.Intn(len(pool))])
		}
		check(strings.Join(parts, " "))
	}
	// The exact bypass: either order must give exactly the own issue.
	for _, q := range []string{"key:SEC-1,ACME-1", "key:ACME-1,SEC-1"} {
		items, _, _ := s.ListIssues(ctx, acmeRW, "", q, "", 100, 0)
		if len(items) != 1 || items[0].Key != "ACME-1" {
			t.Fatalf("%q = %+v", q, items)
		}
	}
	// is:blocked must not reveal that an out-of-scope issue blocks ours.
	items, _, _ := s.ListIssues(ctx, acmeRW, "", "is:blocked", "", 100, 0)
	if len(items) != 0 {
		t.Fatalf("is:blocked leaked the existence of an invisible blocker: %+v", items)
	}
	if items, _, _ = s.ListIssues(ctx, admin, "", "is:blocked", "", 100, 0); len(items) != 1 {
		t.Fatalf("admin should see the blocked issue: %+v", items)
	}
}

func TestSearchLimits(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	if _, _, err := s.ListIssues(ctx, alice, "", "a b c d", "", 10, 0); !isInvalid(err) {
		t.Fatalf("four free-text terms: %v", err)
	}
	if _, _, err := s.ListIssues(ctx, alice, "", "a b c status:todo", "", 10, 0); err != nil {
		t.Fatalf("three free-text terms plus a field: %v", err)
	}
	if _, _, err := s.ListIssues(ctx, alice, "", "", "", 10, maxOffset+1); !isInvalid(err) {
		t.Fatalf("offset past the clamp: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.ListIssues(cctx, alice, "", "", "", 10, 0); err == nil {
		t.Fatal("a cancelled or timed-out search must stop")
	}
}

// Searches run on their own read pool: a write transaction that is open must not stall them.
func TestSearchDoesNotWaitForWriters(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	mustIssue(t, s, alice, "ACME", CreateIssue{})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET name = 'held'`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		items, _, err := s.ListIssues(ctx, alice, "", "", "", 10, 0)
		if err == nil && len(items) != 1 {
			err = errors.New("wrong result")
		}
		if _, e := s.GetBoard(ctx, alice, "ACME", "all"); err == nil {
			err = e
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("search blocked behind an open write transaction")
	}
}

func TestWorkflowRejectsDeadEnds(t *testing.T) {
	wf := Workflow{
		Statuses:    []Status{{"open", CategoryTodo}, {"stuck", CategoryActive}, {"shipped", CategoryDone}},
		Initial:     "open",
		Transitions: map[string][]string{"open": {"stuck", "shipped"}}, // stuck has no way out
	}
	if err := wf.Validate(); err == nil || !strings.Contains(err.Error(), "stuck") {
		t.Fatalf("dead end accepted: %v", err)
	}
	wf.Transitions["stuck"] = []string{"open"}
	if err := wf.Validate(); err != nil {
		t.Fatalf("with a way out: %v", err)
	}
	// A status nobody can reach is harmless, since issues never enter it.
	wf.Statuses = append(wf.Statuses, Status{"orphan", CategoryActive})
	if err := wf.Validate(); err != nil {
		t.Fatalf("unreachable status: %v", err)
	}
}

func TestBootstrapSubjectIsReserved(t *testing.T) {
	s, _ := newTestStore(t)
	for _, sub := range []string{"bootstrap-admin", "Bootstrap-Admin"} {
		if _, _, err := s.CreateToken(context.Background(), admin, NewToken{Kind: KindService, Subject: sub, Scopes: []string{PermRead}}); !isInvalid(err) {
			t.Errorf("%s: %v", sub, err)
		}
	}
}

func TestLinkToArchivedProjectRefused(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	mustProject(t, s, "OLD")
	ctx := context.Background()
	a := mustIssue(t, s, alice, "ACME", CreateIssue{}).Key
	b := mustIssue(t, s, alice, "OLD", CreateIssue{}).Key
	if _, err := s.UpdateProject(ctx, admin, "OLD", ProjectPatch{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLink(ctx, alice, a, "relates", b); !errors.Is(err, ErrConflict) {
		t.Fatalf("link into an archived project: %v", err)
	}
	acts, _, _ := s.ListActivity(ctx, alice, b, 0, 50)
	for _, ac := range acts {
		if ac.Kind == "linked" {
			t.Fatal("archived project's activity changed")
		}
	}
}

func TestPurgeKeepsParentsOfSurvivingChildren(t *testing.T) {
	s, clk := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	epic := mustIssue(t, s, alice, "ACME", CreateIssue{Type: "epic", Title: "E"})
	story := mustIssue(t, s, alice, "ACME", CreateIssue{Type: "story", Title: "S", Parent: &epic.Key})
	sub := mustIssue(t, s, alice, "ACME", CreateIssue{Type: "subtask", Title: "T", Parent: &story.Key})
	for _, k := range []string{epic.Key, story.Key} {
		for _, to := range []string{"in_progress", "done"} {
			if _, err := s.Transition(ctx, alice, k, to, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	clk.Advance(400 * 24 * time.Hour)
	if n, err := s.Purge(ctx, 365, 0); err != nil || n != 0 {
		t.Fatalf("parents with an open child must survive: %d %v", n, err)
	}
	if d, err := s.GetIssue(ctx, alice, sub.Key); err != nil || d.Parent == nil {
		t.Fatalf("subtask orphaned: %+v %v", d, err)
	}
	for _, to := range []string{"in_progress", "done"} {
		if _, err := s.Transition(ctx, alice, sub.Key, to, nil); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(400 * 24 * time.Hour)
	if n, err := s.Purge(ctx, 365, 0); err != nil || n != 3 {
		t.Fatalf("whole resolved tree should go: %d %v", n, err)
	}
}

func TestCompleteSprintRecordsMovedIssues(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	s.events = true
	ctx := context.Background()
	sp1, _ := s.CreateSprint(ctx, alice, "ACME", NewSprint{Name: "1"})
	sp2, _ := s.CreateSprint(ctx, alice, "ACME", NewSprint{Name: "2"})
	is := mustIssue(t, s, alice, "ACME", CreateIssue{SprintID: &sp1.ID})
	if _, err := s.StartSprint(ctx, alice, sp1.ID); err != nil {
		t.Fatal(err)
	}
	var before int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&before)
	if _, err := s.CompleteSprint(ctx, alice, sp1.ID, &sp2.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE type = ?`, EventIssueUpdated).Scan(&after)
	if after != 1 {
		t.Fatalf("moved issue emitted %d updated events", after)
	}
	acts, _, _ := s.ListActivity(ctx, alice, is.Key, 0, 50)
	if last := acts[len(acts)-1]; last.Kind != "updated" || !strings.Contains(string(last.Data), "sprint_id") {
		t.Fatalf("activity: %+v", last)
	}
	got, _ := s.GetIssue(ctx, alice, is.Key)
	if got.Version != is.Version+1 {
		t.Fatalf("version %d", got.Version)
	}
}

func TestRankKeepsVersionButLogsActivityAndStaysShort(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	a := mustIssue(t, s, alice, "ACME", CreateIssue{})
	b := mustIssue(t, s, alice, "ACME", CreateIssue{})
	moved, err := s.Rank(ctx, alice, b.Key, RankRequest{Before: a.Key})
	if err != nil || moved.Version != b.Version || !moved.UpdatedAt.Equal(b.UpdatedAt) {
		t.Fatalf("rank must not change version or updated_at: %v %+v", err, moved)
	}
	acts, _, _ := s.ListActivity(ctx, alice, b.Key, 0, 50)
	if acts[len(acts)-1].Kind != "ranked" {
		t.Fatalf("activity: %+v", acts)
	}
	// Hammer one spot (always insert at the very front) and append many: ranks stay bounded
	// and the order stays right.
	var keys []string
	for i := 0; i < 120; i++ {
		is := mustIssue(t, s, alice, "ACME", CreateIssue{Title: "n"})
		if _, err := s.Rank(ctx, alice, is.Key, RankRequest{Position: "first"}); err != nil {
			t.Fatal(err)
		}
		keys = append([]string{is.Key}, keys...)
	}
	all, _, err := s.ListIssues(ctx, alice, "ACME", "", "rank", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range all {
		if len(it.Rank) > maxRankLen+1 {
			t.Fatalf("rank %q is %d long", it.Rank, len(it.Rank))
		}
		if i < 100 && it.Key != keys[i] {
			t.Fatalf("position %d is %s, want %s", i, it.Key, keys[i])
		}
	}
	for i := 0; i < 300; i++ {
		mustIssue(t, s, alice, "ACME", CreateIssue{})
	}
	last, _, _ := s.ListIssues(ctx, alice, "ACME", "", "-created", 1, 0)
	if len(last[0].Rank) > maxRankLen+1 {
		t.Fatalf("appends grew the rank to %q", last[0].Rank)
	}
}

func TestRemoveLinkNeedsBothEndsVisible(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	mustProject(t, s, "SEC")
	ctx := context.Background()
	sec := mustIssue(t, s, admin, "SEC", CreateIssue{})
	own := mustIssue(t, s, admin, "ACME", CreateIssue{})
	l, err := s.AddLink(ctx, admin, sec.Key, "blocks", own.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveLink(ctx, acmeRW, own.Key, l.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scoped token removing a link into SEC: %v", err)
	}
	d, _ := s.GetIssue(ctx, admin, own.Key)
	if len(d.Links) != 1 {
		t.Fatal("the link must still exist")
	}
	acts, _, _ := s.ListActivity(ctx, admin, sec.Key, 0, 50)
	for _, a := range acts {
		if a.Kind == "unlinked" {
			t.Fatal("activity written on the invisible side")
		}
	}
	if err := s.RemoveLink(ctx, admin, own.Key, l.ID); err != nil {
		t.Fatalf("an admin can: %v", err)
	}
}

func TestAdminTokenMustBeStrong(t *testing.T) {
	for _, weak := range []string{"", "short", strings.Repeat("a", 40), "abababababababababababababababab", "0123456789012345678901234567"} {
		if _, err := SecretsFromEnv(func(k string) string {
			if k == "CAN_WORK_ADMIN_TOKEN" {
				return weak
			}
			return ""
		}); err == nil {
			t.Errorf("%q accepted", weak)
		}
	}
	if _, err := SecretsFromEnv(func(k string) string {
		if k == "CAN_WORK_ADMIN_TOKEN" {
			return "q8Zr3LmW0xYp7TnVb2KdHs9JfAe5GcUo"
		}
		return ""
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchTimeoutIsA503WithAHint(t *testing.T) {
	rec := httptest.NewRecorder()
	writeStoreError(rec, slog.New(slog.DiscardHandler), fmt.Errorf("query: %w", context.DeadlineExceeded))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "narrow the query") || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}
