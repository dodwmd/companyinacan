//go:build integration

package integration

// `can export` against a running cluster (specs/071-company-export), used by the module backup
// and wrapped backup tests.

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const exportPassphrase = "a throw-away passphrase for CI"

// companyExport runs `can export --include-sealed` with a passphrase against the cluster,
// decrypts the file with `can export decrypt` and returns its entries.
func companyExport(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	sealed := filepath.Join(dir, "export.tar.gz.canenc")
	canCLI := func(args ...string) {
		t.Helper()
		cmd := exec.Command("go", append([]string{"run", "./cmd/can"}, args...)...) //nolint:gosec // test helper
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(), "CAN_EXPORT_PASSPHRASE="+exportPassphrase)
		out, err := cmd.CombinedOutput()
		if err != nil {
			logs, _ := kubectl("-n", "can-system", "logs", "deploy/can-core", "--tail=80")
			t.Fatalf("can %v: %v\n%s\ncan-core log:\n%s", args, err, out, logs)
		}
		t.Logf("can %v: %s", args, strings.TrimSpace(string(out)))
	}
	start := time.Now()
	canCLI("export", "--context", kubeContext(), "--include-sealed", "--out", sealed)
	t.Logf("MEASURE can export: %s", time.Since(start).Round(100*time.Millisecond))
	canCLI("export", "decrypt", sealed)
	f, err := os.Open(strings.TrimSuffix(sealed, ".canenc")) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(io.LimitReader(tr, 32<<20))
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = string(b)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	t.Logf("export holds %d entries; manifest:\n%s", len(names), files["manifest.json"])
	return files
}

// holds fails unless the entry exists and holds want.
func holds(t *testing.T, files map[string]string, name, want string) {
	t.Helper()
	body, ok := files[name]
	if !ok {
		t.Errorf("the export lacks %s", name)
	} else if !strings.Contains(body, want) {
		t.Errorf("%s lacks %q:\n%.2000s", name, want, body)
	}
}
