package moduleapi

import (
	"context"
	"testing"
)

// A module cannot forge platform events: an approval.decided.v1 from a module would be taken by
// every receiver for a decision of can-core.
func TestModulesCannotPublishPlatformEvents(t *testing.T) {
	p := newPlatform(t, true)
	for _, ev := range []string{"approval.decided.v1", "approval.requested.v1", "notification.sent.v1", "approval.approved.v1"} {
		resp, body := p.call("POST", "/v1/events", "tok-expenses", nil, map[string]any{"type": ev, "data": map[string]string{"request_id": "apr_0123456789abcdef01234567", "outcome": "approved"}})
		if resp.StatusCode != 403 {
			t.Errorf("%s: %d %s", ev, resp.StatusCode, body)
		}
	}
	var n int
	_ = p.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM events`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d events were stored", n)
	}
}

func TestNotificationDedupeKeysAreScopedPerModule(t *testing.T) {
	p := newPlatform(t, true)
	body := map[string]any{"subject": "user:fin", "type": "expenses.reminder", "dedupe_key": "same"}
	if _, out := p.call("POST", "/v1/notifications", "tok-expenses", nil, body); out == "" {
		t.Fatal("no answer")
	}
	// The key is stored with the module id prefix, so another module's identical key is distinct.
	var key string
	_ = p.db.QueryRowContext(context.Background(), `SELECT dedupe_key FROM notifications LIMIT 1`).Scan(&key)
	if key != "expenses:same" {
		t.Fatalf("stored key = %q", key)
	}
}
