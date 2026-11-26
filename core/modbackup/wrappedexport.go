package modbackup

// The export of a wrapped application (specs/070-company-export): the application's own data in
// its own schema. A PostgreSQL application gives a plain SQL pg_dump (made by the Postgres image),
// a SQLite one its tables as JSON Lines and CSV, and the files of its volume are copied. Unlike
// the in-house services nobody here owns these schemas, so secrets are kept out by name: whole
// tables that hold credentials or settings are left out, and any other column that is named like a
// credential is blanked.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/exportfmt"
)

// wrappedRules say what of a wrapped application is never exported.
type wrappedRules struct {
	// secretTables hold credentials, sessions or settings with passwords in them: their rows are
	// never exported.
	secretTables []string
	// uploads is the directory of the volume that holds what people uploaded, "." for a volume that
	// holds nothing else. Only it is copied: an application keeps its configuration (with its keys
	// and database password), sessions and caches elsewhere on the same volume.
	uploads string
	note    string
}

var wrapped = map[string]wrappedRules{
	"analytics": {secretTables: []string{"users", "api_tokens", "sessions", "store"},
		note: "GoatCounter: its SQLite tables as they are. Accounts, API tokens and sessions are left out."},
	"newsletter": {secretTables: []string{"users", "sessions", "settings"}, uploads: ".",
		note: "Listmonk: a plain SQL pg_dump (load with psql) and the uploads. Accounts, sessions and the settings (which hold the mail relay password) are left out."},
	"support": {secretTables: []string{"users", "password_resets", "sessions", "options", "mailboxes", "jobs", "failed_jobs"}, uploads: "storage/app",
		note: "FreeScout: a plain SQL pg_dump (load with psql) and the attachments (storage/app). Agent accounts, mailbox settings (with their passwords), application options and the rest of the volume (configuration, sessions, caches, logs) are left out."},
}

// secretPatterns are table names that are left out of every wrapped export, whatever the rules say.
var secretPatterns = []string{"*password*", "*session*", "*token*", "*secret*", "*oauth*", "*api_key*"}

var secretNameRe = regexp.MustCompile(`(?i)password|passwd|secret|token|api_?key|private_?key|remember|totp|two_factor|2fa|smtp|auth_?key|recovery_code`)

// secretFileRe are files of a volume that hold keys or configuration with secrets.
var secretFileRe = regexp.MustCompile(`(?i)^(\.env.*|.*\.(key|pem|p12|pfx|keystore)|id_(rsa|dsa|ecdsa|ed25519).*|\.htpasswd|\.pgpass|\.netrc)$`)

// PGExportExcludes returns the pg_dump --exclude-table-data patterns of a wrapped module, and
// false for a module this version has no export rules for (which is then not exported).
func PGExportExcludes(module string) ([]string, bool) {
	r, ok := wrapped[module]
	if !ok {
		return nil, false
	}
	var out []string
	for _, t := range r.secretTables {
		out = append(out, "public."+t)
	}
	for _, p := range secretPatterns {
		out = append(out, "*."+p)
	}
	return out, true
}

// ExportWrapped sends the export of a wrapped module's data to can-core. w.PGDump, when set, is
// the plain SQL dump.
func ExportWrapped(ctx context.Context, w Wrapped, getenv func(string) string, stdout io.Writer) error {
	if err := w.check(); err != nil {
		return err
	}
	api := newClient(getenv)
	api.export = true
	if err := api.check(); err != nil {
		return err
	}
	rules, ok := wrapped[api.module]
	if !ok {
		return fmt.Errorf("this version has no export rules for %q", api.module)
	}
	tmp, err := os.MkdirTemp(w.WorkDir, ".export-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	taken := time.Now().UTC()
	copyPath := ""
	if w.SQLite != "" {
		src := filepath.Join(w.DataDir, w.SQLite)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			return ErrNoData
		} else if err != nil {
			return err
		}
		copyPath = filepath.Join(tmp, "copy.db")
		if err := snapshot(ctx, src, copyPath); err != nil {
			return err
		}
		if err := integrity(ctx, copyPath); err != nil {
			return fmt.Errorf("the copy of the database: %w", err)
		}
	}
	_, err = api.putStream(ctx, "export.tar", func(out io.Writer) error {
		a := exportfmt.NewArchive(out, taken)
		m := ModuleExport{Format: exportfmt.FormatVersion, Module: api.module, Binary: WrappedBinary, TakenAt: taken, Notes: []string{rules.note}}
		if w.PGDump != "" {
			if err := a.Render("native/database.sql", func(out io.Writer) error { return redactSQL(w.PGDump, out) }); err != nil {
				return err
			}
		}
		if copyPath != "" {
			if err := exportForeign(ctx, a, copyPath, rules, &m); err != nil {
				return err
			}
		}
		if w.Files {
			if err := exportVolume(ctx, a, w.DataDir, rules.uploads, &m); err != nil {
				return err
			}
		}
		if err := a.JSON(exportfmt.ManifestName, m); err != nil {
			return err
		}
		return a.Close()
	})
	if err == nil {
		fmt.Fprintln(stdout, "export ok")
	}
	return err
}

