package reconcile

import (
	"slices"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// An installation that has equity without equity.board is asked before it gets it
// (specs/068-equity-board-role).
func TestTheEquityBoardPermissionIsHeldForApproval(t *testing.T) {
	doc, ps := config.Load("../../../modules/equity/module.yaml")
	if doc == nil {
		t.Fatal(ps)
	}
	next, err := config.DecodeModule(doc)
	if err != nil {
		t.Fatal(err)
	}
	old := *next
	without := func(in []string) []string {
		return slices.DeleteFunc(slices.Clone(in), func(p string) bool { return p == "equity.board" })
	}
	old.Permissions = without(next.Permissions)
	old.Roles = map[string][]string{}
	for r, p := range next.Roles {
		old.Roles[r] = without(p)
	}
	want := []string{"new permission equity.board", "role board gains equity.board"}
	if got := Widening(&old, next); !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
}
