package corecli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/store"
)

// A disabled account is brought back only by `admin enable`, never by a password reset.
func TestAdminEnableAndResetPasswordDoesNotReEnable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	if code, _, e := run(t, nil, "correct horse battery staple\n", "admin", "bootstrap", "--db", path, "--username", "root"); code != 0 {
		t.Fatal(e)
	}
	db, _ := store.Open(ctx, path)
	if _, err := db.ExecContext(ctx, `UPDATE admin_users SET disabled_at=5 WHERE username='root'`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	code, _, errs := run(t, nil, "a brand new long password\n", "admin", "reset-password", "--db", path, "--username", "root")
	if code == 0 || !strings.Contains(errs, "disabled") {
		t.Fatalf("reset of a disabled account: %d %q", code, errs)
	}
	if code, _, _ := run(t, nil, "", "admin", "enable", "--db", path); code == 0 {
		t.Fatal("enable without a username")
	}
	if code, _, _ := run(t, nil, "", "admin", "enable", "--db", path, "--username", "ghost"); code == 0 {
		t.Fatal("enable of an unknown account")
	}
	code, out, errs := run(t, nil, "", "admin", "enable", "--db", path, "--username", "root")
	if code != 0 || !strings.Contains(out, "enabled") {
		t.Fatalf("enable: %d %q %q", code, out, errs)
	}
	db, _ = store.Open(ctx, path)
	defer func() { _ = db.Close() }()
	u, _ := db.AdminByUsername(ctx, "root")
	if db.AdminDisabled(ctx, u.ID) {
		t.Fatal("still disabled")
	}
	var n int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM audit_log WHERE action='identity.enable'`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit entries %d", n)
	}
	if code, _, e := run(t, nil, "a brand new long password\n", "admin", "reset-password", "--db", path, "--username", "root"); code != 0 {
		t.Fatalf("reset after enable: %s", e)
	}
}