// exportForeign writes every table of a SQLite database whose schema is not ours.
func exportForeign(ctx context.Context, a *exportfmt.Archive, dbPath string, rules wrappedRules, m *ModuleExport) error {
	db, err := open(dbPath, "mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	names, err := exportfmt.TableNames(ctx, conn)
	if err != nil {
		return err
	}
	skipped := map[string]bool{}
	for _, t := range rules.secretTables {
		skipped[t] = true
	}
	var tables []exportfmt.Table
	for _, n := range names {
		if skipped[n] || secretTable(n) {
			tables = append(tables, exportfmt.Table{Name: n, Skip: whySecret})
			continue
		}
		cols, err := conn.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, n)
		if err != nil {
			return err
		}
		t := exportfmt.Table{Name: n, Omitted: map[string]string{}}
		var keep []string
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				_ = cols.Close()
				return err
			}
			switch {
			case secretNameRe.MatchString(c):
				t.Omitted[c] = whySecret
			case strings.ContainsAny(c, " \t\n"):
				t.Omitted[c] = "column name this version cannot express"
			default:
				keep = append(keep, c)
			}
		}
		if err := errors.Join(cols.Err(), cols.Close()); err != nil {
			return err
		}
		t.Columns = strings.Join(keep, " ")
		if len(keep) == 0 || !exportfmt.ValidName("records/"+n+".jsonl") || strings.Contains(n, "/") {
			t.Skip = "nothing exportable"
		}
		tables = append(tables, t)
	}
	schema, warnings, err := exportfmt.Database(ctx, a, conn, tables, exportfmt.Options{})
	if err != nil {
		return err
	}
	m.Tables = map[string]int{}
	for _, t := range schema.Tables {
		m.Tables[t.Name] = t.Rows
	}
	m.Warnings = append(m.Warnings, warnings...)
	return nil
}

func secretTable(name string) bool {
	name = strings.ToLower(name)
	for _, p := range secretPatterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

var copyRe = regexp.MustCompile(`^COPY \S+ \((.*)\) FROM stdin;$`)

// redactSQL copies a plain pg_dump, blanking in every COPY block the fields of columns that are
// named like a credential. It holds one line at a time.
func redactSQL(file string, out io.Writer) error {
	f, err := os.Open(file) //nolint:gosec // the work directory
	if err != nil {
		return fmt.Errorf("the database dump: %w", err)
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 64<<10)
	w := bufio.NewWriterSize(out, 64<<10)
	var blank []int // field numbers of the COPY block being read
	inCopy := false
	for first := true; ; first = false {
		line, err := r.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		body := bytes.TrimRight(line, "\n")
		if first && !bytes.HasPrefix(body, []byte("--")) {
			return fmt.Errorf("%s is not a plain SQL pg_dump", file)
		}
		switch {
		case inCopy && string(body) == `\.`:
			inCopy, blank = false, nil
		case inCopy && len(blank) > 0:
			fields := bytes.Split(body, []byte("\t"))
			for _, i := range blank {
				if i < len(fields) && string(fields[i]) != `\N` {
					fields[i] = nil
				}
			}
			line = append(bytes.Join(fields, []byte("\t")), '\n')
		case !inCopy:
			if m := copyRe.FindSubmatch(body); m != nil {
				inCopy = true
				for i, c := range strings.Split(string(m[1]), ", ") {
					if secretNameRe.MatchString(c) {
						blank = append(blank, i)
					}
				}
			}
		}
		if _, werr := w.Write(line); werr != nil {
			return werr
		}
		if errors.Is(err, io.EOF) {
			return w.Flush()
		}
	}
}

// exportVolume copies the regular files under uploads of a volume. Links are not followed, and
// files that hold keys or secrets are left out.
func exportVolume(ctx context.Context, a *exportfmt.Archive, dir, uploads string, m *ModuleExport) error {
	if uploads == "" {
		return nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var secret, unnamed int
	var paths []string
	err = fs.WalkDir(root.FS(), uploads, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p != "." { // also: nothing was ever uploaded
				return nil
			}
			return err
		}
		switch {
		case p == stageDir || p == oldDir || p == "lost+found":
			return fs.SkipDir
		case !d.Type().IsRegular():
		case secretFileRe.MatchString(d.Name()):
			secret++
		case !exportfmt.ValidName("files/" + p):
			unnamed++
		default:
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		f, err := root.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err == nil {
			_, err = a.Copy("files/"+p, fi.Size(), io.MultiReader(f, io.LimitReader(zeros{}, fi.Size())))
		}
		_ = f.Close()
		if err != nil {
			return err
		}
		m.Files++
		m.FileBytes += fi.Size()
	}
	if secret > 0 {
		m.Notes = append(m.Notes, fmt.Sprintf("%d file(s) holding keys or secrets were left out", secret))
	}
	if unnamed > 0 {
		m.Warnings = append(m.Warnings, fmt.Sprintf("%d file(s) with names an archive cannot safely hold were left out", unnamed))
	}
	return nil
}
