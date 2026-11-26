package modbackup_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/board"
	"github.com/dodwmd/companyinacan/core/equity"
	"github.com/dodwmd/companyinacan/core/files"
	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/forms"
	"github.com/dodwmd/companyinacan/core/jobs"
	"github.com/dodwmd/companyinacan/core/modbackup"
	"github.com/dodwmd/companyinacan/core/people"
	"github.com/dodwmd/companyinacan/core/provision"
	"github.com/dodwmd/companyinacan/core/work"
)

var keyring = "k1 " + strings.Repeat("0123456789abcdef", 4) + "\n"

var migrations = map[string]func() fs.FS{
	"can-forms": forms.Migrations, "can-work": work.Migrations, "can-people": people.Migrations, "can-finance": finance.Migrations,
	"can-equity": equity.Migrations, "can-board": board.Migrations, "can-provision": provision.Migrations, "can-jobs": jobs.Migrations,
	"can-files": files.Migrations,
}

// instance is one service's data directory and a config file pointing at it.
type instance struct {
	dir, cfg, db string
}

func newInstance(t *testing.T, dbName string) instance {
	t.Helper()
	dir := t.TempDir()
	i := instance{dir: dir, cfg: filepath.Join(dir, "config.yaml"), db: filepath.Join(dir, dbName)}
	body := "db_path: " + i.db + "\n"
	switch dbName {
	case "files.db":
		body += "backend:\n  local: { dir: " + filepath.Join(dir, "blobs") + " }\n"
	case "jobs.db":
		body += "resume_dir: " + filepath.Join(dir, "resumes") + "\n"
	}
	if err := os.WriteFile(i.cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return i
}

func run(t *testing.T, binary string, env map[string]string, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := modbackup.Main(binary, migrations[binary](), args, func(k string) string { return env[k] }, &out, &out)
	return code, out.String()
}

func must(t *testing.T, binary string, args ...string) string {
	t.Helper()
	code, out := run(t, binary, nil, args...)
	if code != 0 {
		t.Fatalf("%s %v: exit %d: %s", binary, args, code, out)
	}
	return out
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// roundTrip backs an instance up into a directory and restores it into a fresh one.
func roundTrip(t *testing.T, binary string, src instance) (instance, string) {
	t.Helper()
	set := t.TempDir()
	must(t, binary, "backup", "-config", src.cfg, "-out", set)
	dst := newInstance(t, filepath.Base(src.db))
	args := []string{"restore", "-config", dst.cfg, "-from", set}
	switch filepath.Base(src.db) {
	case "files.db":
		args = append(args, "-blobs", filepath.Join(src.dir, "blobs"))
	case "jobs.db":
		args = append(args, "-blobs", filepath.Join(src.dir, "resumes"))
	}
	must(t, binary, args...)
	return dst, set
}

func filesStore(i instance, ring string) (*files.Store, error) {
	k, err := files.ParseKeyring(ring)
	if err != nil {
		return nil, err
	}
	be, err := files.NewLocalBackend(filepath.Join(i.dir, "blobs"))
	if err != nil {
		return nil, err
	}
	cfg := files.DefaultConfig()
	no := false
	cfg.Scanner.Required = &no
	s, err := files.OpenStore(i.db, files.StoreOptions{Keyring: k, Backend: be, LinkSecret: []byte(strings.Repeat("L", 40)), Config: cfg})
	if err != nil {
		return nil, err
	}
	return s, s.SetStaging(filepath.Join(i.dir, "staging"))
}

// Every in-house service's database, as its own store creates it, survives dump and restore while
// the service holds it open, and the restored file opens with that store again.
func TestEveryServiceRoundTrips(t *testing.T) {
	for _, c := range []struct {
		binary, db string
		open       func(instance) (io.Closer, error)
	}{
		{"can-forms", "forms.db", func(i instance) (io.Closer, error) { return forms.OpenStore(i.db) }},
		{"can-work", "work.db", func(i instance) (io.Closer, error) { return work.OpenStore(i.db) }},
		{"can-finance", "finance.db", func(i instance) (io.Closer, error) { return finance.OpenStore(i.db) }},
		{"can-provision", "provision.db", func(i instance) (io.Closer, error) { return provision.OpenStore(i.db, nil) }},
		{"can-people", "people.db", func(i instance) (io.Closer, error) {
			kr, err := people.ParseKeyring(keyring)
			if err != nil {
				return nil, err
			}
			return people.Open(i.db, people.Options{Keyring: kr})
		}},
		{"can-equity", "equity.db", func(i instance) (io.Closer, error) {
			kr, err := equity.ParseKeyring(keyring)
			if err != nil {
				return nil, err
			}
			return equity.Open(i.db, equity.Options{Keyring: kr})
		}},
		{"can-board", "board.db", func(i instance) (io.Closer, error) {
			kr, err := board.ParseKeyring(keyring)
			if err != nil {
				return nil, err
			}
			return board.Open(i.db, board.Options{Keyring: kr})
		}},
		{"can-jobs", "jobs.db", func(i instance) (io.Closer, error) {
			kr, err := jobs.ParseKeyring(keyring)
			if err != nil {
				return nil, err
			}
			return jobs.OpenStore(i.db, jobs.StoreOptions{Keyring: kr, HashKey: []byte(strings.Repeat("h", 40))})
		}},
		{"can-files", "files.db", func(i instance) (io.Closer, error) { return filesStore(i, keyring) }},
	} {
		t.Run(c.binary, func(t *testing.T) {
			src := newInstance(t, c.db)
			s, err := c.open(src)
			check(t, err)
			defer func() { _ = s.Close() }()
			dst, _ := roundTrip(t, c.binary, src)
			r, err := c.open(dst)
			check(t, err)
			check(t, r.Close())
		})
	}
}

// The dump of a database being written to is one point in time: forms keeps inserting, the copy
// passes integrity_check and holds everything committed before the dump began.
func TestFormsDumpWhileWriting(t *testing.T) {
	ctx := context.Background()
	src := newInstance(t, "forms.db")
	s, err := forms.OpenStore(src.db)
	check(t, err)
	defer func() { _ = s.Close() }()
	insert := func(n string) error {
		return s.Insert(ctx, &forms.Lead{FormID: "contact", Email: n + "@example.com", Name: strings.Repeat("x", 500), RetentionUntil: time.Now().Add(time.Hour)})
	}
	for range 200 {
		check(t, insert("before"))
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	var werr error
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if werr = insert("during"); werr != nil {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	dst, _ := roundTrip(t, "can-forms", src)
	close(stop)
	wg.Wait()
	check(t, werr) // the dump never blocked the service's writer into an error
	db, err := sql.Open("sqlite", "file:"+dst.db)
	check(t, err)
	defer func() { _ = db.Close() }()
	var before int
	var res string
	check(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM leads WHERE email LIKE 'before%'`).Scan(&before))
	check(t, db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res))
	if before != 200 || res != "ok" {
		t.Fatalf("restored %d of 200 committed leads, integrity %q", before, res)
	}
}

// A stopped (disabled) service leaves no -wal or -shm behind; the dump still works and the
// content comes back.
func TestWorkContentRestored(t *testing.T) {
	ctx := context.Background()
	src := newInstance(t, "work.db")
	s, err := work.OpenStore(src.db)
	check(t, err)
	admin := &work.Principal{Subject: "svc:test", Kind: "static", Permissions: []string{work.PermAdmin}}
	_, err = s.CreateProject(ctx, admin, work.NewProject{Key: "BK", Name: "Backup project"})
	check(t, err)
	check(t, s.Close())
	dst, _ := roundTrip(t, "can-work", src)
	r, err := work.OpenStore(dst.db)
	check(t, err)
	defer func() { _ = r.Close() }()
	p, err := r.GetProject(ctx, admin, "BK")
	check(t, err)
	if p.Name != "Backup project" {
		t.Fatalf("name %q", p.Name)
	}
}

type filesFixture struct {
	instance
	id, body string
}

var alice = &files.Principal{Subject: "user:alice", Perms: []string{files.PermWrite}}

func newFiles(t *testing.T) filesFixture {
	t.Helper()
	f := filesFixture{instance: newInstance(t, "files.db"), body: "quarterly numbers, not for a database\n"}
	s, err := filesStore(f.instance, keyring)
	check(t, err)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	sp, err := s.CreateSpace(ctx, alice, "Finance")
	check(t, err)
	v, err := s.Upload(ctx, alice, files.UploadRequest{SpaceID: sp.ID, Name: "q3.txt", Body: strings.NewReader(f.body), Size: int64(len(f.body))})
	check(t, err)
	f.id = v.ID
	return f
}

func blobFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	check(t, filepath.WalkDir(filepath.Join(dir, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && len(d.Name()) == 32 {
			out = append(out, p)
		}
		return err
	}))
	return out
}

// The files dump and the blobs the volume backup holds agree: the restored instance serves the
// same bytes (the service verifies the checksum on download) and reads the sealed name.
func TestFilesContentRestored(t *testing.T) {
	f := newFiles(t)
	dst, _ := roundTrip(t, "can-files", f.instance)
	r, err := filesStore(dst, keyring)
	check(t, err)
	defer func() { _ = r.Close() }()
	c, err := r.OpenContent(context.Background(), alice, f.id, 0)
	check(t, err)
	got, err := io.ReadAll(c.Body)
	check(t, err)
	check(t, c.Body.Close())
	if string(got) != f.body {
		t.Fatalf("content %q", got)
	}
}

func TestFilesBlobsMustMatchTheDatabase(t *testing.T) {
	f := newFiles(t)
	set := t.TempDir()
	must(t, "can-files", "backup", "-config", f.cfg, "-out", set)

	// Restore without the bytes: refused, and no database is installed.
	dst := newInstance(t, "files.db")
	code, out := run(t, "can-files", nil, "restore", "-config", dst.cfg, "-from", set)
	if code != 1 || !strings.Contains(out, "missing or damaged") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(dst.db); !os.IsNotExist(err) {
		t.Fatalf("a refused restore left a database behind: %v", err)
	}
	// Damaged bytes of the right size are not accepted either.
	blobs := blobFiles(t, f.dir)
	if len(blobs) != 1 {
		t.Fatalf("blobs: %v", blobs)
	}
	check(t, os.WriteFile(blobs[0], bytes.Repeat([]byte("x"), len(f.body)), 0o600))
	code, out = run(t, "can-files", nil, "restore", "-config", dst.cfg, "-from", set, "-blobs", filepath.Join(f.dir, "blobs"))
	if code != 1 {
		t.Fatalf("damaged blob accepted: %d %s", code, out)
	}
	out = must(t, "can-files", "restore", "-config", dst.cfg, "-from", set, "-allow-missing-blobs")
	if !strings.Contains(out, "1 missing") {
		t.Fatal(out)
	}

	// A file that has vanished from a live instance fails the backup instead of dumping a
	// database that refers to it.
	check(t, os.Remove(blobs[0]))
	defer modbackup.SetRetryPause(0)()
	code, out = run(t, "can-files", nil, "backup", "-config", f.cfg, "-out", t.TempDir())
	if code != 1 || !strings.Contains(out, "missing") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestJobsResumesAreListedAndVerified(t *testing.T) {
	src := newInstance(t, "jobs.db")
	kr, err := jobs.ParseKeyring(keyring)
	check(t, err)
	s, err := jobs.OpenStore(src.db, jobs.StoreOptions{Keyring: kr, HashKey: []byte(strings.Repeat("h", 40))})
	check(t, err)
	check(t, s.Close())
	res, err := jobs.NewFSResumeStore(filepath.Join(src.dir, "resumes"))
	check(t, err)
	key, n, sha, err := res.Put(context.Background(), strings.NewReader("a resume"), 100)
	check(t, err)
	db, err := sql.Open("sqlite", "file:"+src.db) // foreign keys are off by default: no job or candidate needed
	check(t, err)
	_, err = db.Exec(`INSERT INTO applications (id,candidate_id,job_id,stage,status,resume_key,resume_size,resume_sha256,created_at,updated_at)
VALUES ('a','c','j','new','active',?,?,?,1,1)`, key, n, sha)
	check(t, err)
	check(t, db.Close())
	dst, _ := roundTrip(t, "can-jobs", src)
	got, err := os.ReadFile(filepath.Join(dst.dir, "resumes", key)) //nolint:gosec // test file
	check(t, err)
	if string(got) != "a resume" {
		t.Fatalf("resume %q", got)
	}
}

func TestNoDatabaseIsItsOwnExitCode(t *testing.T) {
	i := newInstance(t, "forms.db")
	code, out := run(t, "can-forms", nil, "backup", "-config", i.cfg, "-out", t.TempDir())
	if code != modbackup.ExitNoData {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(i.db); !os.IsNotExist(err) {
		t.Fatal("backup must not create a database")
	}
}

func TestRestoreIsGuarded(t *testing.T) {
	src := newInstance(t, "forms.db")
	s, err := forms.OpenStore(src.db)
	check(t, err)
	defer func() { _ = s.Close() }()
	dst, set := roundTrip(t, "can-forms", src)

	if code, out := run(t, "can-forms", nil, "restore", "-config", dst.cfg, "-from", set); code != 1 || !strings.Contains(out, "-replace") {
		t.Fatalf("an existing database must need -replace: %d %s", code, out)
	}
	// stale journal files of the replaced database are removed
	check(t, os.WriteFile(dst.db+"-wal", []byte("stale"), 0o600))
	must(t, "can-forms", "restore", "-config", dst.cfg, "-from", set, "-replace")
	if _, err := os.Stat(dst.db + "-wal"); !os.IsNotExist(err) {
		t.Fatal("stale -wal left")
	}
	if code, out := run(t, "can-work", nil, "restore", "-config", newInstance(t, "work.db").cfg, "-from", set); code != 1 || !strings.Contains(out, "can-forms") {
		t.Fatalf("another service's dump: %d %s", code, out)
	}
	check(t, os.Truncate(filepath.Join(set, modbackup.DatabaseName), 20))
	fresh := newInstance(t, "forms.db")
	if code, out := run(t, "can-forms", nil, "restore", "-config", fresh.cfg, "-from", set); code != 1 {
		t.Fatalf("a damaged dump: %d %s", code, out)
	}
	left, err := os.ReadDir(fresh.dir)
	check(t, err)
	if len(left) != 1 { // the config file
		t.Fatalf("a failed restore left files: %v", left)
	}
}

// Upload and download through the dump API: bearer token, retries, the manifest last.
func TestDumpAPITransport(t *testing.T) {
	var mu sync.Mutex
	stored := map[string][]byte{}
	var order []string
	failed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		name, ok := strings.CutPrefix(r.URL.Path, "/v1/dumps/forms/")
		if r.Header.Get("Authorization") != "Bearer tok" || !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			if !failed {
				failed = true
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			stored[name], order = b, append(order, name)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			_, _ = w.Write(stored[name])
		}
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	check(t, os.WriteFile(tok, []byte("tok\n"), 0o600))
	env := map[string]string{"CAN_API_URL": srv.URL, "CAN_MODULE": "forms", "CAN_TOKEN_FILE": tok}
	src := newInstance(t, "forms.db")
	s, err := forms.OpenStore(src.db)
	check(t, err)
	defer func() { _ = s.Close() }()
	if code, out := run(t, "can-forms", env, "backup", "-config", src.cfg); code != 0 {
		t.Fatal(out)
	}
	if strings.Join(order, ",") != modbackup.DatabaseName+","+modbackup.ManifestName {
		t.Fatalf("uploads %v", order)
	}
	dst := newInstance(t, "forms.db")
	if code, out := run(t, "can-forms", env, "restore", "-config", dst.cfg); code != 0 {
		t.Fatal(out)
	}
	r, err := forms.OpenStore(dst.db)
	check(t, err)
	check(t, r.Close())
}
