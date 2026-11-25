package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func modulesDirWith(t *testing.T, id, provider string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, id)
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatal(err)
	}
	body := "schema: 1\nid: " + id + "\nprovider: " + provider + "\n"
	if err := os.WriteFile(filepath.Join(p, "module.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLicencesWriteThenCheck(t *testing.T) {
	mods := modulesDirWith(t, "wrapped", "{type: oss-wrapper, name: W, licence: MIT, upstream: 'https://w.example'}")
	file := filepath.Join(t.TempDir(), "THIRD_PARTY_LICENSES")
	code, _, errs := run(t, "licences", "--modules-dir", mods, "--file", file, "--check")
	if code != ExitFailure || !strings.Contains(errs+"", "") {
		t.Fatalf("check without a file should fail, code=%d", code)
	}
	if code, _, errs = run(t, "licences", "--modules-dir", mods, "--file", file, "--write"); code != ExitOK {
		t.Fatalf("write: code=%d err=%q", code, errs)
	}
	first, _ := os.ReadFile(file)
	if code, _, _ = run(t, "licences", "--modules-dir", mods, "--file", file, "--write"); code != ExitOK {
		t.Fatal("second write failed")
	}
	second, _ := os.ReadFile(file)
	if string(first) != string(second) {
		t.Fatal("write is not idempotent")
	}
	if code, out, _ := run(t, "licences", "--modules-dir", mods, "--file", file, "--check"); code != ExitOK {
		t.Fatalf("check after write: code=%d out=%q", code, out)
	}
	if err := os.WriteFile(file, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "licences", "--modules-dir", mods, "--file", file, "--check")
	if code != ExitFailure || !strings.Contains(out, "licence.notice-stale") || !strings.Contains(out, "make licences") {
		t.Fatalf("stale check: code=%d out=%q", code, out)
	}
}

func TestLicencesViolationNamesModuleAndBlocksWrite(t *testing.T) {
	mods := modulesDirWith(t, "badmod", "{type: oss-wrapper, name: B, licence: Elastic-2.0, upstream: 'https://b.example'}")
	file := filepath.Join(t.TempDir(), "THIRD_PARTY_LICENSES")
	code, out, _ := run(t, "licences", "--modules-dir", mods, "--file", file, "--write")
	if code != ExitFailure || !strings.Contains(out, "badmod") || !strings.Contains(out, "licence.not-accepted") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("notice must not be written when there are violations")
	}
}

func TestLicencesEmptyModulesDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "THIRD_PARTY_LICENSES")
	if code, _, errs := run(t, "licences", "--modules-dir", t.TempDir(), "--file", file, "--write"); code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), "None") {
		t.Fatalf("empty notice: %s", b)
	}
}
