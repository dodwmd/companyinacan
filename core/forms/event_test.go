package forms

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNewLeadCreated(t *testing.T) {
	ev := testEvent()
	if ev.Type != "lead.created.v1" || ev.Source != "forms" || !strings.HasPrefix(ev.ID, "evt_") {
		t.Fatalf("%+v", ev)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"id", "type", "time", "source", "data"} {
		if _, ok := m[k]; !ok {
			t.Errorf("envelope missing %q: %s", k, b)
		}
	}
	data := m["data"].(map[string]any)
	if data["lead_id"] != "ld_1" || data["form_id"] != "contact" || data["email"] != "a@example.com" || data["submitted_at"] != "2026-11-25T01:02:03Z" {
		t.Fatalf("data: %v", data)
	}
	if m["time"] != "2026-11-25T01:02:04Z" {
		t.Fatalf("time: %v", m["time"])
	}
}

func TestEventIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewLeadCreated(&Lead{ID: "x", CreatedAt: time.Now()}, time.Now()).ID
		if seen[id] {
			t.Fatal("duplicate event id")
		}
		seen[id] = true
	}
}
