package modbackup_test

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/board"
	"github.com/dodwmd/companyinacan/core/equity"
	"github.com/dodwmd/companyinacan/core/exportfmt"
	"github.com/dodwmd/companyinacan/core/files"
	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/forms"
	"github.com/dodwmd/companyinacan/core/jobs"
	"github.com/dodwmd/companyinacan/core/modbackup"
	"github.com/dodwmd/companyinacan/core/people"
	"github.com/dodwmd/companyinacan/core/provision"
	"github.com/dodwmd/companyinacan/core/work"
)

var ring2 = "k2 " + strings.Repeat("fedcba9876543210", 4) + "\n"

var serviceDBs = map[string]string{
	"can-forms": "forms.db", "can-work": "work.db", "can-people": "people.db", "can-jobs": "jobs.db", "can-finance": "finance.db",
	"can-equity": "equity.db", "can-board": "board.db", "can-provision": "provision.db", "can-files": "files.db",
}

// create makes the service's database as its own store creates it.
func create(t *testing.T, binary string, i instance) {
	t.Helper()
	opened := func() (io.Closer, error) {
		switch binary {
		case "can-forms":
			return forms.OpenStore(i.db)
		case "can-work":
			return work.OpenStore(i.db)
		case "can-finance":
			return finance.OpenStore(i.db)
		case "can-provision":
			return provision.OpenStore(i.db, nil)
		case "can-files":
			return filesStore(i, keyring)
		case "can-people":
			k, err := people.ParseKeyring(keyring)
			if err != nil {
				return nil, err
			}
			return people.Open(i.db, people.Options{Keyring: k})
		case "can-equity":
			k, err := equity.ParseKeyring(keyring)
			if err != nil {
				return nil, err
			}
			return equity.Open(i.db, equity.Options{Keyring: k})
		case "can-board":
			k, err := board.ParseKeyring(keyring)
			if err != nil {
				return nil, err
			}
			return board.Open(i.db, board.Options{Keyring: k})
		default:
			k, err := jobs.ParseKeyring(keyring)
			if err != nil {
				return nil, err
			}
			return jobs.OpenStore(i.db, jobs.StoreOptions{Keyring: k, HashKey: []byte(strings.Repeat("h", 40))})
		}
	}
	c, err := opened()
	check(t, err)
	check(t, c.Close())
}

func sqlDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	check(t, err)
	db.SetMaxOpenConns(1)
	return db
}

func sqlExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// tarFiles reads a tar stream into memory.
func tarFiles(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		check(t, err)
		b, err := io.ReadAll(tr)
		check(t, err)
		if !exportfmt.ValidName(h.Name) {
			t.Fatalf("unacceptable entry name %q", h.Name)
		}
		out[h.Name] = string(b)
	}
}

func export(t *testing.T, binary string, i instance, env map[string]string, args ...string) map[string]string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "export.tar")
	code, log := run(t, binary, env, append([]string{"export", "-config", i.cfg, "-out", out}, args...)...)
	if code != 0 {
		t.Fatalf("%s export: exit %d: %s", binary, code, log)
	}
	f, err := os.Open(out)
	check(t, err)
	defer func() { _ = f.Close() }()
	return tarFiles(t, f)
}

func moduleManifest(t *testing.T, files map[string]string) modbackup.ModuleExport {
	t.Helper()
	var m modbackup.ModuleExport
	check(t, json.Unmarshal([]byte(files["manifest.json"]), &m))
	return m
}

// A migration that adds a table or a column must say what an export does with it.
func TestExportClassifiesEveryColumn(t *testing.T) {
	for binary, name := range serviceDBs {
		t.Run(binary, func(t *testing.T) {
			i := newInstance(t, name)
			create(t, binary, i)
			files := export(t, binary, i, nil)
			m := moduleManifest(t, files)
			if len(m.Warnings) > 0 {
				t.Fatalf("not classified in core/modbackup/exportspec.go: %v", m.Warnings)
			}
			var s exportfmt.Schema
			check(t, json.Unmarshal([]byte(files["schema.json"]), &s))
			if len(s.Tables) == 0 {
				t.Fatal("no table in schema.json")
			}
			for _, tbl := range s.Tables {
				for _, f := range tbl.Files {
					if _, ok := files[f]; !ok {
						t.Errorf("%s: %s is in schema.json but not in the archive", tbl.Name, f)
					}
				}
				for _, c := range tbl.Columns {
					// Nothing that is named like a credential is ever a column of an export.
					for _, bad := range []string{"secret", "password", "token", "hmac", "private_key"} {
						if c.Name == "secret_ballot" { // a flag on a resolution
							continue
						}
						if strings.Contains(c.Name, bad) {
							t.Errorf("%s.%s looks like key material", tbl.Name, c.Name)
						}
					}
				}
			}
		})
	}
}

