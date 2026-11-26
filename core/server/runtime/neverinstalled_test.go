package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// E06d: only a failed module without a release record counts as never installed.
func TestNeverInstalled(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for id, state := range map[string]string{"people": store.StateFailed, "work": store.StateFailed, "forms": store.StateEnabled, "jobs": store.StateDisabled} {
		if err := db.UpsertModule(ctx, store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: state}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	api := kube.NewFake()
	// work ran once: can-core recorded its release
	if err := api.Apply(ctx, kube.Object{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": dumps.ReleaseSecret, "namespace": "mod-work"}}); err != nil {
		t.Fatal(err)
	}
	never := neverInstalled(api, db)
	for id, want := range map[string]bool{"people": true, "work": false, "forms": false, "jobs": false, "unknown": false} {
		if got := never(ctx, id); got != want {
			t.Errorf("%s: %v, want %v", id, got, want)
		}
	}
}
