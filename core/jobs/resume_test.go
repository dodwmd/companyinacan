package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSniffResume(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		ok   bool
		ct   string
	}{
		{"cv.pdf", []byte("%PDF-1.7\n..."), true, "application/pdf"},
		{"cv.docx", []byte("PK\x03\x04rest"), true, "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"cv.odt", []byte("PK\x03\x04rest"), true, "application/vnd.oasis.opendocument.text"},
		{"cv.txt", []byte("plain text résumé"), true, "text/plain; charset=utf-8"},
		{"cv.rtf", []byte(`{\rtf1\ansi`), true, "application/rtf"},
		{"cv.pdf", []byte("PK\x03\x04"), false, ""},
		{"cv.exe", []byte("MZ\x90\x00"), false, ""},
		{"cv.pdf", []byte("<html><script>"), false, ""},
		{"cv.txt", []byte("hello\x00world"), false, ""},
		{"cv.PDF", []byte("%PDF-1.4"), true, "application/pdf"},
		{"cv", []byte("%PDF-1.4"), false, ""},
		{"cv.pdf.exe", []byte("%PDF-1.4"), false, ""},
		{"", []byte("%PDF-1.4"), false, ""},
	}
	for _, c := range cases {
		ct, err := SniffResume(c.name, c.head)
		if (err == nil) != c.ok || (c.ok && ct != c.ct) {
			t.Errorf("%s %q: ct=%q err=%v, want ok=%v ct=%q", c.name, c.head, ct, err, c.ok, c.ct)
		}
	}
}

func TestSafeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"../../etc/passwd.pdf":            "passwd.pdf",
		`C:\Users\x\cv.pdf`:               "cv.pdf",
		"my cv\r\n(1).pdf":                "my cv (1).pdf",
		strings.Repeat("a", 300) + ".pdf": strings.Repeat("a", 96) + ".pdf",
	} {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFSResumeStore(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFSResumeStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key, n, sum, err := st.Put(ctx, bytes.NewReader([]byte("%PDF-1.7 hello")), 1<<20)
	if err != nil || n != 14 || len(sum) != 64 {
		t.Fatalf("put: %q %d %q %v", key, n, sum, err)
	}
	if strings.ContainsAny(key, `/\.`) {
		t.Errorf("key %q must be an opaque name", key)
	}
	fi, err := os.Stat(filepath.Join(dir, key))
	if err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("stored file mode: %v %v", fi, err)
	}
	rc, err := st.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "%PDF-1.7 hello" {
		t.Errorf("read back %q", b)
	}
	if _, err := st.Open(ctx, "../etc/passwd"); err == nil {
		t.Error("path traversal key accepted")
	}
	if _, _, _, err := st.Put(ctx, bytes.NewReader(bytes.Repeat([]byte("a"), 100)), 50); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize = %v", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("%d files left, want 1 (the oversize upload must be removed)", len(ents))
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Errorf("delete must be idempotent: %v", err)
	}
	if _, err := st.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("open after delete = %v", err)
	}
}

func TestFakeScanner(t *testing.T) {
	s := &FakeScanner{InfectedMarker: "EICAR"}
	if v, err := s.Scan(context.Background(), "a.txt", strings.NewReader("clean")); err != nil || v != ScanClean {
		t.Errorf("clean = %v %v", v, err)
	}
	if v, _ := s.Scan(context.Background(), "a.txt", strings.NewReader("xx EICAR xx")); v != ScanInfected {
		t.Errorf("infected = %v", v)
	}
	var n NoopScanner
	if v, _ := n.Scan(context.Background(), "a.txt", strings.NewReader("EICAR")); v != ScanSkipped {
		t.Errorf("noop scanner must report skipped, got %v", v)
	}
}
