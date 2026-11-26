package companyexport

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/exportfmt"
	"github.com/dodwmd/companyinacan/core/modbackup"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/store"
)

const (
	passwordHash = "argon2id$HASHVALUE-never-exported"
	totpSecret   = "TOTPSEEDNEVEREXPORTED234"
	hmacSecret   = "hmac-never-exported"
)

type runner struct {
	s    *Service
	run  map[string]func() error
	ran  []string
	seen []bool
}

func (r *runner) RunExport(_ context.Context, module string, sealed bool) error {
	r.ran, r.seen = append(r.ran, module), append(r.seen, sealed)
	return r.run[module]()
}

type fixture struct {
	s  *Service
	db *store.DB
	r  *runner
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, err := store.Open(ctx, path)
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.CreateAdmin(ctx, "root", passwordHash, totpSecret)
	must(t, err)
	for _, id := range []string{"forms", "work", "wiki"} {
		must(t, db.UpsertModule(ctx, store.Module{ID: id, Version: "1", Namespace: "mod-" + id, ServiceAccount: id, State: store.StateEnabled, HMACSecret: hmacSecret}, nil, nil))
	}
	company := filepath.Join(dir, "company.yaml")
	must(t, os.WriteFile(company, []byte("schema: 1\nname: Acme # keep\nprovider: {type: local, options: {api_token: do-not-export}}\n"), 0o600))
	r := &runner{}
	s := &Service{DBPath: path, CompanyFile: company, Company: "Acme", Version: "test", Audit: audit.New(db), Modules: db.ListModules, Hooks: r,
		Site: func(context.Context) ([]byte, error) { return []byte("site-archive"), nil }}
	r.s = s
	r.run = map[string]func() error{
		"forms": func() error { return s.Accept("forms", moduleTar(t, "forms", "records/leads.jsonl", `{"id":"L1"}`)) },
		"work":  func() error { return dumps.ErrNoData },
		"wiki":  func() error { return errors.Join(dumps.ErrNoExport, errors.New("shell scripts")) },
	}
	return fixture{s, db, r}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func moduleTar(t *testing.T, module, name, body string) io.Reader {
	t.Helper()
	var b bytes.Buffer
	a := exportfmt.NewArchive(&b, time.Now())
	must(t, a.Bytes(name, []byte(body)))
	must(t, a.JSON(exportfmt.ManifestName, modbackup.ModuleExport{Format: 1, Module: module}))
	must(t, a.Close())
	return &b
}

func read(t *testing.T, archive []byte) (map[string]string, []string) {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	must(t, err)
	tr := tar.NewReader(zr)
	files, order := map[string]string{}, []string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, order
		}
		must(t, err)
		b, err := io.ReadAll(tr)
		must(t, err)
		files[h.Name], order = string(b), append(order, h.Name)
	}
}

func (f fixture) export(t *testing.T, o Options) ([]byte, Manifest, error) {
	t.Helper()
	o.Actor = "root"
	req, err := f.s.Prepare(context.Background(), o)
	if err != nil {
		return nil, Manifest{}, err
	}
	var out bytes.Buffer
	m, err := req.Write(context.Background(), &out)
	return out.Bytes(), m, err
}

func (f fixture) actions(t *testing.T) string {
	t.Helper()
	recs, err := f.s.Audit.Query(context.Background(), audit.Filter{ActionPrefix: "company."})
	must(t, err)
	var out []string
	for _, r := range recs {
		out = append(out, r.Actor+" "+r.Action)
	}
	return strings.Join(out, ", ")
}

