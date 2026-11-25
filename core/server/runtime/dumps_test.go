package runtime

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDumpConfig(t *testing.T) {
	c, err := FromEnv(env(nil))
	if err != nil || c.DumpsDir != "" {
		t.Fatalf("dumps must be off unless CAN_DUMPS_DIR is set: %q %v", c.DumpsDir, err)
	}
	c, err = FromEnv(env(map[string]string{"CAN_DUMPS_DIR": "/var/lib/can-dumps", "CAN_DB_PATH": "/var/lib/can/state.db"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DumpHour != 2 || c.DumpMinute != 30 || c.DumpStartDelay != 10*time.Minute || c.DumpRequestFile != "/var/lib/can/dump.request" {
		t.Fatalf("defaults: %+v", c)
	}
	c, err = FromEnv(env(map[string]string{"CAN_DUMPS_DIR": "/d", "CAN_DUMP_AT": "23:05", "CAN_DUMP_START_DELAY_SECONDS": "3"}))
	if err != nil || c.DumpHour != 23 || c.DumpMinute != 5 || c.DumpStartDelay != 3*time.Second {
		t.Fatalf("overrides: %+v %v", c, err)
	}
	for _, bad := range []map[string]string{{"CAN_DUMP_AT": "25:00"}, {"CAN_DUMP_AT": "noon"}, {"CAN_DUMP_START_DELAY_SECONDS": "-1"}} {
		bad["CAN_DUMPS_DIR"] = "/d"
		if _, err := FromEnv(env(bad)); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestModulesWithBackupHooks(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	reg := registry.New(db, "http://x")
	add := func(id, state, hook string) {
		m := config.Module{ID: id, Name: id, Version: "1"}
		m.Hooks.Backup = hook
		if err := reg.Register(ctx, store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: state, HMACSecret: "s", Manifest: m}); err != nil {
			t.Fatal(err)
		}
	}
	add("wiki", store.StateEnabled, "./hooks/backup.sh")
	add("forms", store.StateEnabled, "")
	add("crm", store.StateDisabled, "./hooks/backup.sh")
	got, err := backupModules(db)(ctx)
	if err != nil || !reflect.DeepEqual(got, []string{"wiki"}) {
		t.Fatalf("%v %v", got, err)
	}
}
