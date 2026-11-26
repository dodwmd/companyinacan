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
	s.HostStateDir = dir + "/state/host"
	s.LedgerSource = dir + "/ledger.jsonl"
	s.CanBin = dir + "/bin/can"
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
	write(dir+"/bin/restic", fmt.Sprintf("#!/bin/sh\necho \"restic $*\" >> %s\ncase \"$1\" in backup) [ -x %s/on-backup ] && %s/on-backup; exit %s;; esac\nexit 0\n", e.calls, dir, dir, resticExit), 0o755)
	write(dir+"/bin/can", fmt.Sprintf("#!/bin/sh\n[ \"$*\" = \"backup --help\" ] && { cat %s/can-help 2>/dev/null || echo ledger-import; exit 0; }\necho \"can $*\" >> %s\nexit $(cat %s/can-rc 2>/dev/null || echo 0)\n", dir, e.calls, dir), 0o755)
	write(dir+"/bin/curl", fmt.Sprintf("#!/bin/sh\necho \"curl $*\" >> %s\ncat >> %s/curl.stdin\n", e.calls, dir), 0o755)
	write(dir+"/bin/logger", "#!/bin/sh\nexit 0\n", 0o755)
	p, _ := testPlan()
	p.Sources = []string{s.DumpsDir, s.HostStateDir}
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
	if strings.Contains(e.calledRestic(), "restic backup") {
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
	if strings.Contains(e.calledRestic(), "restic backup") {
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
	for _, want := range []string{"restic unlock", "restic backup --tag can --tag run-r9", "restic forget --group-by  --keep-within 35d --prune --tag can"} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing call %q in:\n%s", want, calls)
		}
	}
	m, err := os.ReadFile(e.s.HostStateDir + "/" + ManifestName)
	if err != nil || !strings.Contains(string(m), "./a.dump") {
		t.Errorf("manifest not written: %v %q", err, m)
	}
	if _, err := os.Stat(e.s.StateDir + "/backup-status"); err != nil {
		t.Error("status file missing")
	}
}

