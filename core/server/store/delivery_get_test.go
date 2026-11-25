package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGetDelivery(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	now := time.Unix(5000, 0)
	_ = db.UpsertModule(ctx, Module{ID: "crm", Version: "1", Namespace: "mod-crm", ServiceAccount: "crm", State: StateEnabled, HMACSecret: "s"}, nil, nil)
	_ = db.InsertEvent(ctx, Event{ID: "e1", Type: "lead.created.v1", Time: now, Source: "forms", DataJSON: "{}"}, []DeliveryTarget{{ModuleID: "crm", URL: "http://x"}})
	list, _ := db.ListDeliveries(ctx, 1)
	d, err := db.GetDelivery(ctx, list[0].ID)
	if err != nil || d.Event.Type != "lead.created.v1" || d.ModuleID != "crm" || d.HMACSecret != "" {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := db.GetDelivery(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}
