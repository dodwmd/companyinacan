package staticsrv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name string
	body string
	typ  byte
}

func makeTarGz(t *testing.T, es []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range es {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Size: int64(len(e.body))}
		if typ == tar.TypeSymlink || typ == tar.TypeLink {
			h.Linkname = e.body
			h.Size = 0
		}
		if typ == tar.TypeDir || typ == tar.TypeChar {
			h.Mode, h.Size = 0o755, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestExtractWritesFilesAndDirs(t *testing.T) {
	dir := t.TempDir()
	arc := makeTarGz(t, []entry{
		{name: "index.html", body: "<h1>hi</h1>"},
		{name: "assets/", typ: tar.TypeDir},
		{name: "assets/site.abcd1234.css", body: "body{}"},
		{name: ".can-site", body: "marker"},
	})
	if err := Extract(bytes.NewReader(arc), dir, 1<<20); err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{"index.html": "<h1>hi</h1>", "assets/site.abcd1234.css": "body{}", ".can-site": "marker"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil || string(b) != want {
			t.Errorf("%s: %q %v", f, b, err)
		}
	}
}

func TestExtractRefusesEscapesAndSpecials(t *testing.T) {
	for name, es := range map[string][]entry{
		"dotdot":   {{name: "../evil", body: "x"}},
		"absolute": {{name: "/etc/evil", body: "x"}},
		"nested":   {{name: "a/../../evil", body: "x"}},
		"symlink":  {{name: "link", body: "/etc/passwd", typ: tar.TypeSymlink}},
		"hardlink": {{name: "h", body: "index.html", typ: tar.TypeLink}},
		"device":   {{name: "dev", typ: tar.TypeChar}},
	} {
		dir := t.TempDir()
		if err := Extract(bytes.NewReader(makeTarGz(t, es)), dir, 1<<20); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
			t.Errorf("%s: escaped the directory", name)
		}
	}
}

func TestExtractEnforcesSizeCap(t *testing.T) {
	dir := t.TempDir()
	arc := makeTarGz(t, []entry{{name: "big", body: strings.Repeat("a", 4096)}})
	err := Extract(bytes.NewReader(arc), dir, 1024)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want size cap error, got %v", err)
	}
}

func TestExtractRejectsGarbage(t *testing.T) {
	if err := Extract(strings.NewReader("not gzip"), t.TempDir(), 1<<20); err == nil {
		t.Fatal("want error")
	}
}
