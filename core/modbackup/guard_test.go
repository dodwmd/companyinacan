package modbackup_test

import (
	"bufio"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/files"
	"github.com/dodwmd/companyinacan/core/forms"
	"github.com/dodwmd/companyinacan/core/jobs"
	"github.com/dodwmd/companyinacan/core/modbackup"
)

// TestHoldDatabaseOpen is not a test: run as a child process, it is a service that has its
// database open until its stdin closes.
func TestHoldDatabaseOpen(t *testing.T) {
	path := os.Getenv("MODBACKUP_HOLD")
	if path == "" {
		t.Skip("helper process")
	}
	s, err := forms.OpenStore(path)
	check(t, err)
	_, _ = os.Stdout.WriteString("holding\n")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	check(t, s.Insert(context.Background(), &forms.Lead{FormID: "contact", Email: "late@example.com", RetentionUntil: time.Now().Add(time.Hour)}))
	check(t, s.Close())
}

func count(t *testing.T, path, where string) (n int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	check(t, err)
	defer func() { _ = db.Close() }()
	check(t, db.QueryRow(`SELECT COUNT(*) FROM leads WHERE email LIKE ?`, where).Scan(&n))
	return n
}

// A database another process has open is not replaced: what it writes afterwards would be lost
// without a word. Once it has stopped, the restore works.
func TestRestoreRefusesADatabaseInUse(t *testing.T) {
	src := newInstance(t, "forms.db")
	s, err := forms.OpenStore(src.db)
	check(t, err)
	check(t, s.Close())
	dst, set := roundTrip(t, "can-forms", src)
	restore := []string{"restore", "-config", dst.cfg, "-from", set, "-replace"}
	refused := func(how string) {
		t.Helper()
		code, out := run(t, "can-forms", nil, restore...)
		if code != 1 || !strings.Contains(out, "in use") {
			t.Fatalf("%s: exit %d: %s", how, code, out)
		}
	}

	// in this process, idle
	held, err := forms.OpenStore(dst.db)
	check(t, err)
	refused("an idle service")
	check(t, held.Close())

	// in another process
	cmd := exec.Command(os.Args[0], "-test.run=^TestHoldDatabaseOpen$") //nolint:gosec // this test binary
	cmd.Env = append(os.Environ(), "MODBACKUP_HOLD="+dst.db)
	stdin, err := cmd.StdinPipe()
	check(t, err)
	stdout, err := cmd.StdoutPipe()
	check(t, err)
	check(t, cmd.Start())
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "holding\n" {
		t.Fatalf("helper: %q %v", line, err)
	}
	refused("another process")
	_, _ = stdin.Write([]byte("\n"))
	check(t, cmd.Wait())
	if count(t, dst.db, "late@%") != 1 {
		t.Fatal("the write made after the refused restore is gone")
	}

	must(t, "can-forms", restore...)
	if count(t, dst.db, "late@%") != 0 {
		t.Fatal("the restore did not replace the database")
	}
	// the service opens the restored database again (it goes back to WAL by itself)
	again, err := forms.OpenStore(dst.db)
	check(t, err)
	check(t, again.Close())
	// a corrupt database is nothing to protect
	check(t, os.WriteFile(dst.db, []byte(strings.Repeat("garbage ", 100)), 0o600))
	must(t, "can-forms", restore...)
}

