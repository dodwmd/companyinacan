package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
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

var moduleDirRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// recordedManifest is the module's recorded manifest, completed with the subcommand hooks of the
// manifest shipped in this can-core image when the recorded one has no hook. The record is only
// rewritten when a module's version or settings change (and never while it is disabled), so a
// module installed before its in-house hook existed would otherwise never be dumped
// (specs/043-module-backup-hooks).
func recordedManifest(modulesDir string, m store.Module) config.Module {
	man := m.Manifest
	if modulesDir == "" || !moduleDirRe.MatchString(m.ID) {
		return man
	}
	doc, _ := config.Load(filepath.Join(modulesDir, m.ID, "module.yaml"))
	if doc == nil {
		return man
	}
	// Subcommands only, and only for the same provider: a script hook needs its files and image
	// to match what was installed.
	// A wrapped module recorded with the scripts of the unpublished hooks image is dumped by
	// can-core as the shipped manifest says (specs/046-wrapped-backups).
	shipped, err := config.DecodeModule(doc)
	if err == nil && shipped.Provider.Name == man.Provider.Name &&
		(shipped.Hooks.Builtin != nil || man.Hooks.Backup == "" && shipped.Hooks.Command != "") {
		man.Hooks = shipped.Hooks
	}
	return man
}

// backupModules lists every recorded module that has a backup hook, whatever its state: a
// disabled or failed module keeps its data. Only a purge removes the record.
func backupModules(db *store.DB, modulesDir string) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		mods, err := db.ListModules(ctx)
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, m := range mods {
			if recordedManifest(modulesDir, m).Hooks.Backup != "" {
				ids = append(ids, m.ID)
			}
		}
		sort.Strings(ids)
		return ids, nil
	}
}

func (c Config) kubeHooks(api kube.API, db *store.DB) *dumps.KubeHooks {
	return &dumps.KubeHooks{API: api, ModulesDir: c.ModulesDir, PlatformURL: c.PlatformURL,
		Manifest: func(ctx context.Context, id string) (*config.Module, error) {
			m, err := db.GetModule(ctx, id)
			if err != nil {
				return nil, err
			}
			man := recordedManifest(c.ModulesDir, m)
			return &man, nil
		}}
}

// newDumper builds the dump runner; hooks can only run (as Jobs) with the Kubernetes installer.
func (c Config) newDumper(db *store.DB, log *slog.Logger) (*dumps.Dumper, error) {
	record := func(ctx context.Context, id string) (store.Module, bool) {
		m, err := db.GetModule(ctx, id)
		return m, err == nil
	}
	d := &dumps.Dumper{Store: &dumps.Store{Dir: c.DumpsDir}, DBPath: c.DBPath, Modules: backupModules(db, c.ModulesDir),
		Database: func(ctx context.Context, id string) string {
			m, ok := record(ctx, id)
			if !ok || m.Manifest.Requires.Database == "" {
				return "unknown"
			}
			return m.Manifest.Requires.Database
		},
		Disabled: func(ctx context.Context, id string) bool {
			m, ok := record(ctx, id)
			return ok && m.State == store.StateDisabled
		},
		Unrunnable: func(ctx context.Context, id string) string {
			m, ok := record(ctx, id)
			if !ok {
				return ""
			}
			man := recordedManifest(c.ModulesDir, m)
			return man.BackupHookUnrunnable()
		},
		CompanyFile: c.CompanyFile, Log: log}
	if c.Installer == "kubernetes" && c.ModuleInstaller == nil {
		api, err := kube.NewInCluster(c.KubeAPI, c.SAToken, c.SACA)
		if err != nil {
			return nil, err
		}
		d.Hooks = c.kubeHooks(api, db)
		d.Secrets = func(ctx context.Context) (*backup.SecretsExport, []string, error) {
			mods, err := db.ListModules(ctx)
			if err != nil {
				return nil, nil, err
			}
			refs := make([]dumps.ModuleRef, 0, len(mods))
			for _, m := range mods {
				refs = append(refs, dumps.ModuleRef{ID: m.ID, Enabled: m.State == store.StateEnabled})
			}
			return dumps.ExportSecrets(ctx, api, refs)
		}
	}
	return d, nil
}
