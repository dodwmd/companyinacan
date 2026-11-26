package pat

import (
	"context"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// finance and work treat their *.admin permission as every permission of the module, so an issuer
// holding only the admin permission holds the rest: issue and introspection expand it the same
// way. Modules whose admin does not imply the rest (people, jobs) are not expanded.
func TestAdminImpliesTheModulePermissionsAtIssueAndIntrospect(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	m := config.Module{ID: "work", Version: "1.0.0", Permissions: []string{"work.read", "work.write", "work.admin"}}
	if err := e.db.UpsertModule(ctx, store.Module{ID: "work", Version: "1.0.0", Namespace: "mod-work", ServiceAccount: "work", State: store.StateEnabled, Manifest: m}, nil, nil); err != nil {
		t.Fatal(err)
	}
	held := map[string]bool{"work.admin": true}
	e.s.Holds = func(_ context.Context, _, p string) (bool, error) { return held[p], nil }
	iss := issue(t, e, "work", "work.read", "work.write") // not refused: admin holds both
	r, ok := e.s.Introspect(ctx, "work", iss.Token)
	if !ok || len(r.Permissions) != 2 {
		t.Fatalf("expanded admin lost at introspect: %+v %v", r, ok)
	}
	held["work.admin"] = false
	if _, ok := e.s.Introspect(ctx, "work", iss.Token); ok {
		t.Fatal("token still active after the admin permission was removed")
	}
	// people: people.write does not imply people.read for a token grant
	e.s.Holds = func(_ context.Context, _, p string) (bool, error) { return p == "people.write", nil }
	if _, err := e.s.Issue(ctx, "user:alice", IssueRequest{Name: "x", Module: "people", Permissions: []string{"people.read"}, TTL: time.Hour}); err == nil {
		t.Fatal("people.write was taken to imply people.read")
	}
}

// The ledger permissions are implied exactly as the finance module itself implies them, so what can-core lets a
// person grant a token agrees with what the module lets that person do.
func TestLedgerPermissionsImpliedAsTheFinanceModuleDoes(t *testing.T) {
	all := []string{"finance.read", "finance.write", "finance.approve", "finance.admin",
		"finance.ledger.read", "finance.ledger.write", "finance.ledger.approve", "finance.ledger.admin"}
	for _, held := range all {
		module := &finance.Principal{Permissions: []string{held}}
		for _, want := range all[4:] {
			got, err := HeldBy(context.Background(), func(_ context.Context, _, p string) (bool, error) { return p == held, nil }, "user:a", "finance", want)
			if err != nil {
				t.Fatal(err)
			}
			if got != module.Can(want) {
				t.Errorf("holding %s: can-core says %s=%v, the module says %v", held, want, got, module.Can(want))
			}
		}
	}
}
