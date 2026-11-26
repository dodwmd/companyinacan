package modbackup_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/modbackup"
)

func wrappedExport(t *testing.T, module string, w modbackup.Wrapped) map[string]string {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/exports/"+module {
			t.Error(r.URL.Path)
		}
		got, _ = io.ReadAll(r.Body)
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	token := filepath.Join(t.TempDir(), "token")
	check(t, os.WriteFile(token, []byte("tok"), 0o600))
	env := map[string]string{"CAN_API_URL": srv.URL, "CAN_MODULE": module, "CAN_TOKEN_FILE": token}
	w.WorkDir = t.TempDir()
	check(t, modbackup.ExportWrapped(context.Background(), w, func(k string) string { return env[k] }, io.Discard))
	return tarFiles(t, bytes.NewReader(got))
}

func TestWrappedExportKeepsSecretsOut(t *testing.T) {
	data := t.TempDir()
	// As FreeScout lays its volume out: its configuration is a file with no telling name.
	for name, body := range map[string]string{"storage/app/attachment/a.pdf": "an attachment", "config": "APP_KEY=base64:KEYMATERIAL",
		"storage/framework/sessions/abc": "KEYMATERIAL", "storage/logs/x.log": "KEYMATERIAL",
		"storage/app/.env": "APP_KEY=base64:KEYMATERIAL", "storage/app/private.key": "KEYMATERIAL"} {
		check(t, os.MkdirAll(filepath.Dir(filepath.Join(data, name)), 0o700))
		check(t, os.WriteFile(filepath.Join(data, name), []byte(body), 0o600))
	}
	check(t, os.Symlink("/etc/hostname", filepath.Join(data, "storage/app/link")))
	dump := filepath.Join(t.TempDir(), "database.sql")
	sql := "--\n-- PostgreSQL database dump\n--\nCREATE TABLE public.customers (id integer, name text, api_token text);\n" +
		"COPY public.customers (id, name, api_token) FROM stdin;\n1\tAnn\tTOKENVALUE\n2\tBob\t\\N\n\\.\n" +
		"COPY public.threads (id, body) FROM stdin;\n1\tmy password is in the body\n\\.\n"
	check(t, os.WriteFile(dump, []byte(sql), 0o600))
	out := wrappedExport(t, "support", modbackup.Wrapped{DataDir: data, Files: true, PGDump: dump})
	got := out["native/database.sql"]
	if strings.Contains(got, "TOKENVALUE") || !strings.Contains(got, "1\tAnn\t\n2\tBob\t\\N\n") || !strings.Contains(got, "my password is in the body") {
		t.Fatalf("redaction: %q", got)
	}
	if out["files/storage/app/attachment/a.pdf"] != "an attachment" {
		t.Fatal("the attachment is missing")
	}
	for name, body := range out {
		if strings.Contains(body, "KEYMATERIAL") || strings.HasSuffix(name, "/link") {
			t.Errorf("%s is in the export", name)
		}
	}
	if m := moduleManifest(t, out); m.Files != 1 || len(m.Notes) != 2 {
		t.Fatalf("%+v", m)
	}
}

func TestWrappedSQLiteExport(t *testing.T) {
	data := t.TempDir()
	db := sqlDB(t, filepath.Join(data, "goatcounter.sqlite3"))
	sqlExec(t, db, `CREATE TABLE sites (site_id INTEGER PRIMARY KEY, code TEXT, session_secret TEXT)`)
	sqlExec(t, db, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, password BLOB)`)
	sqlExec(t, db, `CREATE TABLE api_tokens (id INTEGER PRIMARY KEY, token TEXT)`)
	sqlExec(t, db, `INSERT INTO sites VALUES (1, '=cmd', 'SECRETVALUE')`)
	sqlExec(t, db, `INSERT INTO users VALUES (1, 'admin@example.com', 'HASHVALUE')`)
	sqlExec(t, db, `INSERT INTO api_tokens VALUES (1, 'TOKENVALUE')`)
	check(t, db.Close())
	out := wrappedExport(t, "analytics", modbackup.Wrapped{DataDir: data, SQLite: "goatcounter.sqlite3"})
	for name, body := range out {
		for _, bad := range []string{"SECRETVALUE", "HASHVALUE", "TOKENVALUE"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s holds %s", name, bad)
			}
		}
	}
	if !strings.Contains(out["records/sites.csv"], "'=cmd") || !strings.Contains(out["records/sites.jsonl"], `"code":"=cmd"`) {
		t.Fatal(out["records/sites.csv"])
	}
	if _, ok := out["records/users.jsonl"]; ok {
		t.Fatal("accounts were exported")
	}
}

func TestWrappedExportNeedsRules(t *testing.T) {
	env := map[string]string{"CAN_API_URL": "http://127.0.0.1:1", "CAN_MODULE": "mystery", "CAN_TOKEN_FILE": "/nonexistent"}
	err := modbackup.ExportWrapped(context.Background(), modbackup.Wrapped{DataDir: t.TempDir(), Files: true, WorkDir: t.TempDir()}, func(k string) string { return env[k] }, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no export rules") {
		t.Fatal(err)
	}
}
