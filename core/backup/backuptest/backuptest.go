// Package backuptest builds the files of a restorable dump set for tests of backup and cli.
package backuptest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// Module is a module record of the generated state database.
type Module struct {
	ID, State, Database, RestoreHook string
}

// StateDB returns the bytes of a can-core state database with one admin and the given modules.
func StateDB(t *testing.T, mods ...Module) []byte {
	t.Helper()
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAdmin(ctx, "admin", "hash", "secret"); err != nil {
		t.Fatal(err)
	}
	for _, m := range mods {
		man := config.Module{ID: m.ID, Version: "0.1.0"}
		man.Requires.Database = m.Database
		man.Hooks.Restore = m.RestoreHook
		if err := db.UpsertModule(ctx, store.Module{ID: m.ID, Version: "0.1.0", Namespace: "mod-" + m.ID, ServiceAccount: m.ID, State: m.State, Manifest: man}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// A single self-contained file, as VACUUM INTO writes it.
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, p+".copy"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p + ".copy") //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	return b
}
