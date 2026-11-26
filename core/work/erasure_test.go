package work

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	leaver = "leaver.fake@example.test"
	stayer = "stayer.fake@example.test"
)

func person(subject string) *Principal {
	return &Principal{Subject: subject, Permissions: []string{PermWrite}}
}

// seedPeople puts the leaver everywhere the tracker holds a person, the stayer beside them, and a
// former assignee who is left only in the history.
func seedPeople(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	mustProject(t, s, "ACME")
	root := &Principal{Subject: "root", Permissions: []string{PermAdmin}}
	who, other, former := leaver, stayer, "former.fake@example.test"
	a := mustIssue(t, s, person(leaver), "ACME", CreateIssue{Assignee: &who, Title: "reported and held by the leaver"})
	b := mustIssue(t, s, person(stayer), "ACME", CreateIssue{Assignee: &former, Title: leaver}) // a title is not an identifier
	if _, err := s.UpdateIssue(ctx, root, b.Key, &Patch{Assignee: Opt[string]{Set: true, V: other}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{leaver, stayer} {
		if _, err := s.AddComment(ctx, person(p), a.Key, "a comment"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddAttachment(ctx, person(leaver), a.Key, NewAttachment{Filename: "a.txt", SizeBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddLink(ctx, person(leaver), a.Key, "blocks", b.Key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateIssue(ctx, person(leaver), "ACME", CreateIssue{Type: "task", Title: "cached"}, "idem-key-0001"); err != nil {
		t.Fatal(err)
	}
}

// held lists the table rows that name identity, the audit log aside.
func held(t *testing.T, s *Store, identity string) []string {
	t.Helper()
	var out []string
	for _, table := range []string{"issues", "comments", "activity", "watchers", "attachments", "issue_links", "api_tokens", "idempotency", "outbox"} {
		rs, err := s.db.Query(`SELECT * FROM ` + table) //nolint:gosec // test
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
			for i, v := range vals {
				// the one title that is spelled like the identifier is not an identifier
				text := strings.ReplaceAll(fmt.Sprintf("%s", v), `"title":"`+identity+`"`, "")
				if cols[i] != "title" && strings.Contains(text, identity) {
					out = append(out, table+"."+cols[i]+": "+text)
				}
			}
		}
		if err := rs.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func copyStore(t *testing.T, s *Store) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copy.db")
	if _, err := s.db.Exec(`VACUUM INTO ?`, path); err != nil {
		t.Fatal(err)
	}
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	c.now, c.events = s.now, s.events
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestErasePerson(t *testing.T) {
	s, clk := newTestStore(t)
	seedPeople(t, s)
	ctx := t.Context()
	root := &Principal{Subject: "root", Permissions: []string{PermAdmin}}
	if _, _, err := s.CreateToken(ctx, root, NewToken{Kind: KindService, Subject: leaver, Name: "of the leaver", Scopes: []string{PermRead}}); err != nil {
		t.Fatal(err)
	}
	if len(held(t, s, leaver)) < 8 || len(held(t, s, stayer)) == 0 {
		t.Fatalf("the seed proves nothing: %v", held(t, s, leaver))
	}
	stayerBefore := held(t, s, stayer)

	for name, p := range map[string]*Principal{"writer": person("w"), "scoped admin": {Subject: "a", Permissions: []string{PermAdmin}, Projects: []string{"ACME"}}} {
		if _, err := s.ErasePerson(ctx, p, leaver); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, bad := range []string{"", bootstrapSubject, replayActor, "erased-0123456789abcdef", " padded "} {
		if _, err := s.ErasePerson(ctx, root, bad); !isInvalid(err) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := s.ErasePerson(ctx, root, "nobody@example.test"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}

	res, err := s.ErasePerson(ctx, root, leaver)
	if err != nil {
		t.Fatal(err)
	}
	if !pseudonymRe.MatchString(res.Pseudonym) || res.Subject != erasureSubject(leaver) || !erasureSubjectRe.MatchString(res.Subject) {
		t.Fatalf("result %+v", res)
	}
	if left := held(t, s, leaver); len(left) != 0 {
		t.Fatalf("still held:\n%s", strings.Join(left, "\n"))
	}
	after := held(t, s, stayer)
	if len(after) == 0 || len(after) > len(stayerBefore) {
		t.Fatalf("the other person: %v", after)
	}
	var reporter, title string
	var revoked, tokens int
	if err := s.db.QueryRow(`SELECT reporter FROM issues WHERE number = 1`).Scan(&reporter); err != nil || reporter != res.Pseudonym {
		t.Fatalf("reporter %q %v", reporter, err)
	}
	if err := s.db.QueryRow(`SELECT title FROM issues WHERE number = 2`).Scan(&title); err != nil || title != leaver {
		t.Fatalf("a title was rewritten: %q %v", title, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*), COUNT(revoked_at) FROM api_tokens WHERE subject = ?`, res.Pseudonym).Scan(&tokens, &revoked); err != nil || tokens != 1 || revoked != 1 {
		t.Fatalf("tokens %d revoked %d %v", tokens, revoked, err)
	}
	// The erasure's audit entry names nobody.
	var target, detail string
	if err := s.db.QueryRow(`SELECT target, detail FROM audit_log WHERE action = 'person.erase'`).Scan(&target, &detail); err != nil || target != res.Subject || strings.Contains(detail, "@") {
		t.Fatalf("audit %q %q %v", target, detail, err)
	}

	first, err := s.ListErasures(ctx, 0)
	if err != nil || len(first) != 1 || first[0].Class != ClassPerson || first[0].Subject != res.Subject {
		t.Fatalf("feed %+v %v", first, err)
	}
	clk.Advance(time.Hour)
	again, err := s.ErasePerson(ctx, root, leaver)
	if err != nil || !again.AlreadyErased || again.Pseudonym != "" {
		t.Fatalf("repeat %+v %v", again, err)
	}
	if es, _ := s.ListErasures(ctx, 0); !slices.Equal(es, first) {
		t.Fatalf("a repeat that erased nothing moved the tombstone: %+v", es)
	}
	// Used again since: erased again, and the tombstone takes the later time.
	mustIssue(t, s, person(leaver), "ACME", CreateIssue{})
	clk.Advance(time.Hour)
	if res, err := s.ErasePerson(ctx, root, leaver); err != nil || res.AlreadyErased {
		t.Fatalf("again %+v %v", res, err)
	}
	if es, _ := s.ListErasures(ctx, 0); len(es) != 1 || es[0].At <= first[0].At {
		t.Fatalf("tombstone %+v", es)
	}
}

func TestErasePersonScrubsTheFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mustProject(t, s, "ACME")
	who := leaver
	mustIssue(t, s, person(leaver), "ACME", CreateIssue{Assignee: &who})
	mustIssue(t, s, person(stayer), "ACME", CreateIssue{})
	root := &Principal{Subject: "root", Permissions: []string{PermAdmin}}
	if _, err := s.ErasePerson(t.Context(), root, leaver); err != nil {
		t.Fatal(err)
	}
	var all []byte
	for _, f := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(f) //nolint:gosec // test
		if err == nil {
			all = append(all, b...)
		}
	}
	if bytes.Contains(all, []byte(leaver)) {
		t.Error("the erased identifier is still in the database files")
	}
	if !bytes.Contains(all, []byte(stayer)) {
		t.Error("the check above proves nothing: the kept identifier is not found either")
	}
}

func TestReplayPersonErasure(t *testing.T) {
	s, _ := newTestStore(t)
	seedPeople(t, s)
	ctx := context.Background()
	for _, who := range []string{leaver, "former.fake@example.test"} { // the second is left only in the history
		restored := copyStore(t, s)
		others := len(held(t, restored, stayer))
		res, err := restored.PurgeSubject(ctx, erasureSubject(who), ClassPerson)
		if err != nil || res == nil {
			t.Fatalf("%s: %+v %v", who, res, err)
		}
		if left := held(t, restored, who); len(left) != 0 {
			t.Fatalf("%s still held:\n%s", who, strings.Join(left, "\n"))
		}
		if len(held(t, restored, stayer)) == 0 || (who != leaver && len(held(t, restored, leaver)) == 0) || others == 0 {
			t.Fatalf("%s: somebody else was erased", who)
		}
		if again, err := restored.PurgeSubject(ctx, erasureSubject(who), ClassPerson); err != nil || again != nil {
			t.Fatalf("repeat: %+v %v", again, err)
		}
	}

	before := fmt.Sprint(held(t, s, leaver), held(t, s, stayer))
	if res, err := s.PurgeSubject(ctx, erasureSubject("nobody@example.test"), ClassPerson); err != nil || res != nil {
		t.Fatalf("a person not in this database: %+v %v", res, err)
	}
	for _, class := range []string{"work-audit", "work-attachment-metadata", "all", ""} {
		if _, err := s.PurgeSubject(ctx, erasureSubject(leaver), class); err == nil {
			t.Errorf("class %q was accepted", class)
		}
	}
	for _, subject := range []string{"", leaver, "person-abc"} {
		if _, err := s.PurgeSubject(ctx, subject, ClassPerson); err == nil {
			t.Errorf("subject %q was accepted", subject)
		}
	}
	if fmt.Sprint(held(t, s, leaver), held(t, s, stayer)) != before {
		t.Fatal("a refused or unmatched replay changed data")
	}
}

func TestErasePersonAPIAndPurgeCommand(t *testing.T) {
	h := newHarness(t, nil)
	writer := h.setup()
	h.mustDo(201, "POST", "/v1/projects/ACME/issues", bootstrap, map[string]any{"type": "task", "title": "t", "assignee": leaver})
	restored := copyStore(t, h.app.store)
	h.mustDo(403, "POST", "/v1/people/erase", writer, map[string]any{"subject": leaver})
	h.mustDo(403, "GET", "/v1/erasures", writer, nil)
	h.mustDo(401, "POST", "/v1/people/erase", "", map[string]any{"subject": leaver})
	h.mustDo(404, "POST", "/v1/people/erase", bootstrap, map[string]any{"subject": "nobody@example.test"})
	out := h.mustDo(200, "POST", "/v1/people/erase", bootstrap, map[string]any{"subject": leaver})
	items := h.mustDo(200, "GET", "/v1/erasures", bootstrap, nil)["items"].([]any)
	subject, _ := out["subject"].(string)
	if len(items) != 1 || items[0].(map[string]any)["subject"] != subject || items[0].(map[string]any)["class"] != ClassPerson {
		t.Fatalf("%v %v", out, items)
	}

	var path string
	if err := restored.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "work.yaml")
	if err := os.WriteFile(cfg, []byte("db_path: "+path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CAN_WORK_CONFIG": cfg, "CAN_WORK_ADMIN_TOKEN": bootstrap}
	var stdout, stderr bytes.Buffer
	// The command can-core runs in a Job (core/server/dumps RunPurgeCLI).
	if code := Main([]string{"purge", "-subject", subject, "-class", ClassPerson}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("%d %s%s", code, stdout.String(), stderr.String())
	}
	if left := held(t, restored, leaver); len(left) != 0 {
		t.Fatalf("still held: %v", left)
	}
}
