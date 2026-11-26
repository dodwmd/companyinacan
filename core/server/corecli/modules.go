package corecli

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/runtime"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/spf13/cobra"
)

// moduleProblem is one line of `can-core modules problems`; `can status` reads it.
type moduleProblem struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Error string `json:"error"`
}

const statePendingApproval = "pending-approval"

var approvalRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// modulesPlanCmd is the preview of `can apply`: what this can-core would do with a company.yaml.
func modulesPlanCmd(getenv func(string) string) *cobra.Command {
	var company string
	cmd := &cobra.Command{
		Use: "plan", Short: "Print what applying a company.yaml would do, as JSON; change nothing", Args: cobra.NoArgs,
	}
	db := dbFlag(cmd, getenv)
	cmd.Flags().StringVar(&company, "company-b64", "", "company.yaml, base64")
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		data, err := base64.StdEncoding.DecodeString(company)
		if err != nil || len(data) == 0 {
			return fail("--company-b64 must hold company.yaml, base64")
		}
		d, err := openExisting(c.Context(), *db)
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()
		actions, err := runtime.PlanModules(c.Context(), getenv, d, data)
		if err != nil {
			return fail("%v", err)
		}
		if actions == nil {
			actions = []reconcile.Action{}
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(reconcile.Result{Actions: actions})
	}
	return cmd
}

func modulesApproveCmd(getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{
		Use: "approve <module> <approval>", Short: "Approve an upgrade that widens what a module may do", Args: cobra.ExactArgs(2),
	}
	db := dbFlag(cmd, getenv)
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if !approvalRE.MatchString(args[1]) {
			return fail("not an approval code: take it from `can-core modules plan` or the admin UI")
		}
		d, err := openExisting(c.Context(), *db)
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()
		if _, err := audit.New(d).Append(c.Context(), audit.Entry{Actor: "cli", Action: "module.upgrade-approve", Target: args[0], Detail: map[string]any{"approval": args[1]}}); err != nil {
			return fail("%v", err)
		}
		if err := d.ApproveUpgrade(c.Context(), args[0], args[1]); err != nil {
			return fail("%s: %v", args[0], err)
		}
		return nil
	}
	return cmd
}

func modulesCmd(getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{Use: "modules", Short: "Installed modules"}
	problems := &cobra.Command{
		Use: "problems", Short: "List modules whose last install or upgrade failed, as JSON", Args: cobra.NoArgs,
	}
	db := dbFlag(problems, getenv)
	problems.RunE = func(c *cobra.Command, _ []string) error {
		d, err := openExisting(c.Context(), *db)
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()
		mods, err := d.ListModules(c.Context())
		if err != nil {
			return fail("%v", err)
		}
		out := []moduleProblem{}
		for _, m := range mods {
			// A failed upgrade leaves the previous release enabled, with the error.
			if m.State == store.StateFailed || m.LastError != "" {
				out = append(out, moduleProblem{m.ID, m.State, modmgr.Sanitise(m.LastError)})
			}
		}
		for _, m := range mods {
			if p, ok := reconcile.ParsePending(m.Pending); ok && m.State == store.StateEnabled {
				out = append(out, moduleProblem{m.ID, statePendingApproval, modmgr.Sanitise(strings.Join(p.Widens, "; "))})
			}
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(out)
	}
	cmd.AddCommand(problems, modulesPlanCmd(getenv), modulesApproveCmd(getenv))
	return cmd
}
