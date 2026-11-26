package modbackup

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// dumpAPI is a stand-in for can-core's dump store: like it, it keeps an upload only if the body
// arrived whole.
type dumpAPI struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newDumpAPI(t *testing.T) (*dumpAPI, *client) {
	t.Helper()
	a := &dumpAPI{files: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Base(r.URL.Path)
		if r.Method == http.MethodPut {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			a.mu.Lock()
			a.files[name] = b
			a.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		a.mu.Lock()
		b, ok := a.files[name]
		a.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newClient(func(k string) string {
		return map[string]string{"CAN_API_URL": srv.URL, "CAN_MODULE": "support", "CAN_TOKEN_FILE": token}[k]
	})
	c.backoff = time.Millisecond
	return a, c
}

func write(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func TestWrappedFilesAndPostgresRoundTrip(t *testing.T) {
	ctx := context.Background()
	api, c := newDumpAPI(t)
	data, work := t.TempDir(), t.TempDir()
	write(t, filepath.Join(data, "attachment", "a b.txt"), "hello", 0o640)
	write(t, filepath.Join(data, "big"), strings.Repeat("x", 3<<20), 0o600)
	if err := os.Symlink("/etc/passwd", filepath.Join(data, "link")); err != nil {
		t.Fatal(err)
	}
	pg := filepath.Join(work, PGDumpName)
	write(t, pg, "PGDMP rows", 0o600)
	if err := backupWrapped(ctx, Wrapped{DataDir: data, Files: true, PGDump: pg, WorkDir: work}, c, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{ManifestName, PGDumpName, FilesName} {
		if len(api.files[n]) == 0 {
			t.Fatalf("%s was not uploaded", n)
		}
	}
	if bytes.Contains(api.files[FilesName], []byte("root:")) {
		t.Fatal("a link out of the volume was followed")
	}

	// A fresh volume that already has something else: replace-all.
	into, work2 := t.TempDir(), t.TempDir()
	write(t, filepath.Join(into, "stale"), "old", 0o600)
	pg2 := filepath.Join(work2, PGDumpName)
	w := Wrapped{DataDir: into, Files: true, PGDump: pg2, WorkDir: work2}
	for range 2 { // safe to run again
		if err := restoreWrapped(ctx, w, c, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(into, "attachment", "a b.txt")); err != nil || string(b) != "hello" {
		t.Fatal(string(b), err)
	}
	if fi, err := os.Stat(filepath.Join(into, "attachment", "a b.txt")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Error("mode not kept", err)
	}
	if l, err := os.Readlink(filepath.Join(into, "link")); err != nil || l != "/etc/passwd" {
		t.Error(l, err)
	}
	if _, err := os.Stat(filepath.Join(into, "stale")); !os.IsNotExist(err) {
		t.Error("stale file survived")
	}
	if b, err := os.ReadFile(pg2); err != nil || string(b) != "PGDMP rows" {
		t.Fatal(string(b), err)
	}
	if names, err := topEntries(into); err != nil || len(names) != 3 {
		t.Error(names, err)
	}

	// A damaged part changes nothing.
	write(t, filepath.Join(into, "keep"), "k", 0o600)
	api.files[FilesName] = api.files[FilesName][:100]
	if err := restoreWrapped(ctx, w, c, io.Discard); err == nil {
		t.Fatal("a truncated archive was restored")
	}
	if _, err := os.Stat(filepath.Join(into, "keep")); err != nil {
		t.Error("the volume was changed", err)
	}
}

func TestWrappedBackupRefusesWhatIsNotADump(t *testing.T) {
	api, c := newDumpAPI(t)
	work := t.TempDir()
	pg := filepath.Join(work, PGDumpName)
	write(t, pg, "error: no", 0o600)
	if err := backupWrapped(context.Background(), Wrapped{PGDump: pg, WorkDir: work}, c, io.Discard); err == nil {
		t.Fatal("accepted")
	}
	if len(api.files) != 0 {
		t.Error("something was uploaded")
	}
}

// A file that cannot be read breaks the upload off: nothing half-written is kept.
func TestWrappedUnreadableFileFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	api, c := newDumpAPI(t)
	data := t.TempDir()
	write(t, filepath.Join(data, "a"), strings.Repeat("y", 1<<20), 0o600)
	write(t, filepath.Join(data, "secret"), "s", 0o000)
	err := backupWrapped(context.Background(), Wrapped{DataDir: data, Files: true, WorkDir: t.TempDir()}, c, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
	if len(api.files) != 0 {
		t.Errorf("kept %d file(s)", len(api.files))
	}
}

func TestWrappedSQLite(t *testing.T) {
	ctx := context.Background()
	_, c := newDumpAPI(t)
	data := t.TempDir()
	w := Wrapped{DataDir: data, SQLite: "goatcounter.sqlite3", WorkDir: t.TempDir()}
	if err := backupWrapped(ctx, w, c, io.Discard); err != ErrNoData {
		t.Fatalf("empty volume: %v", err)
	}
	write(t, filepath.Join(data, "other.db"), "x", 0o600)
	if err := backupWrapped(ctx, w, c, io.Discard); err == nil || err == ErrNoData {
		t.Fatalf("a volume without the expected database must fail, got %v", err)
	}
	if err := os.Remove(filepath.Join(data, "other.db")); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(data, w.SQLite)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE paths (path TEXT); INSERT INTO paths VALUES ('/one')`); err != nil {
		t.Fatal(err)
	}
	if err := backupWrapped(ctx, w, c, io.Discard); err != nil {
		t.Fatal(err)
	}
	into := t.TempDir()
	write(t, filepath.Join(into, w.SQLite+"-wal"), "junk", 0o600)
	if err := restoreWrapped(ctx, Wrapped{DataDir: into, SQLite: w.SQLite, WorkDir: t.TempDir()}, c, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := sql.Open("sqlite", "file:"+filepath.Join(into, w.SQLite))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.Close() }()
	var p string
	if err := got.QueryRow(`SELECT path FROM paths`).Scan(&p); err != nil || p != "/one" {
		t.Fatal(p, err)
	}
}