type rotator interface {
	io.Closer
	RotateKeys(context.Context) (int, error)
}

// seed inserts one row into every table with sealed columns, ignoring constraints, and puts a
// recognisable sealed value, bound the way the export's table specs say, into each of them.
func seed(t *testing.T, binary string, i instance, seal func(plain, aad string) (string, error)) (plains []string) {
	t.Helper()
	db := sqlDB(t, i.db)
	defer func() { _ = db.Close() }()
	sqlExec(t, db, `PRAGMA foreign_keys = OFF`)
	sqlExec(t, db, `PRAGMA ignore_check_constraints = ON`)
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger'`)
	check(t, err)
	var triggers []string
	for rows.Next() {
		var n string
		check(t, rows.Scan(&n))
		triggers = append(triggers, n)
	}
	check(t, rows.Close())
	for _, n := range triggers {
		sqlExec(t, db, `DROP TRIGGER `+n)
	}
	seeded := map[string]bool{}
	for _, c := range modbackup.SealedColumns(binary) {
		if c.Table == "ballot_staging" {
			continue
		}
		if !seeded[c.Table] {
			insertRow(t, db, c.Table, len(seeded))
			seeded[c.Table] = true
		}
		var id string
		check(t, db.QueryRow(`SELECT `+c.Row+` FROM `+c.Table+` AS t`).Scan(&id))
		plain := "PLAIN-" + c.Table + "-" + c.Column
		v, err := seal(plain, c.Prefix+id+c.Suffix)
		check(t, err)
		sqlExec(t, db, `UPDATE `+c.Table+` SET `+c.Column+` = ?`, v)
		plains = append(plains, plain)
	}
	// Rows the export would otherwise leave out: attributed feedback, a share of a file.
	if binary == "can-people" {
		sqlExec(t, db, `UPDATE review_responses SET reviewer_id = 'reviewer'`)
	}
	if binary == "can-jobs" {
		sqlExec(t, db, `UPDATE applications SET resume_key = ''`)
	}
	if binary == "can-files" {
		sqlExec(t, db, `UPDATE files SET status = 'active'`)
		sqlExec(t, db, `UPDATE shares SET file_id = (SELECT id FROM files)`)
	}
	return plains
}

func insertRow(t *testing.T, db *sql.DB, table string, n int) {
	t.Helper()
	rows, err := db.Query(`SELECT name, type, "notnull", pk FROM pragma_table_info(?)`, table)
	check(t, err)
	var names, marks []string
	var args []any
	for rows.Next() {
		var name, typ string
		var notnull, pk int
		check(t, rows.Scan(&name, &typ, &notnull, &pk))
		if notnull == 0 && pk == 0 {
			continue
		}
		var v any = fmt.Sprintf("x%d-%s", n, name)
		if strings.Contains(strings.ToUpper(typ), "INT") || strings.Contains(strings.ToUpper(typ), "REAL") {
			v = 100 + n
		}
		names, marks, args = append(names, name), append(marks, "?"), append(args, v)
	}
	check(t, rows.Close())
	sqlExec(t, db, `INSERT INTO `+table+` (`+strings.Join(names, ",")+`) VALUES (`+strings.Join(marks, ",")+`)`, args...)
}

