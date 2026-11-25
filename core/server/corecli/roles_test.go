package corecli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRolesBreakGlass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	env := map[string]string{"CAN_ADMIN_PASSWORD": "a long enough password 1"}
	for _, u := range []string{"first", "second"} {
		if code, _, e := run(t, env, "", "admin", "bootstrap", "--db", path, "--username", u); code != 0 {
			t.Fatalf("bootstrap %s: %d %s", u, code, e)
		}
	}
	_, out, _ := run(t, nil, "", "admin", "roles", "list", "--db", path)
	if !strings.Contains(out, "first: owner") || strings.Contains(out, "second: owner") {
		t.Fatalf("first admin is the owner, later ones start empty:\n%s", out)
	}
	if code, _, e := run(t, nil, "", "admin", "roles", "grant", "second", "owner", "--db", path); code != 0 {
		t.Fatalf("grant: %d %s", code, e)
	}
	if code, _, e := run(t, nil, "", "admin", "roles", "revoke", "first", "owner", "--db", path); code != 0 {
		t.Fatalf("revoke with another owner present: %d %s", code, e)
	}
	if code, _, e := run(t, nil, "", "admin", "roles", "revoke", "second", "owner", "--db", path); code == 0 || !strings.Contains(e, "last owner") {
		t.Fatalf("the last owner must stay: %d %s", code, e)
	}
	if code, _, _ := run(t, nil, "", "admin", "roles", "grant", "nobody", "owner", "--db", path); code == 0 {
		t.Fatal("unknown user accepted")
	}
	if code, _, _ := run(t, nil, "", "admin", "roles", "grant", "first", "no-such-role", "--db", path); code == 0 {
		t.Fatal("unknown role accepted")
	}
}
