package rbac

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// The equity board role (specs/068-equity-board-role): equity.board comes only with equity:board, and
// because the grant approval names that role, only an owner or somebody who holds it can assign it,
// however many of its other permissions an administrator holds.
func TestEquityBoardRoleIsOwnerGranted(t *testing.T) {
	doc, ps := config.Load("../../../modules/equity/module.yaml")
	if doc == nil {
		t.Fatal(ps)
	}
	m, err := config.DecodeModule(doc)
	if err != nil {
		t.Fatal(err)
	}
	for role, perms := range m.Roles {
		if got := slices.Contains(perms, "equity.board"); got != (role == "board") {
			t.Fatalf("manifest role %s holds equity.board = %v", role, got)
		}
	}
	e := newEnv(t)
	ctx := context.Background()
	if err := e.db.UpsertModule(ctx, store.Module{ID: "equity", Version: m.Version, Namespace: "mod-equity", ServiceAccount: "equity",
		State: store.StateEnabled, HMACSecret: "x", Manifest: *m}, nil, nil); err != nil {
		t.Fatal(err)
	}
	grant := func(actor, user, role string) error { return e.s.Grant(ctx, actor, "user:"+user, role, "") }
	for _, a := range [][2]string{{"boss", Owner}, {"delegate", "equity:admin"}} {
		if err := grant(CLIActor, a[0], a[1]); err != nil {
			t.Fatal(err)
		}
	}
	// the delegate holds every permission of equity:board but equity.board
	for _, p := range m.Roles["board"] {
		if got, want := e.check("user:delegate", p, ""), p != "equity.board"; got != want {
			t.Fatalf("delegate holds %s = %v", p, got)
		}
	}
	if err := grant("delegate", "someone", "equity:board"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an administrator without it assigned the board role: %v", err)
	}
	if err := grant("boss", "chair", "equity:board"); err != nil {
		t.Fatalf("an owner assigning the board role: %v", err)
	}
	if !e.check("user:chair", "equity.board", "") || e.check("user:chair", "equity.write", "") {
		t.Fatal("the board role grants the board's permissions and no writing")
	}
	// the owner holds every permission, this one included
	if !e.check("user:boss", "equity.board", "") {
		t.Fatal("owner")
	}
}
