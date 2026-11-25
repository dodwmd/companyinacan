package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDisabledModulesGetNoDeliveriesAndDoNotBlockPruning(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	now := time.Unix(5000, 0)
	_ = db.UpsertModule(ctx, Module{ID: "crm", Version: "1", Namespace: "mod-crm", ServiceAccount: "crm", State: StateEnabled, HMACSecret: "s"}, nil, nil)
	_ = db.InsertEvent(ctx, Event{ID: "e1", Type: "lead.created.v1", Time: now, Source: "x", DataJSON: "{}"}, []DeliveryTarget{{ModuleID: "crm", URL: "http://x"}})
	_ = db.SetModuleState(ctx, "crm", StateDisabled, "")
	if due, _ := db.DueDeliveries(ctx, now, 10); len(due) != 0 {
		t.Fatalf("delivered to a disabled module: %+v", due)
	}
	if n, _ := db.PruneEvents(ctx, now.Add(time.Hour)); n != 1 {
		t.Fatalf("a disabled module's pending delivery blocks pruning forever (pruned %d)", n)
	}
	// re-enabled modules receive what is still pending
	_ = db.InsertEvent(ctx, Event{ID: "e2", Type: "lead.created.v1", Time: now, Source: "x", DataJSON: "{}"}, []DeliveryTarget{{ModuleID: "crm", URL: "http://x"}})
	_ = db.SetModuleState(ctx, "crm", StateEnabled, "")
	if due, _ := db.DueDeliveries(ctx, now, 10); len(due) != 1 {
		t.Fatalf("%+v", due)
	}
}

func TestRequeueDeadDelivery(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	now := time.Unix(5000, 0)
	_ = db.UpsertModule(ctx, Module{ID: "crm", Version: "1", Namespace: "mod-crm", ServiceAccount: "crm", State: StateEnabled, HMACSecret: "s"}, nil, nil)
	_ = db.InsertEvent(ctx, Event{ID: "e1", Type: "lead.created.v1", Time: now, Source: "x", DataJSON: "{}"}, []DeliveryTarget{{ModuleID: "crm", URL: "http://x"}})
	due, _ := db.DueDeliveries(ctx, now, 1)
	if err := db.RequeueDelivery(ctx, due[0].ID, now); !errors.Is(err, ErrNotDead) {
		t.Fatalf("pending delivery requeued: %v", err)
	}
	_ = db.RecordAttempt(ctx, due[0].ID, Attempt{At: now, Error: "x"}, StatusDead, now)
	if due, _ := db.DueDeliveries(ctx, now, 10); len(due) != 0 {
		t.Fatal("dead delivery is due")
	}
	if err := db.RequeueDelivery(ctx, due0ID(t, db), now); err != nil {
		t.Fatal(err)
	}
	d, _ := db.DueDeliveries(ctx, now, 10)
	if len(d) != 1 || d[0].Attempts != 0 {
		t.Fatalf("%+v", d)
	}
	if err := db.RequeueDelivery(ctx, 9999, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func due0ID(t *testing.T, db *DB) int64 {
	t.Helper()
	ds, err := db.ListDeliveries(context.Background(), 1)
	if err != nil || len(ds) != 1 {
		t.Fatal(err)
	}
	return ds[0].ID
}
