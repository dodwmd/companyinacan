package backup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptEnv builds a sandbox: temp dirs for every host path and fake restic and
// logger binaries first on PATH, so the real generated script runs end to end.
type scriptEnv struct {
	dir, script, calls string
	s                  Settings
}

func newScriptEnv(t *testing.T, resticExit string) *scriptEnv {
	t.Helper()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not installed")
	}
	dir := t.TempDir()
	e := &scriptEnv{dir: dir, calls: filepath.Join(dir, "calls.log")}
	s := DefaultSettings()
	s.DumpsDir, s.StateDir, s.K3sDir, s.ConfigDir = dir+"/dumps", dir+"/state", dir+"/k3s", dir+"/conf"
	s.PVCRoot = dir + "/pvc"
	s.Repository = "s3:https://s3.example.invalid/b/r"
	e.s = s
	for _, d := range []string{s.DumpsDir, s.StateDir, s.ConfigDir, dir + "/bin"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string, mode os.FileMode) {
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(s.ConfigDir+"/"+EnvFileName, "AWS_ACCESS_KEY_ID=fake\n", 0o600)
	write(s.ConfigDir+"/"+PasswordFileName, "pw\n", 0o600)
	write(dir+"/bin/restic", fmt.Sprintf("#!/bin/sh\necho \"restic $*\" >> %s\ncase \"$1\" in backup) exit %s;; esac\nexit 0\n", e.calls, resticExit), 0o755)
	write(dir+"/bin/logger", "#!/bin/sh\nexit 0\n", 0o755)
	p, _ := testPlan()
	p.Sources = []string{s.DumpsDir}
	p.Excludes = nil
	e.script = dir + "/can-backup"
	write(e.script, GenerateScript(p, s), 0o755)
	return e
}

func (e *scriptEnv) run(t *testing.T) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", e.script)
	cmd.Env = []string{"PATH=" + e.dir + "/bin:" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

func (e *scriptEnv) calledRestic() string {
	b, _ := os.ReadFile(e.calls)
	return string(b)
}

func TestScriptRunMarkerMissing(t *testing.T) {
	e := newScriptEnv(t, "0")
	code, out := e.run(t)
	if code != ExitMarkerMissing {
		t.Fatalf("code %d, want %d\n%s", code, ExitMarkerMissing, out)
	}
	if strings.Contains(e.calledRestic(), "backup") {
		t.Error("restic backup ran without a completion marker")
	}
}

func TestScriptRunMarkerStale(t *testing.T) {
	e := newScriptEnv(t, "0")
	_ = WriteMarker(e.s.DumpsDir+"/"+MarkerName, Marker{RunID: "old", FinishedAt: time.Now().Add(-48 * time.Hour)})
	code, out := e.run(t)
	if code != ExitMarkerStale {
		t.Fatalf("code %d, want %d\n%s", code, ExitMarkerStale, out)
	}
	if strings.Contains(e.calledRestic(), "backup") {
		t.Error("restic backup ran with a stale marker")
	}
}

func TestScriptRunSuccess(t *testing.T) {
	e := newScriptEnv(t, "0")
	_ = os.WriteFile(e.s.DumpsDir+"/a.dump", []byte("data"), 0o600)
	_ = WriteMarker(e.s.DumpsDir+"/"+MarkerName, Marker{RunID: "r9", FinishedAt: time.Now()})
	code, out := e.run(t)
	if code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	calls := e.calledRestic()
	for _, want := range []string{"restic unlock", "restic backup --tag can --tag run-r9", "restic forget --keep-within 35d --prune"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing call %q in:\n%s", want, calls)
		}
	}
	m, err := os.ReadFile(e.s.DumpsDir + "/" + ManifestName)
	if err != nil || !strings.Contains(string(m), "./a.dump") {
		t.Errorf("manifest not written: %v %q", err, m)
	}
	if _, err := os.Stat(e.s.StateDir + "/backup-status"); err != nil {
		t.Error("status file missing")
	}
}

func TestScriptRunResticFailure(t *testing.T) {
	e := newScriptEnv(t, "1")
	_ = WriteMarker(e.s.DumpsDir+"/"+MarkerName, Marker{RunID: "r", FinishedAt: time.Now()})
	code, _ := e.run(t)
	if code != ExitBackupFailed {
		t.Fatalf("code %d, want %d", code, ExitBackupFailed)
	}
	if strings.Contains(e.calledRestic(), "forget") {
		t.Error("forget/prune must not run after a failed backup")
	}
}

func TestScriptRunLocked(t *testing.T) {
	e := newScriptEnv(t, "0")
	holder := exec.Command("flock", e.s.StateDir+"/backup.lock", "sleep", "5")
	if err := holder.Start(); err != nil {
		t.Skip("cannot start flock holder")
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	time.Sleep(300 * time.Millisecond)
	code, out := e.run(t)
	if code != ExitLocked {
		t.Fatalf("code %d, want %d\n%s", code, ExitLocked, out)
	}
}
