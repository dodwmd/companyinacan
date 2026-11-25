package corecli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestAdminDiagnoseCollectsStateWithoutSecrets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	seed(t, path, 3)
	const (
		hmac  = "hmac-secret-seed-0123456789abcdef"
		totp  = "JBSWY3DPEHPK3PXPSEEDTOTP"
		pw    = "hunter2-PASSWORD-seed"
		email = "alice.smith@example.com"
	)
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAdmin(ctx, "alice.smith@example.com", "argon-hash-seed-value-xxxxxxxxxxxxxxxxxxxx", totp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO modules(id,version,namespace,service_account,state,settings_hash,hmac_secret,manifest_json,last_error,updated_at)
		VALUES ('forms','1.0.0','can-forms','forms','failed','h',?, '{"secret":"x"}', ?, 1)`, hmac, "helm: dial postgres://app:"+pw+"@db/x failed for "+email); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO reconcile_status(id, at, ok, actions, failed, error) VALUES (1, 1700000000, 0, 2, 1, ?)`, "token="+pw+" rejected"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	company := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(company, []byte("name: Acme\nowner: "+email+"\nsmtp:\n  password: "+pw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(dir, "can-core.log")
	if err := os.WriteFile(logf, []byte(`{"msg":"up"}`+"\nAuthorization: Bearer "+hmac+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "bundle.tar.gz")
	env := map[string]string{"CAN_COMPANY_FILE": company}
	code, stdout, errs := run(t, env, "", "admin", "diagnose", "--db", path, "--out", out, "--log-file", logf)
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, stdout, errs)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
	}
	for name, body := range files {
		for _, seedv := range []string{hmac, totp, pw, email, "argon-hash-seed"} {
			if strings.Contains(body, seedv) {
				t.Errorf("%s leaks %q", name, seedv)
			}
		}
	}
	for _, want := range []string{"manifest.json", "versions.json", "state.json", "modules.json", "reconcile.json", "config.yaml", "logs.txt"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	if !strings.Contains(files["state.json"], `"audit_entries": 3`) || !strings.Contains(files["state.json"], `"audit_chain_ok": true`) {
		t.Errorf("state.json: %s", files["state.json"])
	}
	if !strings.Contains(files["modules.json"], `"id": "forms"`) || !strings.Contains(files["modules.json"], `"state": "failed"`) {
		t.Errorf("modules.json: %s", files["modules.json"])
	}
	if !strings.Contains(files["reconcile.json"], `"failed": 1`) {
		t.Errorf("reconcile.json: %s", files["reconcile.json"])
	}
}

func TestAdminDiagnoseDoesNotCreateOrMigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if code, _, _ := run(t, nil, "", "admin", "diagnose", "--db", path, "--out", filepath.Join(t.TempDir(), "b.tgz")); code == 0 {
		t.Fatal("diagnosing a missing database must fail, not create it")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("database file was created")
	}
}