// The export opens every sealed value that the service's own key rotation opens, with the same
// binding; without the flag they are only marked.
func TestExportOpensSealedFields(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		binary, db string
		open       func(i instance, ring string) (rotator, error)
	}{
		{"can-people", "people.db", func(i instance, ring string) (rotator, error) {
			kr, err := people.ParseKeyring(ring)
			if err != nil {
				return nil, err
			}
			return people.Open(i.db, people.Options{Keyring: kr})
		}},
		{"can-jobs", "jobs.db", func(i instance, ring string) (rotator, error) {
			kr, err := jobs.ParseKeyring(ring)
			if err != nil {
				return nil, err
			}
			return jobs.OpenStore(i.db, jobs.StoreOptions{Keyring: kr, HashKey: []byte(strings.Repeat("h", 40))})
		}},
		{"can-equity", "equity.db", func(i instance, ring string) (rotator, error) {
			kr, err := equity.ParseKeyring(ring)
			if err != nil {
				return nil, err
			}
			return equity.Open(i.db, equity.Options{Keyring: kr})
		}},
		{"can-board", "board.db", func(i instance, ring string) (rotator, error) {
			kr, err := board.ParseKeyring(ring)
			if err != nil {
				return nil, err
			}
			return board.Open(i.db, board.Options{Keyring: kr})
		}},
		{"can-files", "files.db", func(i instance, ring string) (rotator, error) { return filesStore(i, ring) }},
	} {
		t.Run(c.binary, func(t *testing.T) {
			i := newInstance(t, c.db)
			svc, err := c.open(i, keyring)
			check(t, err)
			check(t, svc.Close())
			// The services share one sealed format.
			keys, err := jobs.ParseKeyring(keyring)
			check(t, err)
			plains := seed(t, c.binary, i, keys.Seal)

			// The service's own rotation opens every value with its own bindings.
			svc, err = c.open(i, ring2+keyring)
			check(t, err)
			n, err := svc.RotateKeys(ctx)
			if err != nil {
				t.Fatalf("the service cannot open what the export's bindings sealed: %v", err)
			}
			check(t, svc.Close())
			if n != len(plains) {
				t.Fatalf("rotation re-sealed %d values, the export knows %d", n, len(plains))
			}
			cfg, err := os.ReadFile(i.cfg)
			check(t, err)
			ringFile := filepath.Join(i.dir, "keyring")
			check(t, os.WriteFile(ringFile, []byte(ring2), 0o600))
			key := "keyring_file"
			if c.binary == "can-jobs" || c.binary == "can-files" {
				key = "keyring_path"
			}
			check(t, os.WriteFile(i.cfg, append(cfg, []byte(key+": "+ringFile+"\n")...), 0o600))

			sealedOnly := export(t, c.binary, i, nil)
			opened := export(t, c.binary, i, nil, "-include-sealed")
			var all, plainAll strings.Builder
			for name, body := range sealedOnly {
				if strings.HasPrefix(name, "records/") && strings.HasSuffix(name, ".jsonl") {
					all.WriteString(body)
				}
			}
			for name, body := range opened {
				if strings.HasPrefix(name, "records/") && strings.HasSuffix(name, ".jsonl") {
					plainAll.WriteString(body)
				}
			}
			for _, p := range plains {
				if strings.Contains(all.String(), p) {
					t.Errorf("%q is in an export without --include-sealed", p)
				}
				if !strings.Contains(plainAll.String(), p) {
					t.Errorf("%q is missing from the export with --include-sealed", p)
				}
			}
			if strings.Contains(all.String(), "v1.k") || strings.Contains(plainAll.String(), "v1.k") {
				t.Error("ciphertext was exported")
			}
			if !strings.Contains(all.String(), exportfmt.SealedMarker) {
				t.Error("sealed values are not marked")
			}
			for name, body := range opened {
				if strings.Contains(body, strings.Fields(ring2)[1]) {
					t.Errorf("%s holds the key", name)
				}
			}
		})
	}
}

func TestExportRefusesWithoutKey(t *testing.T) {
	i := newInstance(t, "people.db")
	create(t, "can-people", i)
	cfg, err := os.ReadFile(i.cfg)
	check(t, err)
	check(t, os.WriteFile(i.cfg, append(cfg, []byte("keyring_file: "+filepath.Join(i.dir, "absent")+"\n")...), 0o600))
	out := filepath.Join(t.TempDir(), "x.tar")
	code, log := run(t, "can-people", nil, "export", "-config", i.cfg, "-out", out, "-include-sealed")
	if code == 0 || !strings.Contains(log, "keyring") {
		t.Fatalf("exit %d: %s", code, log)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a failed export left a file")
	}
	// Without the flag no key is needed.
	export(t, "can-people", i, nil)
}

