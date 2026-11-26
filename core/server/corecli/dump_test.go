package corecli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
)

func dumpCLI(env map[string]string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Execute(args, strings.NewReader(""), &out, &errb, func(k string) string { return env[k] })
	return code, out.String(), errb.String()
}

func TestDumpRequestCreatesTheRequestFile(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"CAN_DB_PATH": filepath.Join(dir, "state.db"), "CAN_DUMPS_DIR": filepath.Join(dir, "dumps")}
	code, out, errb := dumpCLI(env, "dump-request")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errb)
	}
	if _, err := os.Stat(filepath.Join(dir, "dump.request")); err != nil {
		t.Fatal(err)
	}
}

func TestDumpRequestNeedsDumpsConfigured(t *testing.T) {
	if code, _, _ := dumpCLI(map[string]string{"CAN_DB_PATH": filepath.Join(t.TempDir(), "s.db")}, "dump-request"); code == 0 {
		t.Fatal("without CAN_DUMPS_DIR nothing would act on the request")
	}
}

func TestDumpRequestWaitsForANewMarker(t *testing.T) {
	dir := t.TempDir()
	dumpsDir := filepath.Join(dir, "dumps")
	if err := os.MkdirAll(dumpsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dumpsDir, backup.MarkerName)
	if err := backup.WriteMarker(marker, backup.Marker{RunID: "old", FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CAN_DB_PATH": filepath.Join(dir, "state.db"), "CAN_DUMPS_DIR": dumpsDir}
	if code, _, _ := dumpCLI(env, "dump-request", "--wait", "100ms"); code == 0 {
		t.Fatal("the old marker must not count")
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = backup.WriteMarker(marker, backup.Marker{RunID: "new", FinishedAt: time.Now()})
	}()
	code, out, errb := dumpCLI(env, "dump-request", "--wait", "10s")
	if code != 0 || !strings.Contains(out, "new") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
}

// E08b QA3 4: a run that left a database out says so in `dump-request --wait`.
func TestDumpRequestWaitPrintsSkippedModules(t *testing.T) {
	dir := t.TempDir()
	dumpsDir := filepath.Join(dir, "dumps")
	if err := os.MkdirAll(dumpsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CAN_DB_PATH": filepath.Join(dir, "state.db"), "CAN_DUMPS_DIR": dumpsDir}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = backup.WriteDumpRun(dumpsDir, backup.DumpRun{RunID: "new", OK: true, Skipped: []backup.SkippedModule{{ID: "crm", Reason: "disabled", Database: "mariadb"}}})
		_ = backup.WriteMarker(filepath.Join(dumpsDir, backup.MarkerName), backup.Marker{RunID: "new", FinishedAt: time.Now()})
	}()
	code, out, errb := dumpCLI(env, "dump-request", "--wait", "10s")
	if code != 0 || !strings.Contains(out, "WARNING:") || !strings.Contains(out, "DATABASE IS NOT BACKED UP") {
		t.Fatalf("%d %q %q", code, out, errb)
	}
}

// A run that fails writes no marker: the operator is told why at once.
func TestDumpRequestWaitReportsAFailedRun(t *testing.T) {
	dir := t.TempDir()
	dumpsDir := filepath.Join(dir, "dumps")
	if err := os.MkdirAll(dumpsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"CAN_DB_PATH": filepath.Join(dir, "state.db"), "CAN_DUMPS_DIR": dumpsDir}
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = backup.WriteDumpRun(dumpsDir, backup.DumpRun{RunID: "new", Error: "company.yaml: not there yet"})
	}()
	code, _, errb := dumpCLI(env, "dump-request", "--wait", "30s")
	if code == 0 || !strings.Contains(errb, "company.yaml: not there yet") {
		t.Fatalf("%d %q", code, errb)
	}
}
