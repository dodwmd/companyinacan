package reconcile

import (
	"context"
	"slices"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// An installation records a module's manifest again only when its version changes. The forms module
// added a permission, a role and an event (specs/054-lead-erasure), so an installed 0.1.0 must be
// planned for an upgrade by the manifest this repository ships.
func TestInstalledFormsIsUpgradedToTheShippedManifest(t *testing.T) {
	doc, ps := config.Load("../../../modules/forms/module.yaml")
	if doc == nil {
		t.Fatal(ps)
	}
	shipped, err := config.DecodeModule(doc)
	if err != nil {
		t.Fatal(err)
	}
	old := *shipped
	old.Version = "0.1.0"
	old.Permissions = []string{"leads.read", "leads.write"}
	old.Roles = map[string][]string{"sales": {"leads.read", "leads.write"}, "viewer": {"leads.read"}}
	old.Publishes = shipped.Publishes[:1]

	ctx := context.Background()
	g := newRig(t)
	if _, err := g.r.Apply(ctx, desired(nil, nil, &old), Options{}); err != nil {
		t.Fatal(err)
	}
	plan, err := g.r.Plan(ctx, desired(nil, nil, shipped))
	if err != nil || kinds(plan) != "upgrade:forms" {
		t.Fatalf("plan %q %v: is the version in modules/forms/module.yaml bumped?", kinds(plan), err)
	}
	if _, err := g.r.Apply(ctx, desired(nil, nil, shipped), Options{}); err != nil {
		t.Fatal(err)
	}
	rec, err := g.db.GetModule(ctx, "forms")
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, p := range rec.Manifest.Publishes {
		events = append(events, p.Event)
	}
	if !slices.Contains(rec.Manifest.Permissions, "leads.erase") || !slices.Contains(rec.Manifest.Roles["admin"], "leads.erase") || !slices.Contains(events, "lead.erased.v1") {
		t.Errorf("recorded manifest: %v %v %v", rec.Manifest.Permissions, rec.Manifest.Roles, events)
	}
}
