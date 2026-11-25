package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/validate"
)

func TestInitWritesValidCompany(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	code, _, errs := run(t, "init", dir, "--name", "Acme Co", "--domain", "acme.example", "--provider", "local", "--non-interactive")
	if code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if ps := validate.CompanyFile(filepath.Join(dir, "company.yaml"), "../../modules"); len(ps) != 0 {
		t.Fatalf("generated company is invalid: %v", ps)
	}
	gi, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || !strings.Contains(string(gi), ".can/") {
		t.Fatalf(".gitignore missing .can/: %v %q", err, gi)
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := run(t, "init", dir, "--name", "X Co", "--domain", "x.example", "--non-interactive")
	if code != ExitFailure || !strings.Contains(errs, "--force") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me" {
		t.Fatal("existing file was modified")
	}
	code, _, _ = run(t, "init", dir, "--name", "X Co", "--domain", "x.example", "--non-interactive", "--force")
	if code != ExitOK {
		t.Fatalf("--force: code=%d", code)
	}
}

func TestInitNonInteractiveNeedsFlags(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	code, _, errs := run(t, "init", dir) // stdin is not a terminal in tests
	if code != ExitUsage || !strings.Contains(errs, "--name") || !strings.Contains(errs, "--domain") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("directory created despite usage error")
	}
}

func TestInitRejectsInvalidValues(t *testing.T) {
	dir := t.TempDir()
	code, _, errs := run(t, "init", dir, "--name", "X", "--domain", "Not_A_Domain", "--non-interactive")
	if code != ExitUsage || !strings.Contains(errs, "domain") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "company.yaml")); err == nil {
		t.Fatal("invalid company was written")
	}
}

func TestInitInteractive(t *testing.T) {
	old := stdinIsTerminal
	stdinIsTerminal = func(any) bool { return true }
	defer func() { stdinIsTerminal = old }()
	dir := filepath.Join(t.TempDir(), "i")
	var out, errb bytes.Buffer
	code := Execute([]string{"init", dir}, strings.NewReader("Interactive Co\ninteractive.example\n\n"), &out, &errb)
	if code != ExitOK {
		t.Fatalf("code=%d err=%q out=%q", code, errb.String(), out.String())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "company.yaml"))
	if !strings.Contains(string(b), "Interactive Co") || !strings.Contains(string(b), "type: local") {
		t.Fatalf("unexpected company.yaml:\n%s", b)
	}
}

func TestDefaultTerminalDetectionIgnoresDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip("no /dev/null")
	}
	defer func() { _ = f.Close() }()
	if stdinIsTerminal(f) {
		t.Fatal("/dev/null must not be treated as an interactive terminal")
	}
	if stdinIsTerminal(strings.NewReader("")) {
		t.Fatal("a non-file reader is never a terminal")
	}
}
