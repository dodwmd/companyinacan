package corecli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/rbac"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// rolesCmd is the break-glass path to roles: it works on the database directly (inside the pod),
// so an installation whose owners are locked out or removed can always recover. Every change is
// audited with actor "cli".
func rolesCmd(getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{Use: "roles", Short: "Role assignments (recovery and scripting)"}
	var db *string
	change := func(use, short string, grant bool) *cobra.Command {
		c := &cobra.Command{Use: use + " <username> <role>", Short: short, Args: cobra.ExactArgs(2)}
		db = dbFlag(c, getenv)
		scope := c.Flags().String("scope", "", "resource scope (empty = everything, or a prefix ending in /*)")
		path := db
		c.RunE = func(c *cobra.Command, args []string) error {
			d, err := openExisting(c.Context(), *path)
			if err != nil {
				return err
			}
			defer func() { _ = d.Close() }()
			if _, err := d.AdminByUsername(c.Context(), args[0]); err != nil {
				return fail("no admin %q", args[0])
			}
			s := rbac.New(d, audit.New(d), rbac.Options{BreakGlass: true})
			if grant {
				err = s.Grant(c.Context(), rbac.CLIActor, "user:"+args[0], args[1], *scope)
			} else {
				err = s.Revoke(c.Context(), rbac.CLIActor, "user:"+args[0], args[1], *scope)
			}
			switch {
			case errors.Is(err, rbac.ErrLastOwner):
				return fail("%v", err)
			case errors.Is(err, store.ErrNotFound):
				return fail("%s does not hold %s", args[0], args[1])
			case err != nil:
				return fail("%v", err)
			}
			fmt.Fprintf(c.OutOrStdout(), "%s %s %s.\n", map[bool]string{true: "Assigned", false: "Removed"}[grant], args[1], args[0])
			return nil
		}
		return c
	}
	list := &cobra.Command{Use: "list", Short: "List admin users and their roles", Args: cobra.NoArgs}
	ldb := dbFlag(list, getenv)
	list.RunE = func(c *cobra.Command, _ []string) error {
		d, err := openExisting(c.Context(), *ldb)
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()
		us, err := rbac.New(d, audit.New(d), rbac.Options{}).Users(c.Context(), 500)
		if err != nil {
			return fail("%v", err)
		}
		for _, u := range us {
			fmt.Fprintf(c.OutOrStdout(), "%s:", u.Username)
			for _, a := range u.Assignments {
				fmt.Fprintf(c.OutOrStdout(), " %s", a.Role)
				if a.Scope != "" {
					fmt.Fprintf(c.OutOrStdout(), "(%s)", a.Scope)
				}
			}
			fmt.Fprintln(c.OutOrStdout())
		}
		return nil
	}
	cmd.AddCommand(change("grant", "Assign a role (use `owner` for break-glass recovery)", true), change("revoke", "Remove a role", false), list)
	return cmd
}
