package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/store"
)

func newBus(t *testing.T) (*Bus, *store.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return NewBus(db), db
}

func mod(t *testing.T, db *store.DB, id, state string, subs ...store.Subscription) {
	t.Helper()
	if err := db.UpsertModule(context.Background(), store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: state, HMACSecret: "s-" + id}, nil, subs); err != nil {
		t.Fatal(err)
	}
}

func sub(id, ev string) store.Subscription {
	return store.Subscription{ModuleID: id, Event: ev, Service: id, Port: 8000, Webhook: "/hooks/" + id}
}

func TestCatalogueAndNameValidation(t *testing.T) {
	for _, ev := range []string{"lead.created.v1", "deal.won.v1", "approval.requested.v1", "issue.priority_changed.v1", "one_on_one.action_item.created.v1", "person.left.v1"} {
		if !Known(ev) {
			t.Errorf("%s missing from the catalogue", ev)
		}
		if !ValidType(ev) {
			t.Errorf("%s fails validation", ev)
		}
	}
	for _, bad := range []string{"", "lead", "lead.created", "Lead.Created.v1", "lead.created.v0", "lead..created.v1", "lead.created.v1 "} {
		if ValidType(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if Known("lead.created.v2") || Known("nope.v1") {
		t.Fatal("Known must be exact")
	}
}

func TestPublishPersistsAndFansOutToMatchingEnabledSubscribers(t *testing.T) {
	ctx := context.Background()
	b, db := newBus(t)
	mod(t, db, "crm", store.StateEnabled, sub("crm", "lead.created.v1"))
	mod(t, db, "support", store.StateEnabled, sub("support", "lead.created.v1"), sub("support", "deal.won.v1"))
	mod(t, db, "off", store.StateDisabled, sub("off", "lead.created.v1"))
	mod(t, db, "other", store.StateEnabled, sub("other", "invoice.paid.v1"))

	env, err := b.Publish(ctx, "forms", "lead.created.v1", json.RawMessage(`{"email":"a@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env.ID, "evt_") || env.Source != "forms" || env.Type != "lead.created.v1" {
		t.Fatalf("%+v", env)
	}
	ds, _ := db.ListDeliveries(ctx, 10)
	got := map[string]string{}
	for _, d := range ds {
		got[d.ModuleID] = d.URL
	}
	if len(got) != 2 || got["crm"] != "http://crm.mod-crm.svc.cluster.local:8000/hooks/crm" || got["support"] == "" {
		t.Fatalf("deliveries %+v", got)
	}
	// persisted before delivery: the outbox row exists with the payload
	due, _ := db.DueDeliveries(ctx, time.Unix(1_700_000_000, 0), 10)
	if len(due) != 2 || due[0].Event.DataJSON != `{"email":"a@example.com"}` {
		t.Fatalf("%+v", due)
	}
}

func TestPublishWithNoSubscribersStillStoresEvent(t *testing.T) {
	b, db := newBus(t)
	if _, err := b.Publish(context.Background(), "forms", "lead.created.v1", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	if n != 1 {
		t.Fatalf("events=%d", n)
	}
}

func TestPublishValidation(t *testing.T) {
	b, _ := newBus(t)
	ctx := context.Background()
	if _, err := b.Publish(ctx, "x", "bad", json.RawMessage(`{}`)); err == nil {
		t.Error("bad type accepted")
	}
	if _, err := b.Publish(ctx, "x", "lead.created.v1", json.RawMessage(`not json`)); err == nil {
		t.Error("non-JSON data accepted")
	}
	if _, err := b.Publish(ctx, "x", "lead.created.v1", json.RawMessage(`[1]`)); err == nil {
		t.Error("non-object data accepted")
	}
	if _, err := b.Publish(ctx, "x", "lead.created.v1", json.RawMessage(strings.Repeat(" ", MaxDataBytes)+`{}`)); err == nil {
		t.Error("oversized data accepted")
	}
	if _, err := b.Publish(ctx, "", "lead.created.v1", nil); err == nil {
		t.Error("empty source accepted")
	}
}

func TestInProcessSubscribers(t *testing.T) {
	b, _ := newBus(t)
	var got []Envelope
	b.Subscribe("approval.requested.v1", func(_ context.Context, e Envelope) { got = append(got, e) })
	b.Subscribe("approval.requested.v1", func(context.Context, Envelope) { panic("handler bug") }) // must not break publish
	if _, err := b.Publish(context.Background(), "can-core", "approval.requested.v1", json.RawMessage(`{"id":"a1"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(context.Background(), "can-core", "approval.approved.v1", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "approval.requested.v1" {
		t.Fatalf("%+v", got)
	}
}

func TestWakeSignalsDispatcher(t *testing.T) {
	b, db := newBus(t)
	mod(t, db, "crm", store.StateEnabled, sub("crm", "lead.created.v1"))
	if _, err := b.Publish(context.Background(), "x", "lead.created.v1", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.Wake():
	default:
		t.Fatal("no wake signal after publish")
	}
}

func TestEnvelopeJSONShape(t *testing.T) {
	e := Envelope{ID: "evt_1", Type: "lead.created.v1", Time: time.Unix(1_700_000_000, 0), Source: "forms", Data: json.RawMessage(`{"a":1}`)}
	var m map[string]any
	if err := json.Unmarshal(e.JSON(), &m); err != nil {
		t.Fatal(err)
	}
	if m["id"] != "evt_1" || m["type"] != "lead.created.v1" || m["source"] != "forms" || m["time"] != "2023-11-14T22:13:20Z" || m["data"].(map[string]any)["a"] != float64(1) {
		t.Fatalf("%v", m)
	}
}
