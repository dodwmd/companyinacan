package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestPublishToDeliversOnlyToTheNamedModule(t *testing.T) {
	ctx := context.Background()
	b, db := newBus(t)
	mod(t, db, "expenses", store.StateEnabled, sub("expenses", "approval.decided.v1"))
	mod(t, db, "vendors", store.StateEnabled, sub("vendors", "approval.decided.v1"))
	if _, err := b.PublishTo(ctx, "can-core", "approval.decided.v1", json.RawMessage(`{"request_id":"apr_1"}`), "expenses"); err != nil {
		t.Fatal(err)
	}
	ds, _ := db.ListDeliveries(ctx, 10)
	if len(ds) != 1 || ds[0].ModuleID != "expenses" {
		t.Fatalf("deliveries = %+v, want exactly one, to expenses", ds)
	}
}

func TestSubscribeAllSeesEveryEventAndPanicsAreContained(t *testing.T) {
	ctx := context.Background()
	b, _ := newBus(t)
	var seen []string
	b.SubscribeAll(func(_ context.Context, _ Envelope) { panic("boom") })
	b.SubscribeAll(func(_ context.Context, e Envelope) { seen = append(seen, e.Type) })
	for _, ty := range []string{"lead.created.v1", "deal.won.v1"} {
		if _, err := b.Publish(ctx, "forms", ty, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("seen = %v", seen)
	}
}

func TestCatalogueHasApprovalDecidedAndNotificationSent(t *testing.T) {
	for _, ev := range []string{"approval.decided.v1", "notification.sent.v1"} {
		if !Known(ev) {
			t.Errorf("%s missing from the catalogue", ev)
		}
	}
}