// CSV cells a spreadsheet would run as a formula are defused; the JSON is exact; an erased
// submission is in neither; the file is streamed to can-core.
func TestExportFormsCSVAndErasure(t *testing.T) {
	ctx := context.Background()
	i := newInstance(t, "forms.db")
	s, err := forms.OpenStore(i.db)
	check(t, err)
	defer func() { _ = s.Close() }()
	insert := func(email, name string) *forms.Lead {
		l := &forms.Lead{FormID: "contact", Email: email, Name: name, RetentionUntil: time.Now().Add(time.Hour)}
		check(t, s.Insert(ctx, l))
		return l
	}
	insert("kept@example.com", `=HYPERLINK("http://evil")`)
	gone := insert("erased@example.com", "Erased Person")
	if _, err := s.Erase(ctx, "user:admin", gone.ID, nil); err != nil {
		t.Fatal(err)
	}

	var got []byte
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	token := filepath.Join(i.dir, "token")
	check(t, os.WriteFile(token, []byte("tok"), 0o600))
	code, log := run(t, "can-forms", map[string]string{"CAN_API_URL": srv.URL, "CAN_MODULE": "forms", "CAN_TOKEN_FILE": token},
		"export", "-config", i.cfg)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, log)
	}
	if path != "PUT /v1/exports/forms" || auth != "Bearer tok" {
		t.Fatalf("%s %s", path, auth)
	}
	files := tarFiles(t, bytes.NewReader(got))
	for name, body := range files {
		if strings.Contains(body, "erased@example.com") || strings.Contains(body, "Erased Person") {
			t.Errorf("%s holds the erased submission", name)
		}
	}
	recs, err := csv.NewReader(strings.NewReader(files["records/leads.csv"])).ReadAll()
	check(t, err)
	if len(recs) != 2 {
		t.Fatalf("%d rows", len(recs)-1)
	}
	col := slices.Index(recs[0], "name")
	if recs[1][col] != `'=HYPERLINK("http://evil")` {
		t.Fatalf("CSV cell %q", recs[1][col])
	}
	if !strings.Contains(files["records/leads.jsonl"], `"name":"=HYPERLINK(\"http://evil\")"`) {
		t.Fatal(files["records/leads.jsonl"])
	}
}

func TestExportNoData(t *testing.T) {
	i := newInstance(t, "forms.db")
	if code, _ := run(t, "can-forms", nil, "export", "-config", i.cfg, "-out", filepath.Join(i.dir, "x.tar")); code != modbackup.ExitNoData {
		t.Fatalf("exit %d", code)
	}
}

// Files are exported with their bytes, the names only with --include-sealed; an erased file is
// not there, neither its record's name nor its content.
func TestExportFilesAndErasure(t *testing.T) {
	ctx := context.Background()
	f := newFiles(t)
	s, err := filesStore(f.instance, keyring)
	check(t, err)
	sp, err := s.CreateSpace(ctx, alice, "HR")
	check(t, err)
	body := "erase me, please\n"
	_, err = s.Upload(ctx, alice, files.UploadRequest{SpaceID: sp.ID, Name: "secret-cv.txt", Subject: "person-42", Body: strings.NewReader(body), Size: int64(len(body))})
	check(t, err)
	check(t, s.PurgeSubject(ctx, &files.Principal{Subject: "user:admin", Perms: []string{files.PermAdmin}}, "person-42", files.ClassContent))
	check(t, s.Close())

	ringFile := filepath.Join(f.dir, "keyring")
	check(t, os.WriteFile(ringFile, []byte(keyring), 0o600))
	cfg, err := os.ReadFile(f.cfg)
	check(t, err)
	check(t, os.WriteFile(f.cfg, append(cfg, []byte("keyring_path: "+ringFile+"\n")...), 0o600))
	out := export(t, "can-files", f.instance, nil, "-include-sealed")
	var content []string
	for name, b := range out {
		if strings.Contains(b, "erase me, please") || strings.Contains(b, "secret-cv") {
			t.Errorf("%s holds the erased file", name)
		}
		if strings.HasPrefix(name, "files/") {
			content = append(content, name)
		}
	}
	if want := "files/" + f.id + "/v1.txt"; len(content) != 1 || content[0] != want || out[want] != f.body {
		t.Fatalf("files in the export: %v", content)
	}
	if !strings.Contains(out["records/files.jsonl"], "q3.txt") {
		t.Error("the decrypted name is missing")
	}
	if m := moduleManifest(t, out); m.Files != 1 || len(m.Warnings) != 0 || !m.IncludeSealed {
		t.Fatalf("manifest %+v", m)
	}
}
