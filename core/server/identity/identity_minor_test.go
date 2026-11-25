package identity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRevokeDisablesTheAccountAndDropsPushSubscriptions(t *testing.T) {
	e := newEnv(t)
	e.seed()
	if _, err := e.db.ExecContext(context.Background(), `INSERT INTO push_subscriptions(subject, endpoint, p256dh, auth, created_at) VALUES (?,?,?,?,1),(?,?,?,?,1)`,
		"user:sso-abc", "https://push.example/a", "p", "a", "user:sso-keep", "https://push.example/b", "p", "a"); err != nil {
		t.Fatal(err)
	}
	e.offboard("people", party, "2026-06-10", subj)
	if _, err := e.svc.Revoke(context.Background(), provisioner(), Request{PartyID: party, Subject: subj}); err != nil {
		t.Fatal(err)
	}
	if !e.db.AdminDisabled(context.Background(), 7) || e.db.AdminDisabled(context.Background(), 8) {
		t.Fatal("only the leaver's account may be disabled")
	}
	var n int
	_ = e.db.QueryRow(`SELECT count(*) FROM push_subscriptions`).Scan(&n)
	if n != 1 {
		t.Fatalf("push subscriptions left: %d", n)
	}
	if !e.db.AdminDisabled(context.Background(), 999) {
		t.Fatal("an unknown account must count as disabled (fail closed)")
	}
}

func TestEntitlementRequiresBeingTheResolvedProvider(t *testing.T) {
	e := newEnv(t)
	e.offboard("people", party, "2026-06-01", subj)
	e.svc.Providers = func(context.Context, string) ([]string, error) { return []string{"someone-else"}, nil }
	if _, err := e.svc.Revoke(context.Background(), provisioner(), Request{PartyID: party, Subject: subj}); !errors.Is(err, ErrNotEntitled) {
		t.Fatalf("err = %v", err)
	}
	e.svc.Providers = func(context.Context, string) ([]string, error) { return []string{"provisioning"}, nil }
	if _, err := e.svc.Revoke(context.Background(), provisioner(), Request{PartyID: party, Subject: subj}); err != nil {
		t.Fatal(err)
	}
	e.svc.Providers = func(context.Context, string) ([]string, error) { return nil, errors.New("registry down") }
	if _, err := e.svc.Revoke(context.Background(), provisioner(), Request{PartyID: party, Subject: subj}); err == nil {
		t.Fatal("a registry failure must not allow the call")
	}
}

func TestReconcileRebuildsFactsFromStoredEvents(t *testing.T) {
	e := newEnv(t)
	good, _ := json.Marshal(map[string]any{"party_id": party, "kind": "employee", "effective_date": "2026-06-01", "subject_ref": SubjectRef(subj)})
	forged, _ := json.Marshal(map[string]any{"party_id": other, "kind": "employee", "effective_date": "2026-06-01", "subject_ref": SubjectRef(subj)})
	for _, q := range [][]any{{"e1", "people", string(good)}, {"e2", "jobs", string(forged)}} {
		if _, err := e.db.ExecContext(context.Background(), `INSERT INTO events(id,type,time,source,data_json) VALUES (?, 'person.offboarded.v1', 1, ?, ?)`, q...); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.svc.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Revoke(context.Background(), provisioner(), Request{PartyID: party, Subject: subj}); err != nil {
		t.Fatalf("a recorded fact was not rebuilt: %v", err)
	}
	if _, err := e.svc.Revoke(context.Background(), provisioner(), Request{PartyID: other, Subject: subj}); !errors.Is(err, ErrNotOffboarded) {
		t.Fatalf("a forged event was rebuilt into a fact: %v", err)
	}
}

func TestRevokeNowIsAudited(t *testing.T) {
	e := newEnv(t)
	e.seed()
	res, err := e.svc.RevokeNow(context.Background(), "user:boss", subj)
	if err != nil || res.Tokens != 2 || res.Sessions != 2 || !e.db.AdminDisabled(context.Background(), 7) {
		t.Fatalf("%+v %v", res, err)
	}
	var n int
	_ = e.db.QueryRow(`SELECT count(*) FROM audit_log WHERE action='identity.revoke_now' AND actor='user:boss' AND target=?`, subj).Scan(&n)
	if n != 1 {
		t.Fatalf("audit entries %d", n)
	}
	if _, err := e.svc.RevokeNow(context.Background(), "user:boss", "admin"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}
