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
	if _, err := os.Stat(filepath.Join(dir, "stale.html")); !os.IsNotExist(err) {
		t.Error("stale file should have been removed")
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