// Staging directories and temporaries (dot entries) are neither listed in the manifest nor backed up.
func TestScriptRunLeavesTemporariesOut(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	for _, p := range []string{".run-x/tmp/.upload-1", "can-core/.state.db.tmp", "modules/wiki/.upload-2"} {
		full := e.s.DumpsDir + "/" + p
		_ = os.MkdirAll(full[:strings.LastIndex(full, "/")], 0o700)
		if err := os.WriteFile(full, []byte("half"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := e.run(t); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	m, _ := os.ReadFile(e.s.HostStateDir + "/" + ManifestName)
	if !strings.Contains(string(m), "./a.dump") || strings.Contains(string(m), "/.") {
		t.Errorf("manifest: %q", m)
	}
	for _, want := range []string{"--exclude " + e.s.DumpsDir + "/.*", "--exclude " + e.s.DumpsDir + "/**/.*"} {
		if !strings.Contains(e.calledRestic(), want) {
			t.Errorf("restic not told %q:\n%s", want, e.calledRestic())
		}
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

func (e *scriptEnv) fresh(t *testing.T) {
	t.Helper()
	_ = os.WriteFile(e.s.DumpsDir+"/a.dump", []byte("x"), 0o600)
	_ = WriteMarker(e.s.DumpsDir+"/"+MarkerName, Marker{RunID: "r1", FinishedAt: time.Now()})
}

func TestScriptRunMarkerChangedDuringBackup(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	hook := fmt.Sprintf("#!/bin/sh\nprintf 'run_id=r2\\nfinished_at=%d\\n' > %s/%s\n", time.Now().Unix(), e.s.DumpsDir, MarkerName)
	_ = os.WriteFile(e.dir+"/on-backup", []byte(hook), 0o755)
	code, out := e.run(t)
	if code != ExitMarkerStale {
		t.Fatalf("code %d\n%s", code, out)
	}
	calls := e.calledRestic()
	if !strings.Contains(calls, "restic tag --tag attempt-") || !strings.Contains(calls, "--add inconsistent") {
		t.Errorf("snapshot not tagged inconsistent:\n%s", calls)
	}
	if strings.Contains(calls, "forget") {
		t.Error("forget ran after an inconsistent snapshot")
	}
}

func TestScriptRunLedgerSyncedEvenWhenBackupFails(t *testing.T) {
	e := newScriptEnv(t, "1")
	e.fresh(t)
	_ = os.WriteFile(e.s.LedgerSource, []byte("{}\n"), 0o600)
	code, _ := e.run(t)
	if code != ExitBackupFailed {
		t.Fatalf("code %d, want the backup failure to win", code)
	}
	if b, _ := os.ReadFile(e.calls); !strings.Contains(string(b), "can backup ledger-sync") {
		t.Errorf("ledger not synced:\n%s", b)
	}
	if _, err := os.Stat(e.s.HostStateDir + "/" + LedgerCopy); err != nil {
		t.Error("ledger copy missing from host state (it goes into the snapshot)")
	}
}

func TestScriptRunLedgerSyncedWithoutMarker(t *testing.T) {
	e := newScriptEnv(t, "0")
	_ = os.WriteFile(e.s.LedgerSource, []byte("{}\n"), 0o600)
	code, _ := e.run(t)
	if code != ExitMarkerMissing {
		t.Fatalf("code %d", code)
	}
	if b, _ := os.ReadFile(e.calls); !strings.Contains(string(b), "can backup ledger-sync") {
		t.Error("ledger not synced when the marker is missing")
	}
}

func TestScriptRunLedgerFailureCode(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	_ = os.WriteFile(e.s.LedgerSource, []byte("{}\n"), 0o600)
	_ = os.WriteFile(e.dir+"/can-rc", []byte("1"), 0o600)
	code, _ := e.run(t)
	if code != ExitLedgerFailed {
		t.Fatalf("code %d, want %d", code, ExitLedgerFailed)
	}
	if !strings.Contains(e.calledRestic(), "restic forget") {
		t.Error("backup and forget must still run when only the ledger failed")
	}
}

// A company that never erased anything must still be restorable: the bucket gets an empty ledger.
// Without `can` on the host nothing changes, and a ledger that vanished is not replaced.
func TestScriptRunUploadsAnEmptyLedger(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	if code, out := e.run(t); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(e.calls); !strings.Contains(string(b), "can backup ledger-sync") {
		t.Errorf("the empty ledger was not synced:\n%s", b)
	}
	if st, err := os.Stat(e.s.LedgerSource); err != nil || st.Size() != 0 {
		t.Errorf("no empty ledger: %v", err)
	}

	without := newScriptEnv(t, "0")
	without.fresh(t)
	if err := os.Remove(without.s.CanBin); err != nil {
		t.Fatal(err)
	}
	if code, out := without.run(t); code != 0 {
		t.Fatalf("without can: code %d\n%s", code, out)
	}
	if _, err := os.Stat(without.s.LedgerSource); !os.IsNotExist(err) {
		t.Error("a ledger was made that nothing can upload")
	}

	vanished := newScriptEnv(t, "0")
	vanished.fresh(t)
	if err := os.MkdirAll(vanished.s.HostStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vanished.s.HostStateDir+"/"+LedgerCopy, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := vanished.run(t); code != ExitLedgerFailed || !strings.Contains(out, "vanished") {
		t.Fatalf("vanished ledger: code %d\n%s", code, out)
	}
	if _, err := os.Stat(vanished.s.LedgerSource); !os.IsNotExist(err) {
		t.Error("a vanished ledger was replaced by an empty one")
	}
}

func TestScriptRunResticExit3IsASnapshot(t *testing.T) {
	e := newScriptEnv(t, "3")
	e.fresh(t)
	code, out := e.run(t)
	if code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if !strings.Contains(e.calledRestic(), "restic forget") {
		t.Error("exit 3 is a created snapshot; forget must run")
	}
	if !strings.Contains(out, "exit 3") {
		t.Error("exit 3 not reported explicitly")
	}
}

func TestScriptRunRefusesSymlinkInDumps(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	_ = os.Symlink("/etc/shadow", e.s.DumpsDir+"/evil")
	code, _ := e.run(t)
	if code != ExitUnsafeDumps {
		t.Fatalf("code %d, want %d", code, ExitUnsafeDumps)
	}
	if strings.Contains(e.calledRestic(), "restic backup") {
		t.Error("backup ran with a symlink in the dumps dir")
	}
}

func TestScriptRunRefusesWorldWritableDumps(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	_ = os.Chmod(e.s.DumpsDir, 0o777)
	if code, _ := e.run(t); code != ExitUnsafeDumps {
		t.Fatalf("code %d", code)
	}
}

func TestScriptRunHostStateOutsideDumps(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	if code, out := e.run(t); code != 0 {
		t.Fatalf("code %d %s", code, out)
	}
	if _, err := os.Stat(e.s.DumpsDir + "/" + ManifestName); err == nil {
		t.Error("manifest written into the can-core-writable dumps dir")
	}
	if b, err := os.ReadFile(e.s.HostStateDir + "/" + MarkerCopy); err != nil || !strings.Contains(string(b), "run_id=r1") {
		t.Errorf("marker copy: %v %q", err, b)
	}
	// Each snapshot is ungrouped and the host state dir is a restic source.
	if c := e.calledRestic(); !strings.Contains(c, e.s.HostStateDir) {
		t.Errorf("host state not backed up:\n%s", c)
	}
}

func TestScriptRunWebhookSecretNotOnArgv(t *testing.T) {
	e := newScriptEnv(t, "1")
	e.fresh(t)
	_ = os.WriteFile(e.s.ConfigDir+"/"+WebhookFileName, []byte("https://hooks.example.invalid/SECRETTOKEN\n"), 0o600)
	e.run(t)
	b, _ := os.ReadFile(e.calls)
	if strings.Contains(string(b), "SECRETTOKEN") {
		t.Errorf("webhook secret on argv:\n%s", b)
	}
	in, _ := os.ReadFile(e.dir + "/curl.stdin")
	if !strings.Contains(string(in), `url = "https://hooks.example.invalid/SECRETTOKEN"`) {
		t.Errorf("curl config not on stdin: %q", in)
	}
	if !strings.Contains(string(b), "-K -") {
		t.Errorf("curl -K - missing:\n%s", b)
	}
}

func TestScriptJSONEscape(t *testing.T) {
	e := newScriptEnv(t, "0")
	body, _ := os.ReadFile(e.script)
	fn := string(body[strings.Index(string(body), "json_escape() {"):])
	fn = fn[:strings.Index(fn, "\n}\n")+3]
	cmd := exec.Command("bash", "-c", fn+`json_escape "$1"`, "x", "a\"b\\c\nd\te\x01f")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `a\"b\\c d ef` {
		t.Errorf("escaped = %q", out)
	}
}

func TestScriptStateDirCreatedBeforeLock(t *testing.T) {
	e := newScriptEnv(t, "0")
	_ = os.RemoveAll(e.s.StateDir)
	e.fresh(t)
	if code, out := e.run(t); code != 0 {
		t.Fatalf("code %d %s", code, out)
	}
}

func TestScriptRunSymlinkedLedgerIsRefused(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	_ = os.WriteFile(e.dir+"/elsewhere", []byte("{}\n"), 0o600)
	_ = os.Symlink(e.dir+"/elsewhere", e.s.LedgerSource)
	code, out := e.run(t)
	if code != ExitUnsafeDumps || !strings.Contains(out, "symlink") {
		t.Fatalf("code %d\n%s", code, out)
	}
}

func TestScriptRunVanishedLedgerIsReported(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	_ = os.WriteFile(e.s.LedgerSource, []byte("{}\n"), 0o600)
	if code, out := e.run(t); code != 0 {
		t.Fatalf("first run: %d\n%s", code, out)
	}
	_ = os.Remove(e.s.LedgerSource)
	e.fresh(t)
	code, out := e.run(t)
	if code != ExitLedgerFailed || !strings.Contains(out, "vanished") {
		t.Fatalf("code %d\n%s", code, out)
	}
	if !strings.Contains(e.calledRestic(), "restic forget") {
		t.Error("the backup itself must still complete")
	}
}

func TestScriptRunNeverHadALedgerIsFine(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	if code, out := e.run(t); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
}

func TestScriptRunRefusesGroupWritableHostState(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	_ = os.MkdirAll(e.s.HostStateDir, 0o755)
	_ = os.Chmod(e.s.HostStateDir, 0o775)
	if code, out := e.run(t); code != ExitUnsafeDumps || !strings.Contains(out, "writable") {
		t.Fatalf("code %d\n%s", code, out)
	}
}

func TestScriptRunRefusesSymlinkedStateDir(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	_ = os.RemoveAll(e.s.HostStateDir)
	_ = os.MkdirAll(e.dir+"/elsewhere", 0o700)
	_ = os.Symlink(e.dir+"/elsewhere", e.s.HostStateDir)
	if code, _ := e.run(t); code != ExitUnsafeDumps {
		t.Fatalf("code %d", code)
	}
}

// QA 2: raw SQLite files are left out only for modules can-core says it dumped in this run.
func TestScriptRunExcludesOnlyDumpedSQLite(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	list := "forms sqlite\nwiki none\n../evil sqlite\nbad;rm sqlite\n"
	if err := os.WriteFile(e.s.DumpsDir+"/DUMPED", []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := e.run(t); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	calls := e.calledRestic()
	for _, want := range []string{"/pvc-*_mod-forms_*/**/*.db ", "/pvc-*_mod-forms_*/**/*-wal "} {
		if !strings.Contains(calls, "--exclude "+e.s.PVCRoot+want) {
			t.Errorf("missing exclusion %s:\n%s", want, calls)
		}
	}
	for _, bad := range []string{"mod-wiki", "evil", "bad;rm"} {
		if strings.Contains(calls, bad) {
			t.Errorf("unexpected exclusion for %s:\n%s", bad, calls)
		}
	}
}

func TestScriptRunWithoutDumpedListExcludesNoModule(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	if code, out := e.run(t); code != 0 {
		t.Fatalf("code %d\n%s", code, out)
	}
	if strings.Contains(e.calledRestic(), "_mod-") {
		t.Errorf("exclusions without a dump list:\n%s", e.calledRestic())
	}
}

// E08b QA3 4: a database left out of the dumps fires the failure webhook, although the backup
// of everything else succeeds.
func TestScriptRunAnnouncesSkippedDatabases(t *testing.T) {
	e := newScriptEnv(t, "0")
	e.fresh(t)
	if err := os.WriteFile(e.s.ConfigDir+"/"+WebhookFileName, []byte("https://hooks.example/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.s.DumpsDir+"/"+AlertsName, []byte("module \"crm\" was NOT dumped. Its DATABASE IS NOT BACKED UP.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := e.run(t)
	if code != 0 || !strings.Contains(out, "WARNING") || !strings.Contains(out, "DATABASE IS NOT BACKED UP") {
		t.Fatalf("code %d\n%s", code, out)
	}
	if !strings.Contains(e.calledRestic(), "curl ") || !strings.Contains(e.calledRestic(), "DATABASE IS NOT BACKED UP") {
		t.Errorf("webhook not fired:\n%s", e.calledRestic())
	}
}
