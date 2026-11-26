package work

import (
	"context"
	"encoding/json"
	"testing"
)

func p0Events(t *testing.T, h *harness) []issueP0Data {
	t.Helper()
	rows, err := h.app.store.db.QueryContext(context.Background(), `SELECT payload FROM outbox WHERE type = ? ORDER BY id`, EventIssueP0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []issueP0Data
	for rows.Next() {
		var p string
		var ev struct {
			Data issueP0Data `json:"data"`
		}
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(p), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev.Data)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestP0EventWhenRaisedOrHandedOver(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Webhook.URL = "http://127.0.0.1:9/never" })
	writer := h.setup()
	create := func(priority, assignee string) {
		body := map[string]any{"type": "bug", "title": "secret title", "priority": priority}
		if assignee != "" {
			body["assignee"] = assignee
		}
		h.mustDo(201, "POST", "/v1/projects/ACME/issues", writer, body)
	}
	step := func(want int, why string) {
		t.Helper()
		if got := p0Events(t, h); len(got) != want {
			t.Fatalf("%s: %d events %+v", why, len(got), got)
		}
	}
	create("P1", "user:ann")
	step(0, "a P1 asks for nothing")
	create("P0", "user:ann")
	step(1, "created as P0")
	h.mustDo(200, "PATCH", "/v1/issues/ACME-2", writer, map[string]any{"title": "still secret"})
	step(1, "an edit of a P0")
	h.mustDo(200, "PATCH", "/v1/issues/ACME-2", writer, map[string]any{"assignee": "user:ben"})
	step(2, "handed over")
	h.mustDo(200, "PATCH", "/v1/issues/ACME-2", writer, map[string]any{"assignee": nil})
	step(2, "unassigned")
	h.mustDo(200, "PATCH", "/v1/issues/ACME-1", writer, map[string]any{"priority": "P0"})
	step(3, "raised to P0")
	got := p0Events(t, h)
	if got[0].IssueID != "ACME-2" || got[0].Project != "ACME" || got[1].Assignee == nil || *got[1].Assignee != "user:ben" {
		t.Fatalf("%+v", got)
	}
}
