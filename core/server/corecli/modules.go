package corecli

import (
	"encoding/json"

	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/spf13/cobra"
)

// moduleProblem is one line of `can-core modules problems`; `can status` reads it.
type moduleProblem struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Error string `json:"error"`
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
		return json.NewEncoder(c.OutOrStdout()).Encode(out)
	}
	cmd.AddCommand(problems)
	return cmd
}
