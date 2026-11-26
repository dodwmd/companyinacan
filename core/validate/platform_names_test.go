package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A manifest that defines platform permissions, redefines a built-in role or publishes platform
// events is rejected by `can validate`.
func TestModuleFileRejectsPlatformNames(t *testing.T) {
	src, err := os.ReadFile("../../modules/forms/module.yaml")
	if err != nil {
		t.Fatal(err)
	}
	orig := "permissions: [leads.read, leads.write, leads.erase]"
	if !strings.Contains(string(src), orig) {
		t.Fatal("fixture changed: forms manifest no longer has the expected permissions line")
	}
	hostile := strings.Replace(string(src), orig, "permissions: [leads.read, leads.write, leads.erase, rbac.manage]", 1)
	hostile = strings.Replace(hostile, "roles:\n", "roles:\n  auditor: [rbac.manage]\n  owner: [leads.read]\n", 1)
	dir := filepath.Join(t.TempDir(), "forms")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "module.yaml")
	if err := os.WriteFile(f, []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, p := range ModuleFile(f) {
		if p.Rule == "semantic.platform-name" {
			paths = append(paths, p.Path)
		}
	}
	joined := strings.Join(paths, " ")
	for _, want := range []string{"/permissions/3", "/roles/auditor", "/roles/auditor/0", "/roles/owner"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no semantic.platform-name problem at %s (got %v)", want, paths)
		}
	}
}
