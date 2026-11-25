package corecli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestSecretsRotateRecordsARequestAndAuditsWithoutValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	d, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpsertModule(ctx, store.Module{ID: "people", Version: "1", Namespace: "mod-people", ServiceAccount: "people", State: store.StateEnabled, Manifest: config.Module{ID: "people"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()

	code, out, e := run(t, nil, "", "admin", "secrets", "rotate", "people", "keyring", "--db", path)
	if code != 0 || !strings.Contains(out, "next reconcile") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	d, _ = store.Open(ctx, path)
	defer func() { _ = d.Close() }()
	if pend, _ := d.PendingRotations(ctx, "people"); len(pend) != 1 || pend[0] != "keyring" {
		t.Fatalf("%v", pend)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM audit_log WHERE action='secret.rotate.request' AND target='people'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit %d %v", n, err)
	}
}

func TestSecretsRotateRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	d, _ := store.Open(ctx, path)
	_ = d.UpsertModule(ctx, store.Module{ID: "jobs", Version: "1", Namespace: "mod-jobs", ServiceAccount: "jobs", State: store.StateEnabled, Manifest: config.Module{ID: "jobs"}}, nil, nil)
	_ = d.Close()
	for name, args := range map[string][]string{
		"unknown module":       {"nope", "keyring"},
		"unknown key":          {"jobs", "CAN_JOBS_SOMETHING"},
		"hash key (data loss)": {"jobs", "CAN_JOBS_HASH_KEY"},
	} {
		code, _, e := run(t, nil, "", append([]string{"admin", "secrets", "rotate"}, append(args, "--db", path)...)...)
		if code == 0 {
			t.Errorf("%s: accepted", name)
		}
		if strings.Contains(e, "k1 ") {
			t.Errorf("%s: output looks like it prints a key", name)
		}
	}
}