func TestExportOfTheWholeCompany(t *testing.T) {
	f := newFixture(t)
	archive, m, err := f.export(t, Options{})
	must(t, err)
	files, order := read(t, archive)
	if order[len(order)-1] != "manifest.json" {
		t.Fatalf("the manifest is not last: %v", order)
	}
	for _, name := range []string{"README.txt", "company/company.yaml", "company/records/audit_log.jsonl", "company/records/audit_log.csv",
		"company/records/admin_users.jsonl", "company/schema.json", "site/published.tar.gz", "modules/forms/records/leads.jsonl", "modules/forms/manifest.json"} {
		if _, ok := files[name]; !ok {
			t.Errorf("%s is missing (%v)", name, order)
		}
	}
	for name, body := range files {
		for _, secret := range []string{passwordHash, totpSecret, hmacSecret, "do-not-export"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s holds %q", name, secret)
			}
		}
	}
	if !strings.Contains(files["company/company.yaml"], "api_token: REDACTED") || !strings.Contains(files["company/company.yaml"], "# keep") {
		t.Error(files["company/company.yaml"])
	}
	// The request itself is already in the exported audit log.
	if !strings.Contains(files["company/records/audit_log.jsonl"], `"action":"company.export"`) {
		t.Error("the export is not in its own audit log")
	}
	status := map[string]string{}
	for _, mod := range m.Modules {
		status[mod.ID] = mod.Status
	}
	if status["forms"] != StatusExported || status["work"] != StatusNoData || status["wiki"] != StatusNotExported {
		t.Fatalf("%v", status)
	}
	if len(m.Warnings) != 1 || !strings.Contains(m.Warnings[0], "wiki is NOT in this export") || m.Partial {
		t.Fatalf("%+v", m)
	}
	var inFile Manifest
	must(t, json.Unmarshal([]byte(files["manifest.json"]), &inFile))
	if !inFile.Complete || inFile.IncludeSealed {
		t.Fatalf("%+v", inFile)
	}
	if got := f.actions(t); got != "root company.export.completed, root company.export" {
		t.Fatal(got)
	}
}

func TestExportOfSomeModules(t *testing.T) {
	f := newFixture(t)
	archive, m, err := f.export(t, Options{Only: []string{"forms"}, IncludeSealed: true})
	must(t, err)
	files, _ := read(t, archive)
	if _, ok := files["company/company.yaml"]; ok || !m.Partial || len(f.r.ran) != 1 || !f.r.seen[0] {
		t.Fatalf("%v %v %+v", f.r.ran, f.r.seen, m)
	}
	if _, _, err := f.export(t, Options{Skip: []string{"nope"}}); !errors.Is(err, ErrBadRequest) {
		t.Fatal(err)
	}
}

