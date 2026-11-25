package work

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Hostile free text is stored and returned literally (it only ever travels as a bound
// parameter), never breaks the event JSON that goes to webhook receivers, and leaves the
// schema intact. Control characters that could forge log lines or split a title are refused.
func TestHostileTextIsLiteralAndEventsStayValidJSON(t *testing.T) {
	s, _ := newTestStore(t)
	mustProject(t, s, "ACME")
	ctx := context.Background()
	literal := []string{
		`'); DROP TABLE issues; --`,
		`" OR 1=1 --`,
		`50% off_ \ backslash`,
		`</script><img src=x onerror=alert(1)>`,
		"right-to-left \u202eoverride and zero\u200bwidth",
		`{"event":"forged"}`,
	}
	for _, title := range literal {
		is, _, err := s.CreateIssue(ctx, alice, "ACME", CreateIssue{Type: "task", Title: title, Description: title}, "")
		if err != nil {
			t.Fatalf("%q: %v", title, err)
		}
		d, err := s.GetIssue(ctx, alice, is.Key)
		if err != nil || d.Title != title || d.Description != title {
			t.Fatalf("%q did not round-trip: %+v %v", title, d, err)
		}
		if _, err := s.AddComment(ctx, alice, is.Key, title); err != nil {
			t.Fatalf("comment %q: %v", title, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM issues`).Scan(&n); err != nil || n != len(literal) {
		t.Fatalf("issues table damaged: %d %v", n, err)
	}
	rows, err := s.db.Query(`SELECT type, payload FROM outbox`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	events := 0
	for rows.Next() {
		var typ, payload string
		_ = rows.Scan(&typ, &payload)
		events++
		var v map[string]any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Fatalf("event %s is not valid JSON: %v\n%s", typ, err, payload)
		}
		if v["type"] != typ {
			t.Errorf("event type forged by text: %v vs %s", v["type"], typ)
		}
		if strings.Contains(payload, "description") && typ == "work.comment.created.v1" {
			t.Errorf("comment event carries text: %s", payload)
		}
	}
	if events == 0 {
		t.Fatal("no events were recorded")
	}

	for _, bad := range []string{"line\nbreak", "carriage\rreturn", "nul\x00byte", "bell\x07"} {
		if _, _, err := s.CreateIssue(ctx, alice, "ACME", CreateIssue{Type: "task", Title: bad}, ""); err == nil {
			t.Errorf("title %q was accepted", bad)
		}
	}
	// hostile labels and assignees are validated, not interpreted
	for _, in := range []CreateIssue{
		{Type: "task", Title: "x", Labels: []string{"a'; drop table issues;--"}},
		{Type: "task", Title: "x", Assignee: ptr("bob'; --\n")},
	} {
		if _, _, err := s.CreateIssue(ctx, alice, "ACME", in, ""); err == nil {
			t.Errorf("hostile field accepted: %+v", in)
		}
	}
}
