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
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check local prerequisites",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res := doctor.Check(lookPath)
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
}
