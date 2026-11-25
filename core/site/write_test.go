package site

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCreatesTree(t *testing.T) {
	out := renderFull(t)
	dir := filepath.Join(t.TempDir(), "public")
	if err := out.Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Paths() {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil || !bytes.Equal(b, out.Files[p]) {
			t.Errorf("%s: err=%v or content differs", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerFile)); err != nil {
		t.Errorf("marker file missing: %v", err)
	}
}

func TestWriteReplacesStaleFiles(t *testing.T) {
	dir := t.TempDir()
	first := renderFull(t)
	if err := first.Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := render(t, minimal)
	if err := second.Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.html")); err != nil {
		t.Error("a file this tool never generated must survive")
	}
	if _, err := os.Stat(filepath.Join(dir, "assets", "logo.svg")); !os.IsNotExist(err) {
		t.Error("asset from the previous build should have been removed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "index.html")); !bytes.Equal(b, second.Files["index.html"]) {
		t.Error("index.html not replaced")
	}
}

func TestWriteRefusesForeignDirectory(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "precious.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := render(t, minimal).Write(dir, WriteOptions{})
	if err == nil || !strings.Contains(err.Error(), "not a site output") {
		t.Fatalf("want refusal, got %v", err)
	}
	if b, _ := os.ReadFile(keep); string(b) != "mine" {
		t.Error("foreign file was modified")
	}
	if err := render(t, minimal).Write(dir, WriteOptions{Force: true}); err != nil {
		t.Fatalf("force: %v", err)
	}
}

func TestWriteDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := render(t, minimal).Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outside, "victim")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "index.html")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := render(t, minimal).Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Error("write followed a symlink out of the output directory")
	}
}

func TestWriteCompress(t *testing.T) {
	out := renderFull(t)
	dir := t.TempDir()
	if err := out.Write(dir, WriteOptions{Compress: true}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"index.html", "404.html", "sitemap.xml", "robots.txt"} {
		f, err := os.Open(filepath.Join(dir, p+".gz"))
		if err != nil {
			t.Fatalf("%s.gz: %v", p, err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(zr)
		_ = f.Close()
		if !bytes.Equal(b, out.Files[p]) {
			t.Errorf("%s.gz does not decompress to the original", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "assets", "logo.svg.gz")); err != nil {
		t.Error("svg should be precompressed")
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerFile+".gz")); err == nil {
		t.Error("marker must not be compressed")
	}
}

func TestWriteCompressDeterministic(t *testing.T) {
	out := renderFull(t)
	a, b := t.TempDir(), t.TempDir()
	if err := out.Write(a, WriteOptions{Compress: true}); err != nil {
		t.Fatal(err)
	}
	if err := out.Write(b, WriteOptions{Compress: true}); err != nil {
		t.Fatal(err)
	}
	x, _ := os.ReadFile(filepath.Join(a, "index.html.gz"))
	y, _ := os.ReadFile(filepath.Join(b, "index.html.gz"))
	if !bytes.Equal(x, y) {
		t.Error("gzip output differs between runs")
	}
}

func TestWriteRemovesStaleCompressed(t *testing.T) {
	dir := t.TempDir()
	if err := renderFull(t).Write(dir, WriteOptions{Compress: true}); err != nil {
		t.Fatal(err)
	}
	if err := renderFull(t).Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html.gz")); !os.IsNotExist(err) {
		t.Error("stale .gz would be served with outdated content")
	}
}

func TestForceNeverDeletesForeignFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"keep.txt", "sub/also.txt"} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := render(t, minimal).Write(dir, WriteOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"keep.txt", "sub/also.txt", "index.html"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	// Even once the marker exists, foreign files are never deleted.
	if err := render(t, minimal).Write(dir, WriteOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"keep.txt", "sub/also.txt"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s was deleted from a marked directory", f)
		}
	}
}

func TestMarkedDirectoryKeepsForeignFilesAndRemovesStaleGenerated(t *testing.T) {
	dir := t.TempDir()
	if err := renderFull(t).Write(dir, WriteOptions{Compress: true}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"precious.txt", "docs/a.md"} {
		p := filepath.Join(dir, filepath.FromSlash(f))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := render(t, minimal).Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"precious.txt", "docs/a.md"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("foreign %s deleted", f)
		}
	}
	for _, f := range []string{"assets/logo.svg", "index.html.gz", "sitemap.xml.gz"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); !os.IsNotExist(err) {
			t.Errorf("stale generated %s should be removed", f)
		}
	}
	m, _ := os.ReadFile(filepath.Join(dir, MarkerFile))
	if !strings.Contains(string(m), "\nindex.html\n") || strings.Contains(string(m), "precious") {
		t.Errorf("marker content: %q", m)
	}
}

func TestLegacyEmptyMarkerDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	for f, c := range map[string]string{MarkerFile: "generated\n", "old.html": "x"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := render(t, minimal).Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.html")); err != nil {
		t.Error("legacy marker must not authorise deletions")
	}
}

func TestMarkedDirectoryWithCompanyYAMLIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := render(t, minimal).Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	cy := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(cy, []byte("schema: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := render(t, minimal).Write(dir, WriteOptions{Force: true}); err == nil || !strings.Contains(err.Error(), "company.yaml") {
		t.Fatalf("want refusal, got %v", err)
	}
	if _, err := os.Stat(cy); err != nil {
		t.Error("company.yaml deleted")
	}
}

func TestMarkerCannotAuthoriseEscapes(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside-"+filepath.Base(dir))
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(outside) }()
	marker := markerHeader + "../" + filepath.Base(outside) + "\n/etc/passwd\ncompany.yaml\n"
	if err := os.WriteFile(filepath.Join(dir, MarkerFile), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := render(t, minimal).Write(dir, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("a hostile marker deleted a file outside the directory")
	}
}

func TestWriteRefusesCompanyDirectory(t *testing.T) {
	dir := t.TempDir()
	cy := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(cy, []byte("schema: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		err := render(t, minimal).Write(dir, WriteOptions{Force: force})
		if err == nil || !strings.Contains(err.Error(), "company.yaml") {
			t.Fatalf("force=%v: want refusal, got %v", force, err)
		}
	}
	if b, _ := os.ReadFile(cy); string(b) != "schema: 1\n" {
		t.Error("company.yaml was modified")
	}
}
