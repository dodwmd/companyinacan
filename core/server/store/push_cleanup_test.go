package store

import (
	"context"
	"testing"
)

// A provisioned user's push devices must not outlive the user: the username is derived from the
// identity, so the same person provisioned again would otherwise inherit them.
func TestUnlinkingProvisionedUserErasesPushDevices(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	id, err := db.CreateSSOUser(ctx, "sso_abc", SSOIdentity{IssuerHash: "i", Subject: "s"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	put := func(subject, ep string) {
		if _, err := db.ExecContext(ctx, `INSERT INTO push_subscriptions(subject, endpoint, p256dh, auth, created_at) VALUES (?,?,?,?,1)`, subject, ep, "p", "a"); err != nil {
			t.Fatal(err)
		}
	}
	count := func(subject string) (n int) {
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM push_subscriptions WHERE subject=?`, subject).Scan(&n)
		return
	}
	put("user:sso_abc", "https://push.example.com/a")
	put("user:other", "https://push.example.com/b")
	if err := db.UnlinkSSOIdentity(ctx, id.ID, nil); err != nil {
		t.Fatal(err)
	}
	if count("user:sso_abc") != 0 {
		t.Fatal("deleted user's push devices survived")
	}
	if count("user:other") != 1 {
		t.Fatal("another user's device was removed")
	}
	// Provisioned again with the same name: starts with no devices.
	if _, err := db.CreateSSOUser(ctx, "sso_abc", SSOIdentity{IssuerHash: "i", Subject: "s"}, nil); err != nil {
		t.Fatal(err)
	}
	if count("user:sso_abc") != 0 {
		t.Fatal("re-provisioned user inherited devices")
	}
}
