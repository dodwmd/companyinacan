package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/validate"
	"github.com/dodwmd/companyinacan/modules"
)

// A release archive and `go install` have no modules directory.
func init() { validate.UseShipped(modules.Manifests) }

// resolveModulesDir picks the modules directory: flag, $CAN_MODULES_DIR, <dir>/modules, ./modules,
// else the manifests built into can. A flag or the variable is taken as given; a directory found by
// its name only counts when it holds a module (a project may keep other things in modules/).
func resolveModulesDir(flag, dir string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv("CAN_MODULES_DIR"); env != "" {
		return env
	}
	for _, c := range []string{filepath.Join(dir, "modules"), "modules"} {
		if holdsModules(c) {
			return c
		}
	}
	return validate.BuiltinModules
}

func holdsModules(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "module.yaml")); err == nil {
			return true
		}
	}
	return false
}

func printProblems(cmd *cobra.Command, g *globalOpts, ps []problem.Problem) error {
	w := cmd.OutOrStdout()
	if g.json() {
		if ps == nil {
			ps = []problem.Problem{}
		}
		return json.NewEncoder(w).Encode(ps)
	}
	errs, warns := 0, 0
	for _, p := range ps {
		fmt.Fprintln(w, p.String())
		if p.Severity == problem.Error {
			errs++
		} else {
			warns++
		}
	}
	fmt.Fprintf(w, "%d error(s), %d warning(s)\n", errs, warns)
	return nil
}

func newValidateCmd(g *globalOpts) *cobra.Command {
	var modulesDir string
	cmd := &cobra.Command{
		Use:   "validate [dir]",
		Short: "Validate company.yaml and the manifests of its enabled modules",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			ps := validate.CompanyFile(filepath.Join(dir, "company.yaml"), resolveModulesDir(modulesDir, dir))
			if err := printProblems(cmd, g, ps); err != nil {
				return err
			}
			if problem.HasErrors(ps) {
				return silent(ExitFailure)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "", "directory containing module manifests (default $CAN_MODULES_DIR, ./modules, else the ones built into can)")
	return cmd
}
