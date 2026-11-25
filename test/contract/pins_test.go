package contract

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Placeholder digests are allowed in development, but the release check must catch them.
func TestImagePinCheck(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	script := filepath.Join(root, "scripts", "check-image-pins.sh")

	tmp := t.TempDir()
	dir := filepath.Join(tmp, "deploy", "x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM a@sha256:REPLACE_WITH_DIGEST\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", script, tmp).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "REPLACE_WITH_") {
		t.Fatalf("placeholder not detected: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM a@sha256:abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", script, tmp).CombinedOutput(); err != nil {
		t.Fatalf("pinned Dockerfile rejected: %v %s", err, out)
	}
}
