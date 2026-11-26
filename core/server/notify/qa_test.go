package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// QA of #52, 4: a module's manifest chooses the rule type, so mailed text spaces its dots out: mail readers
// turn "some.host.example" into a link.
func TestRuleTypesCannotBeLinkedInMail(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := store.Module{ID: "m", Version: "1", Namespace: "mod-m", ServiceAccount: "m", State: store.StateEnabled, HMACSecret: "s",
		Manifest: config.Module{ID: "m", Notifications: []config.NotifyRule{{Event: "lead.created.v1", Type: "m.reset.evil.example", Recipients: []string{"user:user:alice", "user:user:bob"}}}}}
	if err := e.db.UpsertModule(ctx, m, nil, nil); err != nil {
		t.Fatal(err)
	}
	e.setEmail("user:alice", "alice@acme.example")
	if err := e.s.SetSettings(ctx, Settings{Subject: "user:bob", Email: "bob@acme.example", QuietStart: -1, QuietEnd: -1, TZ: "UTC", Digest: "hourly"}); err != nil {
		t.Fatal(err)
	}
	e.bus.SubscribeAll(e.s.HandleEvent)
	// can-core's own event: the mail is the platform's wording, but the type is still the module's.
	if _, err := e.bus.Publish(ctx, "can-core", "lead.created.v1", json.RawMessage(`{"lead_id":"l1"}`)); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(2 * time.Hour)
	if _, err := e.s.FlushDigests(ctx); err != nil {
		t.Fatal(err)
	}
	sent := e.mail.Sent()
	if len(sent) != 2 {
		t.Fatalf("%d messages", len(sent))
	}
	for _, msg := range sent {
		if all := msg.Subject + msg.Text + msg.HTML; strings.Contains(all, "evil.example") || !strings.Contains(all, "evil / example") {
			t.Errorf("linkable type: %q %q %q", msg.Subject, msg.Text, msg.HTML)
		}
	}
}
