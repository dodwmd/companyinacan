package corecli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/auth"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func init() { auth.TestingParams() }

func run(t *testing.T, env map[string]string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Execute(args, strings.NewReader(stdin), &out, &errb, func(k string) string { return env[k] })
	return code, out.String(), errb.String()
}

func seed(t *testing.T, path string, n int) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	l := audit.New(db)
	for i := 0; i < n; i++ {
		if _, err := l.Append(ctx, audit.Entry{Actor: "a", Action: "x", Target: "t"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuditVerifyCleanTamperedAndAnchored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	seed(t, path, 4)

	code, out, _ := run(t, nil, "", "audit", "verify", "--db", path)
	if code != 0 || !strings.Contains(out, "OK") || !strings.Contains(out, "4 entries") {
		t.Fatalf("clean: %d %q", code, out)
	}
	code, head, _ := run(t, nil, "", "audit", "head", "--db", path)
	if code != 0 || !strings.HasPrefix(head, "4:") {
		t.Fatalf("head: %d %q", code, head)
	}
	if code, out, _ := run(t, nil, "", "audit", "verify", "--db", path, "--head", strings.TrimSpace(head)); code != 0 {
		t.Fatalf("anchored: %d %q", code, out)
	}

	db, _ := store.Open(context.Background(), path)
	_, _ = db.Exec(`UPDATE audit_log SET target='evil' WHERE seq=2`)
	_ = db.Close()
	code, out, errs := run(t, nil, "", "audit", "verify", "--db", path)
	if code != 1 || !strings.Contains(out+errs, "TAMPERED") || !strings.Contains(out+errs, "hash-mismatch") || !strings.Contains(out+errs, "record 2") {
		t.Fatalf("tampered: %d out=%q err=%q", code, out, errs)
	}
	code, out, _ = run(t, nil, "", "audit", "verify", "--db", path, "--output", "json")
	if code != 1 || !strings.Contains(out, `"kind":"hash-mismatch"`) {
		t.Fatalf("json: %d %q", code, out)
	}
}

func TestAuditVerifyDetectsTruncationWithHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	seed(t, path, 4)
	_, head, _ := run(t, nil, "", "audit", "head", "--db", path)
	db, _ := store.Open(context.Background(), path)
	_, _ = db.Exec(`DELETE FROM audit_log WHERE seq=4`)
	_ = db.Close()
	if code, _, _ := run(t, nil, "", "audit", "verify", "--db", path); code != 0 {
		t.Fatal("chain alone cannot see tail truncation")
	}
	if code, _, _ := run(t, nil, "", "audit", "verify", "--db", path, "--head", strings.TrimSpace(head)); code != 1 {
		t.Fatal("anchored head must detect truncation")
	}
}

func TestAuditVerifyMissingDB(t *testing.T) {
	code, _, errs := run(t, nil, "", "audit", "verify", "--db", filepath.Join(t.TempDir(), "nope", "state.db"))
	if code == 0 || errs == "" {
		t.Fatalf("%d %q", code, errs)
	}
	// it must not create an empty database and report it "OK"
	code, _, _ = run(t, nil, "", "audit", "verify", "--db", filepath.Join(t.TempDir(), "absent.db"))
	if code == 0 {
		t.Fatal("verified a database that does not exist")
	}
}

func TestAuditExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	seed(t, path, 2)
	code, out, _ := run(t, nil, "", "audit", "export", "--db", path)
	if code != 0 || strings.Count(out, "\n") != 2 {
		t.Fatalf("%d %q", code, out)
	}
}

func TestAdminBootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	code, out, errs := run(t, nil, "correct horse battery staple\n", "admin", "bootstrap", "--db", path, "--username", "root")
	if code != 0 || !strings.Contains(out, "otpauth://totp/") || !strings.Contains(out, "secret=") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if strings.Contains(out, "correct horse") {
		t.Fatal("password echoed")
	}
	db, _ := store.Open(context.Background(), path)
	u, err := db.AdminByUsername(context.Background(), "root")
	_ = db.Close()
	if err != nil || !strings.HasPrefix(u.PasswordHash, "$argon2id$") || u.TOTPSecret == "" {
		t.Fatalf("%+v %v", u, err)
	}
	// second bootstrap refuses
	if code, _, errs := run(t, nil, "another long password!\n", "admin", "bootstrap", "--db", path, "--username", "root"); code == 0 || !strings.Contains(errs, "already") {
		t.Fatalf("%d %q", code, errs)
	}
	// weak password
	if code, _, _ := run(t, nil, "short\n", "admin", "bootstrap", "--db", filepath.Join(t.TempDir(), "x.db"), "--username", "root"); code == 0 {
		t.Fatal("weak password accepted")
	}
	// env var password source
	if code, _, _ := run(t, map[string]string{"CAN_ADMIN_PASSWORD": "correct horse battery staple"}, "", "admin", "bootstrap", "--db", filepath.Join(t.TempDir(), "y.db"), "--username", "root"); code != 0 {
		t.Fatal("env password rejected")
	}
}

func TestVersionAndUsage(t *testing.T) {
	if code, out, _ := run(t, nil, "", "version"); code != 0 || !strings.HasPrefix(out, "can-core ") {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, _ := run(t, nil, "", "bogus"); code != 2 {
		t.Fatalf("unknown command code = %d", code)
	}
}
