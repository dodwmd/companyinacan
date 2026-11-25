package reconcile

import (
	"context"
	"strings"
	"testing"
)

// A manifest the registry would refuse must be refused before the installer runs, or a Helm
// release would be left running with no record of it.
func TestEnableRefusesReservedNamesBeforeInstalling(t *testing.T) {
	g := newRig(t)
	m := mf("evil", "1.0.0", nil)
	m.Permissions = []string{"rbac.manage"}
	m.Roles = map[string][]string{"auditor": {"rbac.manage"}}
	res, err := g.r.Apply(context.Background(), desired(nil, nil, m), Options{})
	if err == nil && len(res.Failed) == 0 {
		t.Fatal("hostile manifest was applied")
	}
	for _, c := range g.fk.Calls {
		if strings.HasPrefix(c, "enable:") {
			t.Fatalf("installer called for a refused manifest: %v", g.fk.Calls)
		}
	}
	if _, gerr := g.db.GetModule(context.Background(), "evil"); gerr == nil {
		t.Fatal("refused module recorded")
	}
}

func TestEnableRefusesAPermissionOwnedByAnotherModuleBeforeInstalling(t *testing.T) {
	g := newRig(t)
	a := mf("work", "1.0.0", nil)
	a.Permissions = []string{"work.admin"}
	if _, err := g.r.Apply(context.Background(), desired(nil, nil, a), Options{}); err != nil {
		t.Fatal(err)
	}
	b := mf("evil", "1.0.0", nil)
	b.Permissions = []string{"work.admin"}
	g.fk.Calls = nil
	_, _ = g.r.Apply(context.Background(), desired(nil, nil, a, b), Options{})
	for _, c := range g.fk.Calls {
		if c == "enable:evil" {
			t.Fatalf("installer ran for a module whose permission is taken: %v", g.fk.Calls)
		}
	}
}
