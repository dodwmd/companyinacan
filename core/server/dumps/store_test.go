package dumps

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutThenOpenRoundTrips(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "dumps")}
	if err := s.Put("wiki", "storage.tar.gz", strings.NewReader("payload")); err != nil {
		t.Fatal(err)
	}
	f, err := s.Open("wiki", "storage.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(f)
	if string(b) != "payload" {
		t.Fatalf("got %q", b)
	}
	fi, _ := os.Stat(filepath.Join(s.Dir, "modules", "wiki", "storage.tar.gz"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestPutRejectsBadNamesAndTraversal(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	for _, c := range [][2]string{{"..", "x"}, {"wiki", ".."}, {"wiki", "a/b"}, {"wiki", ".hidden"}, {"Wiki", "x"}, {"wiki", ""}, {"", "x"}, {"wiki", COMPLETE}, {"wiki", strings.Repeat("a", 200)}} {
		if err := s.Put(c[0], c[1], strings.NewReader("x")); !errors.Is(err, ErrBadName) {
			t.Errorf("Put(%q,%q) = %v, want ErrBadName", c[0], c[1], err)
		}
	}
}

func TestPutLeavesOldDumpWhenBodyTooLarge(t *testing.T) {
	s := &Store{Dir: t.TempDir(), MaxBytes: 8}
	if err := s.Put("wiki", "d", strings.NewReader("good")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("wiki", "d", strings.NewReader("0123456789")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	f, _ := s.Open("wiki", "d")
	b, _ := io.ReadAll(f)
	_ = f.Close()
	if string(b) != "good" {
		t.Fatalf("the previous dump was replaced by a failed upload: %q", b)
	}
	ents, _ := os.ReadDir(filepath.Join(s.Dir, "modules", "wiki"))
	if len(ents) != 1 {
		t.Fatalf("temporary files left behind: %v", ents)
	}
}

func TestPutRefusesSymlinkedModuleDir(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(s.Dir, "modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(s.Dir, "modules", "wiki")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("wiki", "d", strings.NewReader("x")); err == nil {
		t.Fatal("a symlinked module directory must be refused: the host script runs as root")
	}
}

func TestOpenMissingIsNotFound(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if _, err := s.Open("wiki", "nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v", err)
	}
}
