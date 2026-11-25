package cli

import (
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/doctor"
)

var lookPath = exec.LookPath

func newDoctorCmd(g *globalOpts) *cobra.Command {
	var server bool
	var companyDir, modulesDir string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check local prerequisites, or with --server the server's pod-to-node isolation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res []doctor.Result
			if server {
				res = []doctor.Result{serverCheck(cmd.Context(), cmd, g, companyDir, modulesDir)}
			} else {
				res = append(doctor.Check(lookPath), doctor.Reach(cmd.Context(), lookPath, reachRunner)...)
			}
			w := cmd.OutOrStdout()
			if g.json() {
				if err := json.NewEncoder(w).Encode(res); err != nil {
					return err
				}
			} else {
				for _, r := range res {
					line := fmt.Sprintf("%-17s %-17s %s", r.Status, r.Tool, r.Path)
					if r.Remedy != "" {
						line = fmt.Sprintf("%-17s %-17s %s", r.Status, r.Tool, r.Remedy)
					}
					fmt.Fprintln(w, line)
				}
			}
			if doctor.HasMissingRequired(res) {
				return silent(ExitFailure)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&server, "server", false, "check, over ssh, that pods cannot reach the server's node services (read-only; exit 1 if not)")
	cmd.Flags().StringVar(&companyDir, "company-dir", ".", "with --server: directory holding company.yaml and .can/")
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "", "with --server: modules directory (default: ./modules)")
	return cmd
}
