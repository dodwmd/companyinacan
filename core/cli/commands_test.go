package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCompany(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestValidateOK(t *testing.T) {
	dir := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\n")
	code, out, errs := run(t, "validate", dir, "--modules-dir", "../../modules")
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestValidateReportsAllProblemsInOneRun(t *testing.T) {
	dir := writeCompany(t, "schema: 2\nname: \"\"\ndomain: BAD\nprovider: {type: nope}\nextra: 1\n")
	code, out, _ := run(t, "validate", dir, "--modules-dir", "../../modules")
	if code != ExitFailure {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"/schema", "/name", "/domain", "/provider/type", "/extra", "company.yaml:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestValidateJSON(t *testing.T) {
	dir := writeCompany(t, "schema: 1\nname: A\nprovider: {type: local}\n")
	code, out, _ := run(t, "validate", dir, "--output", "json", "--modules-dir", "../../modules")
	if code != ExitFailure {
		t.Fatalf("code=%d", code)
	}
	var ps []map[string]any
	if err := json.Unmarshal([]byte(out), &ps); err != nil || len(ps) != 1 || ps[0]["rule"] != "schema.required" {
		t.Fatalf("bad json: %v %q", err, out)
	}
}

func TestValidateMissingFile(t *testing.T) {
	code, out, _ := run(t, "validate", t.TempDir())
	if code != ExitFailure || !strings.Contains(out, "parse.read") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != ExitOK || !strings.HasPrefix(out, "can ") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	code, out, _ = run(t, "version", "--output", "json")
	var m map[string]string
	if code != ExitOK || json.Unmarshal([]byte(out), &m) != nil || m["version"] == "" || m["go"] == "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestApplyRefusesToDeployWithoutAnInstaller(t *testing.T) {
	dir := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\n")
	code, out, errs := run(t, "apply", dir, "--modules-dir", "../../modules")
	if code != ExitNotImplemented || out != "" || !strings.Contains(errs, "E03") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestDoctorExitCodes(t *testing.T) {
	old := lookPath
	defer func() { lookPath = old }()
	lookPath = func(n string) (string, error) {
		if n == "go" || n == "git" || n == "make" {
			return "/bin/" + n, nil
		}
		return "", os.ErrNotExist
	}
	code, out, _ := run(t, "doctor")
	if code != ExitOK || !strings.Contains(out, "optional") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	code, _, _ = run(t, "doctor")
	if code != ExitFailure {
		t.Fatalf("code=%d, want failure when required tools are missing", code)
	}
	lookPath = func(n string) (string, error) { return "/bin/" + n, nil }
	code, out, _ = run(t, "doctor", "--output", "json")
	var rs []map[string]any
	if code != ExitOK || json.Unmarshal([]byte(out), &rs) != nil || len(rs) == 0 {
		t.Fatalf("code=%d out=%q", code, out)
	}
}
