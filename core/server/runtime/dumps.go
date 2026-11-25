package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// dumpEnv reads the backup-dump settings (epic E08b). Dumps are off unless CAN_DUMPS_DIR is set.
func (c *Config) dumpEnv(getenv func(string) string) error {
	c.DumpsDir = getenv("CAN_DUMPS_DIR")
	if c.DumpsDir == "" {
		return nil
	}
	c.DumpHour, c.DumpMinute, c.DumpStartDelay = 2, 30, 10*time.Minute
	if v := getenv("CAN_DUMP_AT"); v != "" {
		t, err := time.Parse("15:04", v)
		if err != nil {
			return fmt.Errorf("CAN_DUMP_AT: %q is not HH:MM (UTC)", v)
		}
		c.DumpHour, c.DumpMinute = t.Hour(), t.Minute()
	}
	if v := getenv("CAN_DUMP_START_DELAY_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("CAN_DUMP_START_DELAY_SECONDS: %q is not a non-negative integer", v)
		}
		c.DumpStartDelay = time.Duration(n) * time.Second
	}
	c.DumpRequestFile = getenv("CAN_DUMP_REQUEST_FILE")
	if c.DumpRequestFile == "" {
		c.DumpRequestFile = DefaultDumpRequestFile(c.DBPath)
	}
	return nil
}

// DefaultDumpRequestFile is the file `can-core dump-request` creates, next to the state database.
func DefaultDumpRequestFile(dbPath string) string { return dirOf(dbPath) + "/dump.request" }

// backupModules lists every recorded module whose manifest ships a backup hook, whatever its
// state: a disabled or failed module keeps its data. Only a purge removes the record.
func backupModules(db *store.DB) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		mods, err := db.ListModules(ctx)
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, m := range mods {
			if m.Manifest.Hooks.Backup != "" {
				ids = append(ids, m.ID)
			}
		}
		sort.Strings(ids)
		return ids, nil
	}
}

// newDumper builds the dump runner; hooks can only run (as Jobs) with the Kubernetes installer.
func (c Config) newDumper(db *store.DB, log *slog.Logger) (*dumps.Dumper, error) {
	d := &dumps.Dumper{Store: &dumps.Store{Dir: c.DumpsDir}, DBPath: c.DBPath, Modules: backupModules(db),
		Database: func(ctx context.Context, id string) string {
			m, err := db.GetModule(ctx, id)
			if err != nil || m.Manifest.Requires.Database == "" {
				return "unknown"
			}
			return m.Manifest.Requires.Database
		},
		CompanyFile: c.CompanyFile, Log: log}
	if c.Installer == "kubernetes" && c.ModuleInstaller == nil {
		api, err := kube.NewInCluster(c.KubeAPI, c.SAToken, c.SACA)
		if err != nil {
			return nil, err
		}
		d.Hooks = &dumps.KubeHooks{API: api, ModulesDir: c.ModulesDir, PlatformURL: c.PlatformURL,
			Manifest: func(ctx context.Context, id string) (*config.Module, error) {
				m, err := db.GetModule(ctx, id)
				if err != nil {
					return nil, err
				}
				return &m.Manifest, nil
			}}
	}
	return d, nil
}
