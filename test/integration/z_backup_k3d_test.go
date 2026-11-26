//go:build integration

package integration

// Epic E08b (specs/037-backup-dumps): the backup path on a real k3d cluster.
//
// The k3d node stands in for the server: can-core writes its dumps through the can-dumps volume
// to /var/lib/can/dumps on the node. The node image has no bash, restic or sqlite3, so the host
// half runs on the CI runner: the node's directories are copied to the same paths on the runner
// and the installed backup script runs there as root, with a filesystem restic repository in
// place of the S3 bucket. Because that writes under /var/lib on the machine running the test, it
// only runs on a throwaway GitHub Actions runner.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
)

func nodeName() string { return kubeContext() + "-server-0" }

func onNode(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", nodeName(), "sh", "-c", script).CombinedOutput() //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("on node %q: %v\n%s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

// prepareDumpsDir does on the node what host bootstrap does on a server.
func prepareDumpsDir(t *testing.T) {
	t.Helper()
	onNode(t, "mkdir -p /var/lib/can/dumps && chown 65532:65532 /var/lib/can/dumps && chmod 0700 /var/lib/can/dumps")
}

// requestDump asks the running can-core for a dump run and waits for its completion marker.
func requestDump(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	out, err := kubectl("-n", "can-system", "exec", "deploy/can-core", "--", "/can-core", "dump-request", "--wait", "5m")
	if err != nil {
		logs, _ := kubectl("-n", "can-system", "logs", "deploy/can-core", "--tail=60")
		jobs, _ := kubectl("get", "jobs,pods", "-A", "-l", "can.dev/hook=backup", "-o", "wide")
		t.Fatalf("dump-request: %v\n%s\ncan-core log:\n%s\nhook jobs:\n%s", err, out, logs, jobs)
	}
	t.Log(out)
	return time.Since(start)
}

// A module's backup hook runs as a restricted Job in its namespace and uploads through the dump
// API with its projected token; only then does the completion marker appear.
func TestBackupHookJobOnK3d(t *testing.T) {
	if os.Getenv("CAN_IT_BACKUP_HOOKS") != "1" {
		t.Skip("CAN_IT_BACKUP_HOOKS=1 not set (needs the stub-module can-core image and docker)")
	}
	prepareDumpsDir(t)
	canApply(t, company("backup", true))
	eventually(t, 180*time.Second, "the stub to run the new configuration", func() (bool, string) {
		out, err := kubectl("-n", "mod-stub", "rollout", "status", "deploy/stub", "--timeout=5s")
		l := stubLogs() // the logs prove can-core has reconciled, not merely that an old Deployment exists
		return err == nil && strings.Contains(l, "GREETING=backup") && strings.Contains(l, `"id":"stub"`), out + "\n" + l
	})
	took := requestDump(t)
	t.Logf("dump run with one module hook Job: %s", took.Round(100*time.Millisecond))

	got := onNode(t, "tar -xzOf /var/lib/can/dumps/modules/stub/probe.tar.gz probe")
	if got != "stub-data-from-the-hook" {
		t.Fatalf("the module's dump holds %q", got)
	}
	marker := onNode(t, "cat /var/lib/can/dumps/COMPLETE")
	if !strings.Contains(marker, "run_id=") || !strings.Contains(marker, "finished_at=") {
		t.Fatalf("marker: %q", marker)
	}
	onNode(t, "test -s /var/lib/can/dumps/can-core/state.db")
	if got := onNode(t, "cat /var/lib/can/dumps/DUMPED"); !strings.Contains(got, "stub none") {
		t.Errorf("DUMPED: %q", got)
	}
	if got := onNode(t, "cat /var/lib/can/dumps/LAST-RUN"); !strings.Contains(got, `"ok":true`) {
		t.Errorf("LAST-RUN: %q", got)
	}
	// the Job is cleaned up
	if out := mustKubectl(t, "-n", "mod-stub", "get", "jobs", "-o", "name"); out != "" {
		t.Errorf("hook Jobs left behind: %s", out)
	}
}

func sudo(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("sudo", append([]string{"-n"}, args...)...).CombinedOutput() //nolint:gosec // test helper, throwaway runner
	return string(out), err
}

func mustSudo(t *testing.T, args ...string) string {
	t.Helper()
	out, err := sudo(t, args...)
	if err != nil {
		t.Fatalf("sudo %v: %v\n%s", args, err, out)
	}
	return out
}

func sudoWrite(t *testing.T, path, mode string, body []byte) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, body, 0o600); err != nil {
		t.Fatal(err)
	}
	mustSudo(t, "install", "-D", "-o", "root", "-g", "root", "-m", mode, f, path)
}

