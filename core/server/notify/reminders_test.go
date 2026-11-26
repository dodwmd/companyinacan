package notify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestEventDataKeepsIdsAndCountsOnly(t *testing.T) {
	got := idData(json.RawMessage(`{"invoice_id": 12, "issue_id": "WEB-3", "count": 41, "name": "Ada", "owner": "user:ada",
		"amount_minor": 500, "ratio_id": 1.5, "note_id": {"x": 1}, "title": "t", "due_date": "2026-01-01"}`))
	if len(got) != 3 || got["invoice_id"] != "12" || got["issue_id"] != "WEB-3" || got["count"] != "41" {
		t.Fatalf("%v", got)
	}
	if got := idData(json.RawMessage(`{"count": "many people"}`)); len(got) != 0 {
		t.Fatalf("a count is a number: %v", got)
	}
}

func TestRoleNobodyHoldsFallsBackToOwners(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	err := e.db.UpsertModule(ctx, store.Module{ID: "finance", Version: "1", Namespace: "mod-finance", ServiceAccount: "finance", State: store.StateEnabled,
		Manifest: config.Module{ID: "finance", Notifications: []config.NotifyRule{
			{Event: "invoice.overdue.v1", Recipients: []string{"role:finance"}},
			{Event: "expense.decided.v1", Recipients: []string{"field:owner"}},
		}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.roles["owner"] = []string{"user:boss"}
	e.bus.SubscribeAll(e.s.HandleEvent)
	publish := func(typ, data string) {
		t.Helper()
		if _, err := e.bus.Publish(ctx, "finance", typ, json.RawMessage(data)); err != nil {
			t.Fatal(err)
		}
	}
	unread := func(s string) int {
		t.Helper()
		n, err := e.s.UnreadCount(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	publish("invoice.overdue.v1", `{"invoice_id": 7, "party_id": 2}`)
	if unread("user:boss") != 1 {
		t.Fatal("nobody holds finance:finance: the owner is told")
	}
	items, err := e.s.List(ctx, "user:boss", ListOptions{Limit: 5})
	if err != nil || items[0].Title != "Invoice overdue" || !strings.Contains(items[0].Body, "Invoice 7 ") {
		t.Fatalf("%+v %v", items, err)
	}
	// A person who is not an account is not replaced by the owner.
	publish("expense.decided.v1", `{"expense_id": 3, "owner": "person-9"}`)
	if unread("user:boss") != 1 {
		t.Fatal("somebody else's own business is not the owner's")
	}
	e.roles["finance:finance"] = []string{"user:fin"}
	publish("invoice.overdue.v1", `{"count": 12}`)
	if unread("user:boss") != 1 || unread("user:fin") != 1 {
		t.Fatalf("boss %d fin %d", unread("user:boss"), unread("user:fin"))
	}
	items, err = e.s.List(ctx, "user:fin", ListOptions{Limit: 5})
	if err != nil || items[0].Title != "Invoices overdue" || !strings.HasPrefix(items[0].Body, "12 invoices ") {
		t.Fatalf("%+v %v", items, err)
	}
	// The recipient's preference holds for these like for any other type.
	if err := e.s.SetPref(ctx, "user:fin", "invoice.overdue", ChannelInApp, false); err != nil {
		t.Fatal(err)
	}
	publish("invoice.overdue.v1", `{"invoice_id": 8}`)
	if unread("user:fin") != 1 {
		t.Fatal("muted types stay muted")
	}
}
