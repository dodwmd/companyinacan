package corecli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// secretsCmd rotates the secrets can-core generated for a module. It only records the request (in
// the state database, audited); the reconciler regenerates the value on its next pass and rolls
// the module. No value is ever printed or logged.
func secretsCmd(getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{Use: "secrets", Short: "Secrets can-core generated for modules"}
	rot := &cobra.Command{
		Use: "rotate <module> <key>", Short: "Regenerate one generated secret of a module at the next reconcile", Args: cobra.ExactArgs(2),
		Long: "Keys: CAN_WEBHOOK_SECRET (the signing secret of can-core's webhooks to the module) or a module secret such as\n" +
			"CAN_WORK_ADMIN_TOKEN. `keyring` (people, jobs) adds a new key and keeps the old ones so sealed data still opens; run the\n" +
			"module's own `rotate-keys` afterwards to re-seal it. Keys whose replacement would silently damage data are refused.",
	}
	db := dbFlag(rot, getenv)
	rot.RunE = func(c *cobra.Command, args []string) error {
		d, err := openExisting(c.Context(), *db)
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()
		module, key := args[0], args[1]
		rec, err := d.GetModule(c.Context(), module)
		if errors.Is(err, store.ErrNotFound) {
			return fail("no installed module %q", module)
		} else if err != nil {
			return fail("%v", err)
		}
		if !modsecrets.Rotatable(module, key) {
			return fail("%s of module %s cannot be rotated: it is not one of the secrets can-core generates, or replacing it would damage data (the jobs hash key breaks duplicate detection)", key, module)
		}
		if err := d.RequestRotation(c.Context(), module, key, "cli"); err != nil {
			return fail("%v", err)
		}
		if _, err := audit.New(d).Append(c.Context(), audit.Entry{Actor: "cli", Action: "secret.rotate.request", Target: module, Detail: map[string]any{"key": key}}); err != nil {
			return fail("%v", err)
		}
		if _, held := reconcile.ParsePending(rec.Pending); held && rec.State == store.StateEnabled {
			return fail("NOT APPLIED: rotation of %s for %s is recorded, but an upgrade of %s is waiting for approval and no change reaches the module until then: the old value stays valid. Two ways out: %s. The rotation then happens with the upgrade, or when the module is enabled again.", key, module, module, reconcile.WaysOut)
		}
		fmt.Fprintf(c.OutOrStdout(), "Rotation of %s for %s recorded. It is applied at the next reconcile (within a minute) and the module is restarted with the new value.\n", key, module)
		return nil
	}
	cmd.AddCommand(rot)
	return cmd
}
