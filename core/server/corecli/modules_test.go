package corecli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestModulesProblemsListsFailedInstallsAndUpgrades(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []store.Module{
		{ID: "fine", State: store.StateEnabled},
		{ID: "broken", State: store.StateFailed, LastError: "rollout timed out"},
		{ID: "stale", State: store.StateEnabled, LastError: "upgrade failed: password=hunter2"},
		{ID: "off", State: store.StateDisabled},
	} {
		m.Version, m.Namespace, m.ServiceAccount = "1", "mod-"+m.ID, m.ID
		if err := db.UpsertModule(ctx, m, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	var out, errs bytes.Buffer
	if code := Execute([]string{"modules", "problems", "--db", path}, strings.NewReader(""), &out, &errs, func(string) string { return "" }); code != exitOK {
		t.Fatal(code, errs.String())
	}
	got := out.String()
	for _, want := range []string{`"id":"broken","state":"failed"`, `"id":"stale","state":"enabled"`, "rollout timed out"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "fine") || strings.Contains(got, `"off"`) {
		t.Errorf("too much: %s", got)
	}
}
