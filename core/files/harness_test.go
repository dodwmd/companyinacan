package files

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKeyring = "k1 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n"

// env is a Store over a temporary directory with a controllable clock.
type env struct {
	t       *testing.T
	s       *Store
	be      *LocalBackend
	dir     string
	dbPath  string
	mu      sync.Mutex
	clock   time.Time
	alice   *Principal
	bob     *Principal
	carol   *Principal
	root    *Principal // files.admin
	svc     *Principal
	keyring *Keyring
}

func (e *env) now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.clock
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.clock = e.clock.Add(d)
	e.mu.Unlock()
}

func newEnv(t *testing.T, mods ...func(*Config, *StoreOptions)) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	f := false
	cfg.Scanner.Required = &f
	e := &env{t: t, dir: dir, clock: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		alice: &Principal{Subject: "user:alice", Perms: []string{PermWrite}},
		bob:   &Principal{Subject: "user:bob", Perms: []string{PermWrite}},
		carol: &Principal{Subject: "user:carol", Perms: []string{PermWrite}},
		root:  &Principal{Subject: "user:root", Perms: []string{PermAdmin}},
		svc:   &Principal{Subject: "svc:static", Perms: []string{PermAdmin}}}
	kr, err := ParseKeyring(testKeyring)
	if err != nil {
		t.Fatal(err)
	}
	e.keyring = kr
	be, err := NewLocalBackend(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	e.be = be
	opt := StoreOptions{Keyring: kr, Backend: be, Scanner: &FakeScanner{InfectedMarker: "EICAR-TEST"}, LinkSecret: []byte(strings.Repeat("L", 40)), Config: cfg, Now: e.now, EventsOn: true}
	for _, m := range mods {
		m(cfg, &opt)
	}
	e.dbPath = filepath.Join(dir, "files.db")
	s, err := OpenStore(e.dbPath, opt)
	if err != nil {
		t.Fatal(err)
	}
	s.stageDir = filepath.Join(dir, "staging")
	if err := os.MkdirAll(s.stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	e.s = s
	return e
}

func pdf(extra string) []byte { return []byte("%PDF-1.7\n" + extra) }

func (e *env) space(p *Principal, name string) *Space {
	e.t.Helper()
	sp, err := e.s.CreateSpace(context.Background(), p, name)
	if err != nil {
		e.t.Fatalf("create space: %v", err)
	}
	return sp
}

func (e *env) upload(p *Principal, space, name string, content []byte) *FileView {
	e.t.Helper()
	f, err := e.s.Upload(context.Background(), p, UploadRequest{SpaceID: space, Name: name, Body: bytes.NewReader(content)})
	if err != nil {
		e.t.Fatalf("upload %s: %v", name, err)
	}
	return f
}

func (e *env) member(p *Principal, space, kind, id, role string) {
	e.t.Helper()
	if err := e.s.SetMember(context.Background(), p, space, Member{Kind: kind, ID: id, Role: role}); err != nil {
		e.t.Fatalf("set member: %v", err)
	}
}

// blobs lists the stored blob files (not the temp area).
func (e *env) blobs() []string {
	var out []string
	_ = filepath.WalkDir(filepath.Join(e.dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && !strings.Contains(p, string(filepath.Separator)+".tmp"+string(filepath.Separator)) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func (e *env) staged() int {
	ents, _ := os.ReadDir(e.s.stageDir)
	return len(ents)
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.s.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) readAll(c *Content) []byte {
	e.t.Helper()
	defer func() { _ = c.Body.Close() }()
	var b bytes.Buffer
	if err := c.Stream(context.Background(), &b); err != nil {
		e.t.Fatalf("stream: %v", err)
	}
	return b.Bytes()
}

// rawFiles reads the database file and its write-ahead log.
func (e *env) rawDB() []byte {
	var all []byte
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(e.dbPath + suffix)
		if err == nil {
			all = append(all, b...)
		}
	}
	return all
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

var _ = io.EOF