// hostBackup runs the host half: installed is what `can up` copied to the (fake) host.
func hostBackup(t *testing.T, installed map[string][]byte) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("CAN_IT_BACKUP=1 writes under /var/lib and /etc as root: only on a throwaway CI runner")
	}
	s := backup.DefaultSettings()
	if _, err := os.Stat(s.StateDir); err == nil {
		t.Fatalf("%s already exists on this machine: refusing to overwrite it", s.StateDir)
	}
	var report strings.Builder
	report.WriteString("\n### backup on k3d (measured)\n\n| step | value |\n|---|---|\n")

	// the pinned restic, installed by the production script (download and digest check included)
	mustSudo(t, "sh", "-c", backup.HostToolsScript("/usr/local/bin/restic"))
	t.Log(mustSudo(t, "/usr/local/bin/restic", "version"))

	fmt.Fprintf(&report, "| dump run (request to completion marker, polled by `dump-request`) | %.1f s |\n", requestDump(t).Seconds())
	if pid := onNode(t, "pidof can-core | awk '{print $1}'"); pid != "" {
		fmt.Fprintf(&report, "| can-core memory after the dump | %s |\n", strings.Join(strings.Fields(onNode(t, "grep -E 'VmRSS|VmHWM' /proc/"+pid+"/status")), " "))
	}

	// the node's directories, at the same paths on the runner, ownership preserved
	copyCmd := fmt.Sprintf("docker exec %s tar -C / -cf - var/lib/can/dumps var/lib/rancher/k3s/storage var/lib/rancher/k3s/server/db var/lib/rancher/k3s/server/token var/lib/rancher/k3s/server/cred | tar -C / -xpf -", nodeName())
	mustSudo(t, "bash", "-o", "pipefail", "-c", "install -d -m 0700 /var/lib/can && "+copyCmd)

	// exactly the files `can up` installed; only the repository line points at a local directory
	script, ok := installed[s.ScriptPath]
	if !ok {
		t.Fatal("no backup script was installed")
	}
	const repo = "/var/lib/can-e2e-repo"
	lines := strings.Split(string(script), "\n")
	replaced := 0
	for i, l := range lines {
		if strings.HasPrefix(l, "export RESTIC_REPOSITORY=") {
			lines[i], replaced = "export RESTIC_REPOSITORY='"+repo+"'", replaced+1
		}
	}
	if replaced != 1 {
		t.Fatalf("expected one repository line in the script, found %d", replaced)
	}
	sudoWrite(t, s.ScriptPath, "0700", []byte(strings.Join(lines, "\n")))
	pw := s.ConfigDir + "/" + backup.PasswordFileName
	env := s.ConfigDir + "/" + backup.EnvFileName
	sudoWrite(t, pw, "0600", installed[pw])
	sudoWrite(t, env, "0600", installed[env])
	restic := func(args ...string) string {
		t.Helper()
		return mustSudo(t, append([]string{"/usr/local/bin/restic", "-r", repo, "--password-file", pw}, args...)...)
	}
	restic("init")

	run := []string{s.ScriptPath}
	if _, err := os.Stat("/usr/bin/time"); err == nil {
		run = []string{"/usr/bin/time", "-f", "MAXRSS_KB=%M", s.ScriptPath}
	}
	start := time.Now()
	out, err := sudo(t, run...)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("backup script: %v\n%s", err, out)
	}
	t.Log(out)
	fmt.Fprintf(&report, "| backup script (restic backup, forget, prune; local repository) | %.1f s |\n", took.Seconds())
	for _, l := range strings.Split(out, "\n") {
		if kb, ok := strings.CutPrefix(l, "MAXRSS_KB="); ok {
			fmt.Fprintf(&report, "| largest process of the backup script (max RSS) | %s kB |\n", kb)
		}
	}

	var snaps []struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(restic("snapshots", "--json")), &snaps); err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %v %+v", err, snaps)
	}
	tags := strings.Join(snaps[0].Tags, ",")
	if !strings.Contains(","+tags+",", ",can,") || strings.Contains(tags, "inconsistent") {
		t.Fatalf("tags %s", tags)
	}
	ls := restic("ls", "latest")
	for _, want := range []string{"/var/lib/can/dumps/COMPLETE", "/var/lib/can/dumps/can-core/state.db", "/var/lib/can/dumps/company.yaml", "/var/lib/can/host-state/MANIFEST.sha256", "/var/lib/can/host-state/k3s-state.db", "_mod-forms_"} {
		if !strings.Contains(ls, want) {
			t.Errorf("the snapshot lacks %s:\n%s", want, ls)
		}
	}
	// E08d: forms has its own dump in the snapshot, so its live database is not copied raw.
	for _, want := range []string{"/var/lib/can/dumps/modules/forms/manifest.json", "/var/lib/can/dumps/modules/forms/database.sqlite3.gz"} {
		if !strings.Contains(ls, want) {
			t.Errorf("the snapshot lacks %s:\n%s", want, ls)
		}
	}
	for _, l := range strings.Split(ls, "\n") {
		if strings.Contains(l, "_mod-forms_") && (strings.HasSuffix(l, "/forms.db") || strings.HasSuffix(l, "/forms.db-wal")) {
			t.Errorf("the live forms database is in the snapshot: %s", l)
		}
		if strings.Contains(l, "_can-system_can-core-data/state.db") {
			t.Errorf("can-core's live database is in the snapshot: %s", l)
		}
	}

	// `can backup status` and `verify` against the same layout
	bin := filepath.Join(t.TempDir(), "can")
	build := exec.Command("go", "build", "-o", bin, "./cmd/can") //nolint:gosec // test helper
	build.Dir = repoRoot(t)
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	for _, sub := range []string{"status", "verify"} {
		t.Log(mustSudo(t, bin, "backup", sub, "--repo", repo))
	}

	// the dumps come back from the repository, and the database opens
	target := filepath.Join(t.TempDir(), "restored")
	restic("restore", "latest", "--target", target, "--include", s.DumpsDir)
	db := target + s.DumpsDir + "/can-core/state.db"
	if got := strings.TrimSpace(mustSudo(t, "sqlite3", db, "PRAGMA integrity_check;")); got != "ok" {
		t.Fatalf("restored state.db: %s", got)
	}
	if got := mustSudo(t, "sqlite3", db, ".tables"); !strings.Contains(got, "modules") {
		t.Fatalf("restored state.db has no tables: %q", got)
	}
	t.Cleanup(func() { _, _ = sudo(t, "rm", "-rf", target) })

	// without the completion marker nothing is backed up
	mustSudo(t, "rm", s.DumpsDir+"/"+backup.MarkerName)
	if out, err := sudo(t, s.ScriptPath); err == nil || !strings.Contains(out, "FAILED(10)") {
		t.Fatalf("a missing marker must exit 10: %v\n%s", err, out)
	}

	t.Log(report.String())
	if f := os.Getenv("CAN_IT_TIMINGS"); f != "" {
		fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600) //nolint:gosec // test helper
		if err == nil {
			_, _ = fh.WriteString(report.String())
			_ = fh.Close()
		}
	}
}
