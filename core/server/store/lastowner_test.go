package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// Unlinking deletes a provisioned account. The only other owner being disabled (or a mere
// assignment with no account) leaves nobody, so the last enabled owner cannot be unlinked.
func TestUnlinkSSOIdentityKeepsOneEnabledOwner(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.CreateAdmin(ctx, "root", "h", "s"); err != nil { // the bootstrap owner
		t.Fatal(err)
	}
	id, err := db.CreateSSOUser(ctx, "sso-abc", SSOIdentity{IssuerHash: "h", Subject: "s", GroupsJSON: "[]", AMRJSON: "[]"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO rbac_assignments(subject,role,scope,granted_by,granted_at) VALUES ('user:sso-abc','owner','','t',0), ('user:ghost','owner','','t',0)`)
	exec(`UPDATE admin_users SET disabled_at=1 WHERE username='root'`)
	audited := false
	err = db.UnlinkSSOIdentity(ctx, id.ID, func(*sql.Tx, SSOIdentity) error { audited = true; return nil })
	if !errors.Is(err, ErrLastOwner) || audited {
		t.Fatalf("err = %v (audited %v), want ErrLastOwner", err, audited)
	}
	if _, err := db.AdminByUsername(ctx, "sso-abc"); err != nil {
		t.Fatalf("the owner was deleted: %v", err)
	}
	exec(`UPDATE admin_users SET disabled_at=0 WHERE username='root'`)
	if err := db.UnlinkSSOIdentity(ctx, id.ID, nil); err != nil {
		t.Fatalf("with another enabled owner: %v", err)
	}
}

// Linking deletes the provisioned user and its roles; they do not move to the target. When that user
// is the only enabled owner the link is refused and nothing changes.
func TestLinkSSOIdentityKeepsOneEnabledOwner(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.CreateAdmin(ctx, "root", "h", "s"); err != nil {
		t.Fatal(err)
	}
	carol, err := db.CreateAdmin(ctx, "carol", "h", "s")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateSSOUser(ctx, "sso-abc", SSOIdentity{IssuerHash: "h", Subject: "s", GroupsJSON: "[]", AMRJSON: "[]"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO rbac_assignments(subject,role,scope,granted_by,granted_at) VALUES ('user:sso-abc','owner','','t',0)`)
	exec(`UPDATE admin_users SET disabled_at=1 WHERE username='root'`)
	audited := false
	err = db.LinkSSOIdentity(ctx, id.ID, carol.ID, "root", func(*sql.Tx, SSOIdentity, AdminUser) error { audited = true; return nil })
	if !errors.Is(err, ErrLastOwner) || audited {
		t.Fatalf("err = %v (audited %v), want ErrLastOwner", err, audited)
	}
	if got, err := db.SSOIdentityByUser(ctx, id.UserID); err != nil || got.ID != id.ID {
		t.Fatalf("the identity moved or the owner was deleted: %+v %v", got, err)
	}
	// A target that is itself an enabled owner leaves one behind.
	exec(`INSERT INTO rbac_assignments(subject,role,scope,granted_by,granted_at) VALUES ('user:carol','owner','','t',0)`)
	if err := db.LinkSSOIdentity(ctx, id.ID, carol.ID, "root", nil); err != nil {
		t.Fatalf("with another enabled owner: %v", err)
	}
}
