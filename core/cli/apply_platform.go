package cli

import (
	"context"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/approvals"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/notify"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// syncPlatform records the approval policies and outbound notification channels of company.yaml in
// the state database (epic E11a). It runs after a successful, non-dry-run `can apply`; the policies
// are validated again (the schema cannot express everything) and replace the stored ones.
func syncPlatform(ctx context.Context, db *store.DB, c *config.Company) error {
	if err := approvals.New(db, audit.New(db), approvals.Deps{}).SyncPolicies(ctx, c.Approvals); err != nil {
		return err
	}
	return notify.New(db, notify.Options{}).SetChannels(ctx, c.Notifications.Webhooks)
}
