package reconcile

import (
	"context"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/modsecrets"
)

func TestRotationRequestTriggersAnUpgradeAndIsConsumed(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	d := desired(nil, nil, mf("people", "0.1.0", nil))
	if _, err := g.r.Apply(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	first := g.fk.Enabled["people"].HMACSecret

	// nothing requested: nothing to do, and the same signing secret
	if acts, _ := g.r.Plan(ctx, d); len(acts) != 0 {
		t.Fatalf("%v", acts)
	}
	if err := g.db.RequestRotation(ctx, "people", "keyring", "user:alice"); err != nil {
		t.Fatal(err)
	}
	acts, err := g.r.Plan(ctx, d)
	if err != nil || kinds(acts) != "upgrade:people" || acts[0].Reason != "secret rotation requested" {
		t.Fatalf("%v %v", acts, err)
	}
	if _, err := g.r.Apply(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	got := g.fk.Enabled["people"]
	if len(got.Rotate) != 1 || got.Rotate[0] != "keyring" {
		t.Fatalf("installer was not told to rotate: %+v", got.Rotate)
	}
	if got.HMACSecret != first {
		t.Fatal("rotating the keyring changed the webhook signing secret")
	}
	if pend, _ := g.db.PendingRotations(ctx, "people"); len(pend) != 0 {
		t.Fatalf("the request was not consumed: %v", pend)
	}
	if acts, _ := g.r.Plan(ctx, d); len(acts) != 0 {
		t.Fatalf("rotation repeats: %v", acts)
	}
}

func TestWebhookSecretRotationChangesTheSigningSecret(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	d := desired(nil, nil, mf("work", "0.1.0", nil))
	if _, err := g.r.Apply(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	first := g.fk.Enabled["work"].HMACSecret
	_ = g.db.RequestRotation(ctx, "work", modsecrets.WebhookKey, "user:alice")
	if _, err := g.r.Apply(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	rec, _ := g.db.GetModule(ctx, "work")
	if rec.HMACSecret == first || rec.HMACSecret != g.fk.Enabled["work"].HMACSecret || len(rec.HMACSecret) != 64 {
		t.Fatalf("signing secret not rotated consistently")
	}
}

func TestFailedUpgradeKeepsTheRotationRequest(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	d := desired(nil, nil, mf("work", "0.1.0", nil))
	if _, err := g.r.Apply(ctx, d, Options{}); err != nil {
		t.Fatal(err)
	}
	_ = g.db.RequestRotation(ctx, "work", "CAN_WORK_ADMIN_TOKEN", "user:alice")
	g.fk.FailEnable = map[string]error{"work": context.DeadlineExceeded}
	if _, err := g.r.Apply(ctx, d, Options{}); err == nil {
		t.Fatal("expected the failure")
	}
	if pend, _ := g.db.PendingRotations(ctx, "work"); len(pend) != 1 {
		t.Fatalf("a failed rotation must be retried, pending=%v", pend)
	}
	rec, _ := g.db.GetModule(ctx, "work")
	if rec.HMACSecret != g.fk.Enabled["work"].HMACSecret {
		t.Fatal("recorded secret differs from the running one")
	}
}