// A module that fails fails the whole export, and what was written cannot be taken for complete.
func TestFailedModuleFailsTheExport(t *testing.T) {
	for name, run := range map[string]func(f fixture, t *testing.T) error{
		"job failed":   func(fixture, *testing.T) error { return errors.New("exit 1") },
		"sent nothing": func(fixture, *testing.T) error { return nil },
		"no manifest": func(f fixture, t *testing.T) error {
			var b bytes.Buffer
			a := exportfmt.NewArchive(&b, time.Now())
			must(t, a.Bytes("records/x.jsonl", []byte("{}")))
			must(t, a.Close())
			return f.s.Accept("forms", &b)
		},
		"cut off": func(f fixture, t *testing.T) error {
			b, _ := io.ReadAll(moduleTar(t, "forms", "records/x.jsonl", strings.Repeat("y", 4000)))
			return f.s.Accept("forms", bytes.NewReader(b[:1000]))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.r.run["forms"] = func() error { return run(f, t) }
			archive, _, err := f.export(t, Options{})
			if err == nil || !strings.Contains(err.Error(), "--skip forms") {
				t.Fatal(err)
			}
			zr, zerr := gzip.NewReader(bytes.NewReader(archive))
			if zerr == nil {
				_, zerr = io.Copy(io.Discard, zr)
			}
			if zerr == nil {
				t.Fatal("a failed export left a well-formed gzip stream")
			}
			if got := f.actions(t); !strings.HasPrefix(got, "root company.export.failed") {
				t.Fatal(got)
			}
			// The slot is free again.
			f.r.run["forms"] = func() error { return dumps.ErrNoData }
			if _, _, err := f.export(t, Options{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A module writes only under its own directory, once, and only while its Job runs.
func TestAcceptIsConfined(t *testing.T) {
	f := newFixture(t)
	if err := f.s.Accept("forms", strings.NewReader("")); !errors.Is(err, ErrNoTurn) {
		t.Fatal("accepted outside an export", err)
	}
	hostile := func(h tar.Header) io.Reader {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		must(t, tw.WriteHeader(&h))
		must(t, tw.Close())
		return &b
	}
	f.r.run["forms"] = func() error {
		if err := f.s.Accept("work", moduleTar(t, "work", "x", "y")); !errors.Is(err, ErrNoTurn) {
			t.Error("another module was accepted", err)
		}
		for _, h := range []tar.Header{
			{Name: "../company/company.yaml", Typeflag: tar.TypeReg},
			{Name: "/etc/passwd", Typeflag: tar.TypeReg},
			{Name: "a/../../b", Typeflag: tar.TypeReg},
			{Name: `a\..\b`, Typeflag: tar.TypeReg},
			{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
		} {
			if err := f.s.Accept("forms", hostile(h)); err == nil || errors.Is(err, ErrNoTurn) {
				t.Errorf("%q: %v", h.Name, err)
			}
		}
		// Nothing was written, so a good upload is still taken, but only one.
		if err := f.s.Accept("forms", moduleTar(t, "forms", "records/x", "y")); err != nil {
			return err
		}
		if err := f.s.Accept("forms", moduleTar(t, "forms", "records/z", "y")); !errors.Is(err, ErrNoTurn) {
			t.Error("a second upload", err)
		}
		return nil
	}
	archive, _, err := f.export(t, Options{})
	must(t, err)
	_, order := read(t, archive)
	for _, n := range order {
		if !exportfmt.ValidName(n) || strings.Contains(n, "passwd") {
			t.Error(n)
		}
	}
}

func TestOneExportAtATime(t *testing.T) {
	f := newFixture(t)
	req, err := f.s.Prepare(context.Background(), Options{Actor: "root"})
	must(t, err)
	if _, err := f.s.Prepare(context.Background(), Options{Actor: "root"}); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	req.Abort()
	f.s.DumpRunning = func() bool { return true }
	if _, err := f.s.Prepare(context.Background(), Options{Actor: "root"}); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}

func TestCoreExportClassifiesEveryColumn(t *testing.T) {
	f := newFixture(t)
	db, err := f.s.stateDB()
	must(t, err)
	defer func() { _ = db.Close() }()
	tx, err := db.Begin()
	must(t, err)
	defer func() { _ = tx.Rollback() }()
	missing, err := exportfmt.Classify(context.Background(), tx, coreTables)
	must(t, err)
	if len(missing) > 0 {
		t.Fatalf("not classified in core/server/companyexport/core.go: %v", missing)
	}
}

func TestSocket(t *testing.T) {
	f := newFixture(t)
	dir, err := os.MkdirTemp("", "ce")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "export.sock")
	l, err := Listen(path)
	must(t, err)
	fi, err := os.Stat(path)
	must(t, err)
	if fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- f.s.Serve(ctx, l) }()
	var out bytes.Buffer
	must(t, Fetch(ctx, path, Options{}, &out))
	if files, _ := read(t, out.Bytes()); !strings.Contains(files["manifest.json"], `"created_by": "cli"`) {
		t.Fatal(files["manifest.json"])
	}
	if err := Fetch(ctx, path, Options{Only: []string{"nope"}}, io.Discard); err == nil || !strings.Contains(err.Error(), "no module") {
		t.Fatal(err)
	}
	f.r.run["forms"] = func() error { return errors.New("exit 1") }
	if err := Fetch(ctx, path, Options{}, io.Discard); err == nil || !strings.Contains(err.Error(), "module forms") {
		t.Fatal("a failure in mid-stream must reach the client", err)
	}
	cancel()
	must(t, <-done)
}
