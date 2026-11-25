// Package backup implements epic E08: the backup plan, the host systemd timer and
// backup script, the restic runner abstraction, the recovery kit, the erasure
// tombstone ledger and the `can restore` state machine (SPEC.md section 6.5).
//
// Nothing here talks to a real bucket, restic or cluster in tests: restic runs
// behind Runner, and the restore steps run behind small interfaces.
package backup

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
)

// Settings are the host-side parameters of the backup. The zero value is not
// usable; start from DefaultSettings.
type Settings struct {
	DumpsDir      string // can-core writes dumps here (hostPath on can-core only)
	PVCRoot       string // k3s local-path storage
	K3sDir        string // k3s data directory (datastore copy, token, encryption key)
	ConfigDir     string // /etc/can/backup.d: restic.env, password, webhook
	StateDir      string // /var/lib/can: status file, lock
	ScriptPath    string // installed backup script
	Repository    string // restic repository, e.g. s3:https://host/bucket/restic
	RetentionDays int    // forget horizon; also the erasure "beyond use" horizon
	MaxMarkerAge  time.Duration
	Schedule      string // systemd OnCalendar expression
	RandomDelay   string // systemd RandomizedDelaySec
	LedgerSource  string // erasure ledger file written by can-core
	CanBin        string // can binary used for ledger upload
}

// DefaultSettings returns the documented defaults.
func DefaultSettings() Settings {
	return Settings{
		DumpsDir:      "/var/lib/can/dumps",
		PVCRoot:       "/var/lib/rancher/k3s/storage",
		K3sDir:        "/var/lib/rancher/k3s",
		ConfigDir:     "/etc/can/backup.d",
		StateDir:      "/var/lib/can",
		ScriptPath:    "/usr/local/sbin/can-backup",
		RetentionDays: 35,
		MaxMarkerAge:  24 * time.Hour,
		Schedule:      "*-*-* 03:30:00",
		RandomDelay:   "15m",
		LedgerSource:  "/var/lib/can/erasure-ledger.jsonl",
		CanBin:        "/usr/local/bin/can",
	}
}

var (
	absPathRe = regexp.MustCompile(`^/[A-Za-z0-9._/+-]*$`)
	safeValRe = regexp.MustCompile(`^[A-Za-z0-9:/._@*+ -]+$`)
)

// Validate rejects values that could break out of the generated script or unit.
func (s Settings) Validate() error {
	for name, p := range map[string]string{
		"dumps dir": s.DumpsDir, "pvc root": s.PVCRoot, "k3s dir": s.K3sDir,
		"config dir": s.ConfigDir, "state dir": s.StateDir, "script path": s.ScriptPath,
		"ledger source": s.LedgerSource, "can binary": s.CanBin,
	} {
		if !absPathRe.MatchString(p) {
			return fmt.Errorf("backup: %s %q must be an absolute path of plain characters", name, p)
		}
	}
	if s.RetentionDays < 1 {
		return fmt.Errorf("backup: retention must be at least 1 day")
	}
	if s.MaxMarkerAge < time.Hour {
		return fmt.Errorf("backup: marker max age must be at least 1h")
	}
	for name, v := range map[string]string{"schedule": s.Schedule, "random delay": s.RandomDelay, "repository": s.Repository} {
		if v != "" && !safeValRe.MatchString(v) {
			return fmt.Errorf("backup: %s %q has unsafe characters", name, v)
		}
	}
	if s.Schedule == "" || s.RandomDelay == "" {
		return fmt.Errorf("backup: schedule and random delay are required")
	}
	return nil
}

// Dump is one module backup hook whose output lands in the dumps directory.
type Dump struct {
	Module string `json:"module"`
	Hook   string `json:"hook"`
	File   string `json:"file"` // relative to the dumps directory
}

// Plan is what gets backed up, derived from company.yaml and module manifests.
type Plan struct {
	Dumps    []Dump   `json:"dumps"`
	Sources  []string `json:"sources"`  // restic paths, dumps first
	Excludes []string `json:"excludes"` // restic --exclude patterns
	Warnings []string `json:"warnings,omitempty"`
}

// sqliteExcludes are the SQLite file patterns kept out of the raw PVC copy for
// modules whose database is covered by a dump (SPEC.md section 6.5).
var sqliteExcludes = []string{"*.db", "*.sqlite", "*.sqlite3", "*-wal", "*-shm", "*-journal"}

// BuildPlan derives the plan. Disabled modules are skipped; an enabled module
// without a loaded manifest is an error, because silently skipping it would
// lose its data.
func BuildPlan(co *config.Company, mods map[string]*config.Module, s Settings) (*Plan, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	p := &Plan{Sources: []string{s.DumpsDir, s.PVCRoot}}
	ids := make([]string, 0, len(co.Modules))
	for id, e := range co.Modules {
		if e.IsEnabled() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		m, ok := mods[id]
		if !ok || m == nil {
			return nil, fmt.Errorf("backup: module %q is enabled but has no manifest", id)
		}
		hasDump := m.Hooks.Backup != ""
		if hasDump {
			p.Dumps = append(p.Dumps, Dump{Module: id, Hook: m.Hooks.Backup, File: "modules/" + id + ".dump"})
		}
		if m.Requires.Database != "sqlite" {
			continue
		}
		if !hasDump {
			p.Warnings = append(p.Warnings, fmt.Sprintf("module %q keeps SQLite on a PVC but has no backup hook: raw database files are backed up and may be inconsistent", id))
			continue
		}
		for _, pat := range sqliteExcludes {
			p.Excludes = append(p.Excludes, fmt.Sprintf("%s/pvc-*_mod-%s_*/**/%s", strings.TrimRight(s.PVCRoot, "/"), id, pat))
		}
	}
	return p, nil
}
