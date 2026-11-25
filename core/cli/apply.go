package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func newApplyCmd(g *globalOpts) *cobra.Command {
	var modulesDir, statePath string
	var dryRun, recordOnly bool
	cmd := &cobra.Command{
		Use:   "apply [dir]",
		Short: "Reconcile company.yaml with the recorded module state",
		Long: "Validates company.yaml and the manifests of its enabled modules, compares them with the state\n" +
			"recorded in can-core's SQLite store and plans the install/upgrade/enable/disable steps.\n\n" +
			"The Helm/Kubernetes installer is not implemented yet (epic E03 follow-up). Use --dry-run to print\n" +
			"the plan, or --record-only to update the recorded state without touching a cluster (development).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			desired, ps := reconcile.LoadDesired(filepath.Join(dir, "company.yaml"), resolveModulesDir(modulesDir, dir))
			if problem.HasErrors(ps) {
				if err := printProblems(cmd, g, ps); err != nil {
					return err
				}
				return silent(ExitFailure)
			}
			if !dryRun && !recordOnly {
				return &exitError{ExitNotImplemented, "can apply cannot deploy yet: the Helm installer is a follow-up to epic E03; use --dry-run to see the plan or --record-only to update recorded state"}
			}

			dbPath := statePath
			if dbPath == "" {
				dbPath = filepath.Join(dir, ".can", "state.db")
			}
			if _, err := os.Stat(dbPath); err != nil && dryRun {
				dbPath = ":memory:" // nothing recorded yet: plan against an empty state
			} else if dbPath != ":memory:" {
				if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
					return failure("%v", err)
				}
			}
			db, err := store.Open(cmd.Context(), dbPath)
			if err != nil {
				return failure("opening state %s: %v", dbPath, err)
			}
			defer func() { _ = db.Close() }()
			r := reconcile.New(db, registry.New(db, ""), audit.New(db), reconcile.RecordOnly{})
			res, applyErr := r.Apply(cmd.Context(), desired, reconcile.Options{DryRun: dryRun})

			w := cmd.OutOrStdout()
			if g.json() {
				if res.Actions == nil {
					res.Actions = []reconcile.Action{}
				}
				_ = json.NewEncoder(w).Encode(res)
			} else {
				verb := "applied"
				if dryRun {
					verb = "planned"
				}
				for _, a := range res.Actions {
					fmt.Fprintf(w, "%s %s: %s (%s)\n", verb, a.Kind, a.Module, a.Reason)
				}
				if len(res.Actions) == 0 {
					fmt.Fprintln(w, "nothing to do: recorded state matches company.yaml")
				}
			}
			if applyErr != nil {
				return failure("%v", applyErr)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "", "directory containing module manifests (default $CAN_MODULES_DIR, ./modules)")
	cmd.Flags().StringVar(&statePath, "state", "", "recorded-state database (default <dir>/.can/state.db)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan and change nothing")
	cmd.Flags().BoolVar(&recordOnly, "record-only", false, "update recorded state without deploying (development)")
	return cmd
}
