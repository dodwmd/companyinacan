package work

import (
	"strings"
	"testing"
	"time"
)

var qNow = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

func mustSQL(t *testing.T, q string) (string, []any) {
	t.Helper()
	f, err := ParseQuery(q, qNow, "alice@example.com")
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", q, err)
	}
	return f.SQL()
}

func TestQueryEmptyMatchesAll(t *testing.T) {
	w, args := mustSQL(t, "   ")
	if w != "1=1" || len(args) != 0 {
		t.Fatalf("got %q %v", w, args)
	}
}

func TestQueryTermsAreParameterised(t *testing.T) {
	evil := `x'; DROP TABLE issues; --`
	w, args := mustSQL(t, `status:todo,in_progress priority:P0 label:"`+evil+`" "`+evil+`"`)
	if strings.Contains(w, "DROP") || strings.Contains(w, evil) {
		t.Fatalf("user input leaked into SQL: %s", w)
	}
	// Every placeholder has an argument.
	if strings.Count(w, "?") != len(args) {
		t.Fatalf("placeholders %d != args %d in %s", strings.Count(w, "?"), len(args), w)
	}
	found := 0
	for _, a := range args {
		if s, ok := a.(string); ok && strings.Contains(s, "DROP TABLE") {
			found++
		}
	}
	if found == 0 {
		t.Fatal("injection payload should travel as an argument")
	}
}

func TestQueryFields(t *testing.T) {
	cases := []struct {
		q       string
		wantSQL string
		args    int
	}{
		{"status:todo", "i.status IN (?)", 1},
		{"priority:P0,P1", "i.priority IN (?,?)", 2},
		{"assignee:me", "i.assignee IN (?)", 1},
		{"assignee:none", "i.assignee IS NULL", 0},
		{"-status:done", "NOT (i.status IN (?))", 1},
		{"is:open", "i.resolved_at IS NULL", 0},
		{"sprint:active", "sprints", 0},
		{"parent:ACME-3", "i.parent_id IN", 2},
		{"key:ACME-3", "p.key = ?", 2},
		{"label:none", "NOT EXISTS", 0},
		{"due<2026-12-01", "i.due_date < ?", 1},
		{"created>=-7d", "i.created_at >= ?", 1},
		{"login bug", "LIKE ? ESCAPE", 4},
	}
	for _, tc := range cases {
		w, args := mustSQL(t, tc.q)
		if !strings.Contains(w, tc.wantSQL) {
			t.Errorf("%q: SQL %q lacks %q", tc.q, w, tc.wantSQL)
		}
		if len(args) != tc.args {
			t.Errorf("%q: %d args, want %d (%s)", tc.q, len(args), tc.args, w)
		}
	}
}

func TestQueryMeAndRelativeDates(t *testing.T) {
	_, args := mustSQL(t, "assignee:me")
	if args[0] != "alice@example.com" {
		t.Fatalf("me not resolved: %v", args)
	}
	_, args = mustSQL(t, "due<today")
	if args[0] != "2026-11-25" {
		t.Fatalf("today: %v", args)
	}
	_, args = mustSQL(t, "updated>-1d")
	if args[0] != qNow.Add(-24*time.Hour).Unix() {
		t.Fatalf("-1d: %v", args)
	}
}

func TestQueryLikeEscaping(t *testing.T) {
	_, args := mustSQL(t, `100%_done`)
	if args[0] != `%100\%\_done%` {
		t.Fatalf("wildcards must be escaped: %v", args)
	}
}

func TestQueryRejects(t *testing.T) {
	for _, q := range []string{
		"nosuch:field",
		"status:",
		"priority:P9",
		"status:" + strings.Repeat("a,", 30) + "b",
		strings.Repeat("a ", 30),
		strings.Repeat("x", 600),
		"due<notadate",
		"status<todo",
		"sprint:abc",
		"parent:bogus",
		`status:"unterminated`,
		"is:weird",
	} {
		if _, err := ParseQuery(q, qNow, "me"); err == nil {
			t.Errorf("%.40q should be rejected", q)
		}
	}
}
