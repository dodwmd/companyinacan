package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const applyCompany = "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nmodules:\n  example: {}\n"

func TestApplyDryRunPlansWithoutWritingState(t *testing.T) {
	dir := writeCompany(t, applyCompany)
	code, out, errs := run(t, "apply", dir, "--modules-dir", "../../modules", "--dry-run")
	if code != ExitOK || !strings.Contains(out, "planned install: example") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, ".can")); err == nil {
		t.Fatal("dry run created state")
	}
}

func TestApplyRecordOnlyIsIdempotent(t *testing.T) {
	dir := writeCompany(t, applyCompany)
	args := []string{"apply", dir, "--modules-dir", "../../modules", "--record-only"}
	if code, out, errs := run(t, args...); code != ExitOK || !strings.Contains(out, "applied install: example") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	code, out, _ := run(t, args...)
	if code != ExitOK || !strings.Contains(out, "nothing to do") {
		t.Fatalf("second apply: code=%d out=%q", code, out)
	}
	// disabling in company.yaml disables the recorded module
	off := strings.Replace(applyCompany, "example: {}", "example: {enabled: false}", 1)
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), []byte(off), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, args...); code != ExitOK || !strings.Contains(out, "applied disable: example") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	// dry-run now plans against the recorded state and reports nothing to do
	code, out, _ = run(t, "apply", dir, "--modules-dir", "../../modules", "--dry-run", "--output", "json")
	var res struct {
		Actions []any `json:"actions"`
	}
	if code != ExitOK || json.Unmarshal([]byte(out), &res) != nil || len(res.Actions) != 0 {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestApplyRejectsInvalidCompanyBeforeDoingAnything(t *testing.T) {
	dir := writeCompany(t, "schema: 1\nname: A\nprovider: {type: local}\nmodules:\n  ghost: {}\n")
	code, out, _ := run(t, "apply", dir, "--modules-dir", "../../modules", "--record-only")
	if code != ExitFailure || !strings.Contains(out, "semantic.module-manifest-missing") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".can")); err == nil {
		t.Fatal("state created for an invalid company")
	}
}
