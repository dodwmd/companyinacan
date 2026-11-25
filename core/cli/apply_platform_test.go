package cli

import (
	"context"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func TestSyncPlatformStoresPoliciesAndChannelsAndRejectsBadPolicies(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	c := &config.Company{
		Approvals:     map[string]config.ApprovalPolicy{"expense": {Steps: []config.ApprovalStep{{Name: "fin", Approvers: []config.Approver{{Role: "finance"}}}}}},
		Notifications: config.NotificationConfig{Webhooks: map[string]string{"ops": "https://hooks.example.com/x"}},
	}
	if err := syncPlatform(ctx, db, c); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM approval_policies) + (SELECT COUNT(*) FROM notify_channels)`).Scan(&n)
	if n != 2 {
		t.Fatalf("rows = %d", n)
	}
	c.Approvals["expense"] = config.ApprovalPolicy{}
	if err := syncPlatform(ctx, db, c); err == nil {
		t.Fatal("an invalid policy was accepted")
	}
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approval_policies`).Scan(&n)
	if n != 1 {
		t.Fatal("a rejected sync must leave the previous policies in place")
	}
}
