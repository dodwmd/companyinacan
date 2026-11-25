package backup

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestResticPinsAreRealDigests(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for arch, sum := range ResticSHA256 {
		if !hex64.MatchString(sum) {
			t.Errorf("%s: %q is not a sha256", arch, sum)
		}
	}
	if len(ResticSHA256) != 2 {
		t.Errorf("want amd64 and arm64 pins, got %v", ResticSHA256)
	}
}

// runInstall runs the host tool script with a PATH holding only the given fakes plus the real
// shell utilities, installing into a temp dir.
func runInstall(t *testing.T, fakes map[string]string) (string, string, error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil { //nolint:gosec // test
		t.Fatal(err)
	}
	for name, body := range fakes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil { //nolint:gosec // test
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", HostToolsScript(filepath.Join(dir, "restic"))) //nolint:gosec // test
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return dir, string(out), err
}

func TestHostToolsKeepANewEnoughRestic(t *testing.T) {
	_, out, err := runInstall(t, map[string]string{
		"restic":  `echo "restic 0.18.0 compiled with go1.24 on linux/amd64"`,
		"sqlite3": `exit 0`,
		"curl":    `echo "curl must not run" >&2; exit 1`,
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestHostToolsRefuseADownloadWithTheWrongDigest(t *testing.T) {
	dir, out, err := runInstall(t, map[string]string{
		"restic":  `echo "restic 0.14.0 compiled"`,
		"sqlite3": `exit 0`,
		"curl":    `while [ $# -gt 1 ]; do if [ "$1" = -o ]; then echo evil >"$2"; fi; shift; done`,
	})
	if err == nil || !strings.Contains(out, "checksum") {
		t.Fatalf("a download with the wrong digest must fail: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "restic")); err == nil {
		t.Fatal("nothing may be installed after a digest mismatch")
	}
}
