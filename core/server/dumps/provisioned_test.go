package dumps

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

// A can-db Secret without DB_HOST belongs to a database that was never provisioned (a first
// enable that failed): there is nothing to dump, and it must not block every backup. With
// DB_HOST the database exists (a failed re-enable keeps it), so the hook runs and its failure
// stays loud.
func TestNeverProvisionedDatabaseModuleHasNoData(t *testing.T) {
	h, api, m := hookFixture(t)
	m.Requires.Database = "postgres"
	apply := func(data map[string]any) {
		if err := api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{"name": "can-db", "namespace": "mod-wiki"}, "stringData": data}); err != nil {
			t.Fatal(err)
		}
	}
	apply(map[string]any{"DB_PASSWORD": "x"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.RunBackup(ctx, "wiki"); !errors.Is(err, ErrNoData) {
		t.Fatalf("never provisioned: %v", err)
	}
	apply(map[string]any{"DB_PASSWORD": "x", "DB_HOST": "postgres"})
	finishJob(api, "failed")
	if err := h.RunBackup(ctx, "wiki"); err == nil || errors.Is(err, ErrNoData) {
		t.Fatalf("provisioned database with a failing hook must fail: %v", err)
	}
}
