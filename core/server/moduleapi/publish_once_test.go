package moduleapi

import (
	"encoding/json"
	"testing"
)

func TestPublishWithSourceIDIsIdempotent(t *testing.T) {
	h := newHarness(t)
	body := map[string]any{"type": "lead.created.v1", "data": map[string]any{"lead_id": "l1"}, "source_id": "evt_form_1"}
	resp, b1 := h.call("POST", "/v1/events", "tok-forms", body)
	if resp.StatusCode != 202 {
		t.Fatalf("%d %s", resp.StatusCode, b1)
	}
	resp, b2 := h.call("POST", "/v1/events", "tok-forms", body)
	if resp.StatusCode != 202 {
		t.Fatalf("retry: %d %s", resp.StatusCode, b2)
	}
	var a, b struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(b1, &a)
	_ = json.Unmarshal(b2, &b)
	if a.ID == "" || a.ID != b.ID {
		t.Fatalf("a retried publish created a second event: %s vs %s", a.ID, b.ID)
	}
	var n int
	if err := h.db.QueryRow(`SELECT count(*) FROM deliveries`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("deliveries: %d %v (one subscriber, one event)", n, err)
	}
}

func TestPublishRejectsAMalformedSourceID(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"has space", "a\nb", string(make([]byte, 300))} {
		resp, _ := h.call("POST", "/v1/events", "tok-forms", map[string]any{"type": "lead.created.v1", "data": map[string]any{}, "source_id": id})
		if resp.StatusCode != 400 {
			t.Errorf("%q -> %d", id, resp.StatusCode)
		}
	}
}
