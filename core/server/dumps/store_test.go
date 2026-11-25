package dumps

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func turnFor(t *testing.T, module string) *Dumper {
	t.Helper()
	d := &Dumper{Store: &Store{Dir: filepath.Join(t.TempDir(), "dumps")}}
	if err := d.BeginForTest(module); err != nil {
		t.Fatal(err)
	}
	return d
}

func staged(d *Dumper, module, name string) string {
	return filepath.Join(d.cur.dir, "modules", module, name)
}

func TestAcceptStoresA0600File(t *testing.T) {
	d := turnFor(t, "wiki")
	if err := d.Accept("wiki", "storage.tar.gz", strings.NewReader("payload")); err != nil {
		t.Fatal(err)
	}
	p := staged(d, "wiki", "storage.tar.gz")
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "payload" {
		t.Fatalf("%q %v", b, err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestAcceptRejectsBadNamesAndTraversal(t *testing.T) {
	d := turnFor(t, "wiki")
	for _, c := range [][2]string{{"..", "x"}, {"wiki", ".."}, {"wiki", "a/b"}, {"wiki", ".hidden"}, {"Wiki", "x"}, {"wiki", ""}, {"", "x"}, {"wiki", COMPLETE}, {"wiki", strings.Repeat("a", 200)}} {
		if err := d.Accept(c[0], c[1], strings.NewReader("x")); !errors.Is(err, ErrBadName) {
			t.Errorf("Accept(%q,%q) = %v, want ErrBadName", c[0], c[1], err)
		}
	}
}

func TestOversizedUploadKeepsThePreviousFile(t *testing.T) {
	d := turnFor(t, "wiki")
	d.Store.MaxBytes = 8
	if err := d.Accept("wiki", "d", strings.NewReader("good")); err != nil {
		t.Fatal(err)
	}
	if err := d.Accept("wiki", "d", strings.NewReader("0123456789")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	if b, _ := os.ReadFile(staged(d, "wiki", "d")); string(b) != "good" {
		t.Fatalf("the previous upload was replaced by a failed one: %q", b)
	}
	ents, _ := os.ReadDir(filepath.Join(d.cur.dir, "tmp"))
	if len(ents) != 0 {
		t.Fatalf("temporary files left behind: %v", ents)
	}
}

func TestAcceptRefusesASymlinkedModuleDir(t *testing.T) {
	d := turnFor(t, "wiki")
	if err := os.Symlink(t.TempDir(), filepath.Join(d.cur.dir, "modules", "wiki")); err != nil {
		t.Fatal(err)
	}
	if err := d.Accept("wiki", "d", strings.NewReader("x")); err == nil {
		t.Fatal("a symlinked module directory must be refused: the host script runs as root")
	}
}

func TestOpenReadsPublishedDumps(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if _, err := s.Open("wiki", "nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
	dir := filepath.Join(s.Dir, "modules", "wiki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "d"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := s.Open("wiki", "d")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if b, _ := io.ReadAll(f); string(b) != "x" {
		t.Fatalf("%q", b)
	}
}