func TestRestoreRefusesANewerSchema(t *testing.T) {
	src := newInstance(t, "forms.db")
	s, err := forms.OpenStore(src.db)
	check(t, err)
	check(t, s.Close())
	set := t.TempDir()
	must(t, "can-forms", "backup", "-config", src.cfg, "-out", set)
	man, err := os.ReadFile(filepath.Join(set, modbackup.ManifestName))
	check(t, err)
	if !regexp.MustCompile(`"schema_version": 5`).Match(man) {
		t.Fatalf("the manifest records the schema version: %s", man)
	}
	db, err := sql.Open("sqlite", "file:"+src.db)
	check(t, err)
	_, err = db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (999, 1)`)
	check(t, err)
	check(t, db.Close())
	newer := t.TempDir()
	must(t, "can-forms", "backup", "-config", src.cfg, "-out", newer)
	dst := newInstance(t, "forms.db")
	code, out := run(t, "can-forms", nil, "restore", "-config", dst.cfg, "-from", newer)
	if code != 1 || !strings.Contains(out, "schema version 999") {
		t.Fatalf("exit %d: %s", code, out)
	}
	// also when the manifest does not say (a set made before it was recorded)
	man, err = os.ReadFile(filepath.Join(newer, modbackup.ManifestName))
	check(t, err)
	check(t, os.WriteFile(filepath.Join(newer, modbackup.ManifestName), []byte(strings.Replace(string(man), `"schema_version": 999`, `"schema_version": 0`, 1)), 0o600))
	if code, out = run(t, "can-forms", nil, "restore", "-config", dst.cfg, "-from", newer); code != 1 || !strings.Contains(out, "999") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(dst.db); !os.IsNotExist(err) {
		t.Fatal("a refused restore left a database")
	}
}

// An empty or schemaless file is not a database of the service: dumping it would replace a good
// dump with nothing.
func TestBackupRefusesAnUninitialisedDatabase(t *testing.T) {
	for name, make := range map[string]func(string){
		"zero bytes": func(p string) { check(t, os.WriteFile(p, nil, 0o600)) },
		"no schema": func(p string) {
			db, err := sql.Open("sqlite", "file:"+p)
			check(t, err)
			_, err = db.Exec(`CREATE TABLE something (x)`)
			check(t, err)
			check(t, db.Close())
		},
		"no migration applied": func(p string) {
			db, err := sql.Open("sqlite", "file:"+p)
			check(t, err)
			_, err = db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`)
			check(t, err)
			check(t, db.Close())
		},
	} {
		i := newInstance(t, "forms.db")
		make(i.db)
		out := t.TempDir()
		code, msg := run(t, "can-forms", nil, "backup", "-config", i.cfg, "-out", out)
		if code != modbackup.ExitFail || !strings.Contains(msg, "not an initialised database") {
			t.Errorf("%s: exit %d: %s", name, code, msg)
		}
		if _, err := os.Stat(filepath.Join(out, modbackup.ManifestName)); err == nil {
			t.Errorf("%s: a set was written", name)
		}
	}
}

// The error names the record and the purge that clears it, and following it makes the backup work
// again, also after a restore with -allow-missing-blobs.
func TestMissingFileGuidanceUnblocksTheBackup(t *testing.T) {
	defer modbackup.SetRetryPause(0)()
	f := newFiles(t)
	set := t.TempDir()
	must(t, "can-files", "backup", "-config", f.cfg, "-out", set)
	dst := newInstance(t, "files.db")
	out := must(t, "can-files", "restore", "-config", dst.cfg, "-from", set, "-allow-missing-blobs")
	for _, want := range []string{"WARNING", "FAILS until", f.id, "Purge the record", "/v1/purge"} {
		if !strings.Contains(out, want) {
			t.Errorf("restore output lacks %q: %s", want, out)
		}
	}
	code, msg := run(t, "can-files", nil, "backup", "-config", dst.cfg, "-out", t.TempDir())
	if code != 1 || !strings.Contains(msg, f.id) || !strings.Contains(msg, "file-content") || !strings.Contains(msg, "deleting it is not enough") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if n := len(strings.TrimSpace(msg)); n > 400 {
		t.Errorf("only the last 400 characters of a hook's log reach can-core; the message has %d", n)
	}
	s, err := filesStore(dst, keyring)
	check(t, err)
	defer func() { _ = s.Close() }()
	check(t, s.PurgeSubject(context.Background(), &files.Principal{Subject: "svc:t", Perms: []string{files.PermAdmin}}, f.id, "file-content"))
	must(t, "can-files", "backup", "-config", dst.cfg, "-out", t.TempDir())
}

// The same for a resume: the record to purge is the candidate.
func TestMissingResumeNamesTheCandidate(t *testing.T) {
	defer modbackup.SetRetryPause(0)()
	src := newInstance(t, "jobs.db")
	kr, err := jobs.ParseKeyring(keyring)
	check(t, err)
	s, err := jobs.OpenStore(src.db, jobs.StoreOptions{Keyring: kr, HashKey: []byte(strings.Repeat("h", 40))})
	check(t, err)
	check(t, s.Close())
	db, err := sql.Open("sqlite", "file:"+src.db)
	check(t, err)
	_, err = db.Exec(`INSERT INTO applications (id,candidate_id,job_id,stage,status,resume_key,resume_size,resume_sha256,created_at,updated_at)
VALUES ('a','cand_42','j','new','active',?,8,'x',1,1)`, strings.Repeat("ab", 16))
	check(t, err)
	check(t, db.Close())
	code, msg := run(t, "can-jobs", nil, "backup", "-config", src.cfg, "-out", t.TempDir())
	if code != 1 || !strings.Contains(msg, "candidate cand_42") || !strings.Contains(msg, `"class":"candidate-resumes"`) {
		t.Fatalf("exit %d: %s", code, msg)
	}
}
