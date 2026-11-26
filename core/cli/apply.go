package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
	"github.com/dodwmd/companyinacan/core/server/store"
)

func newApplyCmd(g *globalOpts) *cobra.Command {
	var modulesDir, statePath string
	var dryRun, recordOnly, planOnly, skipImageCheck, forceCompany bool
	var approve []string
	var kubeContext string
	cmd := &cobra.Command{
		Use:   "apply [dir]",
		Short: "Reconcile company.yaml with the recorded module state",
		Long: "Validates company.yaml and the manifests of its enabled modules, compares them with the state\n" +
			"recorded in can-core's SQLite store and plans the install/upgrade/enable/disable steps.\n\n" +
			"Without flags it syncs company.yaml into the cluster (ConfigMap can-system/company) and can-core, which runs\n" +
			"in the cluster, reconciles it. Use --dry-run to print the plan, or --record-only to update the recorded state\n" +
			"without touching a cluster (development).",
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
			// Warnings (a module not ready, a backup hook that cannot run) are shown at enable time.
			for _, p := range ps {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", p.String())
			}
			if !dryRun && !recordOnly {
				// Before anything changes: can the cluster get every enabled module's images?
				if !skipImageCheck && !planOnly {
					if err := applyImageCheck(cmd, resolveModulesDir(modulesDir, dir), kubeContext, desired.Company); err != nil {
						return failure("%v", err)
					}
				}
				// Not over a change made in the admin UI (or by anyone else) since this directory saw it.
				cc, err := readClusterCompany(cmd, kubeContext)
				if err != nil {
					return failure("%v", err)
				}
				local, err := os.ReadFile(filepath.Join(dir, "company.yaml")) //nolint:gosec // validated above
				if err != nil {
					return failure("%v", err)
				}
				if !forceCompany {
					if err := overwriteRefusal(dir, cc, local); err != nil {
						if !planOnly {
							return failure("%v", err)
						}
						fmt.Fprintln(cmd.ErrOrStderr(), "warning:", err)
					}
				}
				expect := cc.ResourceVersion
				if forceCompany {
					expect = ""
				}
				// What can-core would do, before anything changes; approvals are recorded here.
				notApplied, err := clusterPreview(cmd, filepath.Join(dir, "company.yaml"), kubeContext, approve, planOnly)
				if err != nil {
					return failure("%v", err)
				}
				if planOnly {
					return nil
				}
				// The routing facts first: can-core reads them when it sees the new company.yaml.
				proxied, _ := desired.Company.DNS["proxied"].(bool)
				state, err := provider.NewStore(dir).Load() // what `can up` remembered here, if it ran here
				if err != nil {
					return failure("%v", err)
				}
				note, err := syncRouting(cmd, desired.Company.Domain, kubeContext, proxied, state.NoEdgeLimits)
				if err != nil {
					return failure("%v", err)
				}
				if note != "" {
					fmt.Fprintln(cmd.OutOrStdout(), note)
				}
				// What the change does to how modules behave, before it is made.
				for _, n := range reconcile.ModeNotes(companyInForce(cmd, kubeContext), desired.Company) {
					fmt.Fprintln(cmd.OutOrStdout(), "note:", n)
				}
				out, err := syncCompany(cmd, filepath.Join(dir, "company.yaml"), kubeContext, expect)
				if err != nil {
					return failure("%v", err)
				}
				// What the cluster holds now, for the next apply to compare with.
				if after, err := readClusterCompany(cmd, kubeContext); err == nil && after.Found {
					if err := recordBase(dir, after.UID, []byte(after.Data)); err != nil {
						fmt.Fprintln(cmd.ErrOrStderr(), "warning: could not record what was applied:", err)
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "synced company.yaml to ConfigMap can-system/company (%s)\ncan-core applies it within its reconcile interval; watch with: kubectl -n can-system logs deploy/can-core\n", out)
				if notApplied != nil {
					return failure("%v", notApplied)
				}
				return nil
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
			r.RequireApproval = true
			if len(approve) > 0 && !dryRun {
				planned, err := r.Plan(cmd.Context(), desired)
				if err != nil {
					return failure("%v", err)
				}
				codes, err := approvalCodes(planned, approve)
				if err != nil {
					return failure("%v", err)
				}
				for id, code := range codes {
					if err := db.ApproveUpgrade(cmd.Context(), id, code); err != nil {
						return failure("%v", err)
					}
				}
			}
			res, applyErr := r.Apply(cmd.Context(), desired, reconcile.Options{DryRun: dryRun})
			if applyErr == nil && !dryRun {
				if err := syncPlatform(cmd.Context(), db, desired.Company); err != nil {
					applyErr = fmt.Errorf("approval policies or notification channels: %w", err)
				}
			}

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
				printActions(w, verb, res.Actions)
				printActions(w, verb, res.Held)
				if len(res.Actions)+len(res.Held) == 0 {
					fmt.Fprintln(w, "nothing to do: recorded state matches company.yaml")
				}
			}
			if applyErr == nil && !dryRun {
				applyErr = notAppliedError(res.Held)
			}
			if applyErr != nil {
				return failure("%v", applyErr)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "", "directory containing module manifests (default $CAN_MODULES_DIR, ./modules, else the ones built into can)")
	cmd.Flags().StringVar(&statePath, "state", "", "recorded-state database (default <dir>/.can/state.db)")
	cmd.Flags().BoolVar(&skipImageCheck, "skip-image-check", false, skipImageCheckHelp)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan and change nothing")
	cmd.Flags().BoolVar(&forceCompany, "force-company", false, "overwrite the company.yaml in the cluster even if it was changed there (admin UI) since this directory last applied or pulled it")
	cmd.Flags().BoolVar(&recordOnly, "record-only", false, "update recorded state without deploying (development)")
	cmd.Flags().BoolVar(&planOnly, "plan", false, "print what can-core in the cluster would do and change nothing")
	cmd.Flags().StringSliceVar(&approve, "approve", nil, "module=code of a waiting upgrade to approve, as the plan shows it")
	cmd.Flags().StringVar(&kubeContext, "context", os.Getenv("CAN_DEV_CONTEXT"), "kubectl context of the cluster to sync company.yaml to (default $CAN_DEV_CONTEXT, else the current context)")
	return cmd
}
