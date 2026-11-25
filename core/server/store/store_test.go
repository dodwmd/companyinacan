package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := db.SchemaVersion(ctx)
	if err != nil || v < 1 {
		t.Fatalf("version=%d err=%v", v, err)
	}
	if _, err := db.CreateAdmin(ctx, "root", "hash", "SECRET"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	db2, err := Open(ctx, path) // re-open applies no migration twice and keeps data
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	if v2, _ := db2.SchemaVersion(ctx); v2 != v {
		t.Fatalf("version changed %d -> %d", v, v2)
	}
	if _, err := db2.AdminByUsername(ctx, "root"); err != nil {
		t.Fatalf("data lost: %v", err)
	}
}

func TestAdminAndTOTPReplayGuard(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	u, err := db.CreateAdmin(ctx, "root", "h", "S")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAdmin(ctx, "root", "h", "S"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate admin: %v", err)
	}
	if _, err := db.AdminByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing admin: %v", err)
	}
	ok, err := db.AdvanceTOTPStep(ctx, u.ID, 100)
	if err != nil || !ok {
		t.Fatalf("first use: ok=%v err=%v", ok, err)
	}
	if ok, _ := db.AdvanceTOTPStep(ctx, u.ID, 100); ok {
		t.Fatal("same step accepted twice (replay)")
	}
	if ok, _ := db.AdvanceTOTPStep(ctx, u.ID, 99); ok {
		t.Fatal("older step accepted")
	}
	if ok, _ := db.AdvanceTOTPStep(ctx, u.ID, 101); !ok {
		t.Fatal("newer step rejected")
	}
}

func TestSessionsLifecycle(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	u, _ := db.CreateAdmin(ctx, "root", "h", "S")
	now := time.Unix(1000, 0)
	if err := db.CreateSession(ctx, Session{IDHash: "a", UserID: u.ID, CSRF: "c", IP: "1.2.3.4", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	s, err := db.SessionByHash(ctx, "a")
	if err != nil || s.CSRF != "c" || !s.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("%+v %v", s, err)
	}
	n, _ := db.DeleteExpiredSessions(ctx, now.Add(2*time.Hour))
	if n != 1 {
		t.Fatalf("expired deleted = %d", n)
	}
	if _, err := db.SessionByHash(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session should be gone: %v", err)
	}
}

func TestModulesProvidersAndSubscriptions(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	m := Module{ID: "crm", Version: "1.0.0", Namespace: "mod-crm", ServiceAccount: "crm", State: StateEnabled, HMACSecret: "s", Manifest: config.Module{ID: "crm"}}
	prov := []Provider{{Capability: "contacts", ModuleID: "crm", Service: "crm", Port: 8080}}
	subs := []Subscription{{ModuleID: "crm", Event: "lead.created.v1", Service: "crm", Port: 8081, Webhook: "/hooks/lead"}}
	if err := db.UpsertModule(ctx, m, prov, subs); err != nil {
		t.Fatal(err)
	}
	got, err := db.ModuleByIdentity(ctx, "mod-crm", "crm")
	if err != nil || got.ID != "crm" || got.Manifest.ID != "crm" {
		t.Fatalf("%+v %v", got, err)
	}
	ps, _ := db.Providers(ctx, "contacts")
	if len(ps) != 1 || ps[0].Port != 8080 {
		t.Fatalf("providers %+v", ps)
	}
	ss, _ := db.Subscribers(ctx, "lead.created.v1")
	if len(ss) != 1 || ss[0].HMACSecret != "s" {
		t.Fatalf("subs %+v", ss)
	}
	// disabled modules neither provide nor subscribe
	if err := db.SetModuleState(ctx, "crm", StateDisabled, ""); err != nil {
		t.Fatal(err)
	}
	if ps, _ := db.Providers(ctx, "contacts"); len(ps) != 0 {
		t.Fatalf("disabled module still provides: %+v", ps)
	}
	if ss, _ := db.Subscribers(ctx, "lead.created.v1"); len(ss) != 0 {
		t.Fatalf("disabled module still subscribed: %+v", ss)
	}
	// re-upsert replaces rows rather than duplicating them
	if err := db.UpsertModule(ctx, m, prov, subs); err != nil {
		t.Fatal(err)
	}
	if ps, _ := db.Providers(ctx, "contacts"); len(ps) != 1 {
		t.Fatalf("upsert duplicated providers: %+v", ps)
	}
}

func TestEventsOutbox(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	_ = db.UpsertModule(ctx, Module{ID: "crm", Version: "1", Namespace: "mod-crm", ServiceAccount: "crm", State: StateEnabled, HMACSecret: "s"}, nil, nil)
	now := time.Unix(5000, 0)
	ev := Event{ID: "e1", Type: "lead.created.v1", Time: now, Source: "forms", DataJSON: `{"a":1}`}
	if err := db.InsertEvent(ctx, ev, []DeliveryTarget{{ModuleID: "crm", URL: "http://x/h"}}); err != nil {
		t.Fatal(err)
	}
	due, _ := db.DueDeliveries(ctx, now, 10)
	if len(due) != 1 || due[0].Event.ID != "e1" || due[0].HMACSecret != "s" {
		t.Fatalf("due %+v", due)
	}
	if due, _ := db.DueDeliveries(ctx, now.Add(-time.Second), 10); len(due) != 0 {
		t.Fatal("not yet due")
	}
	if err := db.RecordAttempt(ctx, due[0].ID, Attempt{At: now, StatusCode: 500, Error: "boom"}, StatusPending, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if due, _ := db.DueDeliveries(ctx, now, 10); len(due) != 0 {
		t.Fatal("backoff ignored")
	}
	ds, _ := db.ListDeliveries(ctx, 10)
	if len(ds) != 1 || ds[0].Attempts != 1 {
		t.Fatalf("%+v", ds)
	}
	as, _ := db.Attempts(ctx, ds[0].ID)
	if len(as) != 1 || as[0].StatusCode != 500 {
		t.Fatalf("%+v", as)
	}
}
