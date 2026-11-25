package backup

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden (run with -update to accept)", name)
	}
}

func testPlan() (*Plan, Settings) {
	s := DefaultSettings()
	s.Repository = "s3:https://s3.example.invalid/company-backups/restic"
	p := &Plan{
		Sources:  []string{s.DumpsDir, s.PVCRoot, s.HostStateDir},
		Excludes: []string{"/var/lib/rancher/k3s/storage/pvc-*_mod-forms_*/**/*.db"},
		Dumps:    []Dump{{Module: "forms", File: "modules/forms.dump"}},
	}
	return p, s
}

func TestScriptGolden(t *testing.T) {
	p, s := testPlan()
	golden(t, "backup.sh.golden", GenerateScript(p, s))
}

func TestUnitGolden(t *testing.T) {
	_, s := testPlan()
	golden(t, "can-backup.service.golden", GenerateService(s))
	golden(t, "can-backup.timer.golden", GenerateTimer(s))
	golden(t, "can-backup-failure.service.golden", GenerateFailureService())
}

func TestScriptDeterministic(t *testing.T) {
	p, s := testPlan()
	first, second := GenerateScript(p, s), GenerateScript(p, s)
	if first != second {
		t.Fatal("not deterministic")
	}
}

func TestScriptProperties(t *testing.T) {
	p, s := testPlan()
	sc := GenerateScript(p, s)
	mustContain := []string{
		"set -Eeuo pipefail",
		"flock -n 9",
		"restic unlock",
		"forget --group-by '' --keep-within 35d",
		"--prune",
		"logger -t can-backup",
		"RESTIC_PASSWORD_FILE",
		"/etc/can/backup.d/restic.env",
		"fail 10",
		"fail 11",
		"fail 12",
		"fail 20",
		"sha256sum",
	}
	for _, m := range mustContain {
		if !strings.Contains(sc, m) {
			t.Errorf("script lacks %q", m)
		}
	}
	for _, bad := range []string{"RESTIC_PASSWORD=", "--password ", "AWS_SECRET_ACCESS_KEY="} {
		if strings.Contains(sc, bad) {
			t.Errorf("script leaks credential handling via %q", bad)
		}
	}
	if strings.Index(sc, "fail 11") > strings.Index(sc, "restic backup --tag") {
		t.Error("marker check must precede restic backup")
	}
	svc := GenerateService(s)
	for _, bad := range []string{"Environment=", "RESTIC_PASSWORD", "AWS_"} {
		if strings.Contains(svc, bad) {
			t.Errorf("unit carries credentials via %q", bad)
		}
	}
	for _, want := range []string{"OnFailure=can-backup-failure.service", "ProtectSystem=strict", "ReadWritePaths="} {
		if !strings.Contains(svc, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
	if !strings.Contains(svc, "Type=oneshot") || !strings.Contains(GenerateTimer(s), "Persistent=true") {
		t.Error("unit/timer basics missing")
	}
}

func TestScriptQuotesUntrustedPaths(t *testing.T) {
	p, s := testPlan()
	p.Excludes = []string{"/x/it's a trap/*.db"}
	sc := GenerateScript(p, s)
	if !strings.Contains(sc, `'/x/it'\''s a trap/*.db'`) {
		t.Errorf("exclude not shell-quoted:\n%s", sc)
	}
}

func writeTmp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScriptBashSyntax(t *testing.T) {
	p, s := testPlan()
	f := writeTmp(t, "backup.sh", GenerateScript(p, s))
	if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

func TestScriptShellcheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	p, s := testPlan()
	f := writeTmp(t, "backup.sh", GenerateScript(p, s))
	if out, err := exec.Command(sc, "-s", "bash", f).CombinedOutput(); err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, out)
	}
}

func TestSystemdAnalyzeVerify(t *testing.T) {
	sa, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not installed")
	}
	p, s := testPlan()
	dir := t.TempDir()
	s.ScriptPath = filepath.Join(dir, "can-backup")
	if err := os.WriteFile(s.ScriptPath, []byte(GenerateScript(p, s)), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := filepath.Join(dir, "can-backup.service")
	tmr := filepath.Join(dir, "can-backup.timer")
	_ = os.WriteFile(svc, []byte(GenerateService(s)), 0o644)
	_ = os.WriteFile(tmr, []byte(GenerateTimer(s)), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "can-backup-failure.service"), []byte(GenerateFailureService()), 0o644)
	out, err := exec.Command(sa, "verify", svc, tmr).CombinedOutput()
	if err != nil {
		var ours []string
		for _, l := range strings.Split(string(out), "\n") {
			if strings.Contains(l, "can-backup") && !strings.Contains(l, "Failed to") {
				ours = append(ours, l)
			}
		}
		if len(ours) > 0 {
			t.Fatalf("systemd-analyze verify: %s", strings.Join(ours, "\n"))
		}
		t.Skipf("systemd-analyze verify unusable here: %v: %s", err, out)
	}
}
