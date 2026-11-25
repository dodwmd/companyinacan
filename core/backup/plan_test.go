package backup

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func mod(id, db, backupHook string) *config.Module {
	m := &config.Module{ID: id}
	m.Requires.Database = db
	m.Hooks.Backup = backupHook
	return m
}

func off() *bool { f := false; return &f }

func TestBuildPlanDumpsAndScope(t *testing.T) {
	co := &config.Company{Modules: map[string]config.ModuleEntry{
		"forms": {}, "wiki": {}, "crm": {}, "gone": {Enabled: off()},
	}}
	mods := map[string]*config.Module{
		"forms": mod("forms", "sqlite", "./hooks/backup.sh"),
		"wiki":  mod("wiki", "postgres", "./hooks/backup.sh"),
		"crm":   mod("crm", "none", ""),
		"gone":  mod("gone", "postgres", "./hooks/backup.sh"),
	}
	p, err := BuildPlan(co, mods, DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Dumps) != 2 || p.Dumps[0].Module != "forms" || p.Dumps[1].Module != "wiki" {
		t.Fatalf("dumps = %+v", p.Dumps)
	}
	if p.Dumps[0].File != "modules/forms/" {
		t.Errorf("dump file = %q", p.Dumps[0].File)
	}
	// SQLite-on-PVC module: its database files are excluded from the raw PVC copy.
	var sawDB, sawWAL bool
	for _, e := range p.Excludes {
		if strings.Contains(e, "_mod-forms_") && strings.HasSuffix(e, "*.db") {
			sawDB = true
		}
		if strings.Contains(e, "_mod-forms_") && strings.HasSuffix(e, "*-wal") {
			sawWAL = true
		}
		if strings.Contains(e, "_mod-wiki_") {
			t.Errorf("postgres module must not get sqlite excludes: %s", e)
		}
	}
	if !sawDB || !sawWAL {
		t.Errorf("sqlite excludes missing: %v", p.Excludes)
	}
	// can-core's live database is excluded: its dump covers it.
	if !strings.Contains(strings.Join(p.Excludes, "\n"), "/var/lib/rancher/k3s/storage/pvc-*_can-system_can-core-data/state.db-wal") {
		t.Errorf("can-core state excludes missing: %v", p.Excludes)
	}
	// Sources: dumps dir first (restored first), then PVC root.
	if p.Sources[0] != "/var/lib/can/dumps" || p.Sources[1] != "/var/lib/rancher/k3s/storage" {
		t.Errorf("sources = %v", p.Sources)
	}
}

func TestBuildPlanWarnsSQLiteWithoutHook(t *testing.T) {
	co := &config.Company{Modules: map[string]config.ModuleEntry{"forms": {}}}
	p, err := BuildPlan(co, map[string]*config.Module{"forms": mod("forms", "sqlite", "")}, DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "forms") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
	// And, since no dump covers it, the files must NOT be excluded.
	for _, e := range p.Excludes {
		if strings.Contains(e, "_mod-forms_") {
			t.Errorf("must not exclude data that no dump covers: %s", e)
		}
	}
}

func TestBuildPlanUnknownModule(t *testing.T) {
	co := &config.Company{Modules: map[string]config.ModuleEntry{"x": {}}}
	if _, err := BuildPlan(co, nil, DefaultSettings()); err == nil {
		t.Fatal("expected error for module without manifest")
	}
}

func TestSettingsValidate(t *testing.T) {
	s := DefaultSettings()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*Settings){
		func(s *Settings) { s.DumpsDir = "relative" },
		func(s *Settings) { s.DumpsDir = "/var/lib/can/dumps; rm -rf /" },
		func(s *Settings) { s.RetentionDays = 0 },
		func(s *Settings) { s.Schedule = "daily\nExecStart=/bin/evil" },
		func(s *Settings) { s.Repository = "s3:x'y" },
	}
	for i, f := range bad {
		s := DefaultSettings()
		f(&s)
		if s.Validate() == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}
