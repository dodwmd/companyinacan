package contract

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type alertRule struct {
	Alert string            `yaml:"alert"`
	Expr  string            `yaml:"expr"`
	For   string            `yaml:"for"`
	Label map[string]string `yaml:"labels"`
}

func loadAlerts(t *testing.T) map[string]alertRule {
	t.Helper()
	b, err := os.ReadFile("../../deploy/monitoring/alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Groups []struct {
			Name  string      `yaml:"name"`
			Rules []alertRule `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("alerts.yml does not parse: %v", err)
	}
	out := map[string]alertRule{}
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if r.Alert == "" || strings.TrimSpace(r.Expr) == "" {
				t.Errorf("group %s has a rule without alert or expr: %+v", g.Name, r)
			}
			if _, dup := out[r.Alert]; dup {
				t.Errorf("duplicate alert %s", r.Alert)
			}
			out[r.Alert] = r
		}
	}
	return out
}

// promtool is not available offline; these checks pin the properties the QA review asked for.
func TestAlertRulesExistAndCarryTheirGuards(t *testing.T) {
	rules := loadAlerts(t)
	need := map[string][]string{
		"CanServiceDown":               {"up{job=~"},
		"CanRamOverDeclared":           {"can_ram_rss_bytes", "can_ram_declared_bytes", "1.2"},
		"CanReconcileFailing":          {"can_reconcile_consecutive_failures"},
		"CanAuditChainVerifyFailing":   {"can_audit_verify_ok == 0", "can_audit_verify_last_run_timestamp_seconds > 0"},
		"CanAuditVerifyStale":          {"can_audit_verify_last_run_timestamp_seconds"},
		"CanWebhookDeadLettersGrowing": {"can_webhook_dead_letters"},
		"CanDiskOver85Percent":         {"node_filesystem_avail_bytes", "0.85"},
		"CanBackupStale":               {"can_backup_last_success_timestamp_seconds", "36 * 3600"},
		"CanBackupMissing":             {"absent(can_backup_last_success_timestamp_seconds)"},
		"CanBackupLastRunFailed":       {"can_backup_last_run_ok == 0"},
		"CanHighServerErrorRatio":      {"class=\"5xx\"", "> 0.2"},
	}
	for name, parts := range need {
		r, ok := rules[name]
		if !ok {
			t.Errorf("alert %s is missing", name)
			continue
		}
		for _, p := range parts {
			if !strings.Contains(r.Expr, p) {
				t.Errorf("%s: expression lacks %q:\n%s", name, p, r.Expr)
			}
		}
	}
	if _, old := rules["CanNotReady"]; old {
		t.Error("CanNotReady was a misleading name for a 5xx ratio; use CanHighServerErrorRatio")
	}
	// A failing backup job must not silence the stale alert: the series going absent has its own rule,
	// and it waits long enough not to fire on a fresh install before the first backup.
	if r := rules["CanBackupMissing"]; r.For != "1h" {
		t.Errorf("CanBackupMissing for = %q, want 1h", r.For)
	}
	// One 5xx at low traffic must not page: the rule needs a minimum request rate (30 requests in 5 m).
	if e := strings.Join(strings.Fields(rules["CanHighServerErrorRatio"].Expr), " "); !strings.Contains(e, "> 0.1") || !strings.Contains(e, " and ") {
		t.Errorf("5xx ratio rule has no minimum request rate guard:\n%s", e)
	}
}

// fakeCan puts a `can` in PATH that prints out (or fails when out is empty).
func runBackupScript(t *testing.T, out, canOutput string, fail bool) string {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n"
	if fail {
		script += "exit 1\n"
	} else {
		script += "printf '%s' '" + canOutput + "'\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "can"), []byte(script), 0o755); err != nil { //nolint:gosec // test stub
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "../../deploy/monitoring/backup-textfile.sh", out) //nolint:gosec // test
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script: %v\n%s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBackupTextfileKeepsTheLastGoodTimestampWhenARunFails(t *testing.T) {
	prom := filepath.Join(t.TempDir(), "can_backup.prom")
	ok := runBackupScript(t, prom, `{"enabled":true,"latest_at":"2026-11-25T01:02:03Z"}`, false)
	if !strings.Contains(ok, "can_backup_last_success_timestamp_seconds 17") || !strings.Contains(ok, "can_backup_last_run_ok 1") {
		t.Fatalf("good run:\n%s", ok)
	}
	good := strings.Fields(strings.SplitN(strings.SplitAfter(ok, "can_backup_last_success_timestamp_seconds ")[1], "\n", 2)[0])[0]
	for _, tc := range []struct {
		name string
		out  string
		fail bool
	}{{"status command fails", "", true}, {"no snapshot (zero time)", `{"enabled":true,"latest_at":"0001-01-01T00:00:00Z"}`, false}, {"garbage", "not json", false}} {
		got := runBackupScript(t, prom, tc.out, tc.fail)
		if !strings.Contains(got, "can_backup_last_success_timestamp_seconds "+good) {
			t.Errorf("%s: the last good timestamp was lost (the stale alert would go silent):\n%s", tc.name, got)
		}
		if !strings.Contains(got, "can_backup_last_run_ok 0") {
			t.Errorf("%s: failure not reported:\n%s", tc.name, got)
		}
	}
}

func TestBackupTextfileWithoutAnyHistoryWritesNoTimestamp(t *testing.T) {
	prom := filepath.Join(t.TempDir(), "can_backup.prom")
	got := runBackupScript(t, prom, "", true)
	if strings.Contains(got, "can_backup_last_success_timestamp_seconds ") {
		t.Fatalf("a timestamp was invented:\n%s", got)
	}
	if !strings.Contains(got, "can_backup_last_run_ok 0") {
		t.Fatalf("no failure gauge:\n%s", got)
	}
}
