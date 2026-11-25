package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/licence"
	"github.com/dodwmd/companyinacan/core/problem"
)

func newLicencesCmd(g *globalOpts) *cobra.Command {
	var modulesDir, file, accepted string
	var write, check bool
	cmd := &cobra.Command{
		Use:   "licences",
		Short: "Check wrapped-app licences and generate THIRD_PARTY_LICENSES",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if write && check {
				return usage("--write and --check are mutually exclusive")
			}
			acc := licence.DefaultAccepted()
			if accepted != "" {
				var err error
				if acc, err = licence.LoadAccepted(accepted); err != nil {
					return failure("accepted-licences file: %v", err)
				}
			}
			entries, ps := licence.Collect(modulesDir, acc)
			notice := licence.Notice(entries)
			if check && !problem.HasErrors(ps) {
				ps = append(ps, licence.CheckNotice(file, notice)...)
			}
			if err := printProblems(cmd, g, ps); err != nil {
				return err
			}
			if problem.HasErrors(ps) {
				return silent(ExitFailure)
			}
			if write {
				if err := writeFile(file, notice); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%d third-party module(s))\n", file, len(entries))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "modules", "directory containing module manifests")
	cmd.Flags().StringVar(&file, "file", "THIRD_PARTY_LICENSES", "notice file to write or check")
	cmd.Flags().StringVar(&accepted, "accepted", "", "alternative accepted-licences file (default: embedded schemas/accepted-licences.yaml)")
	cmd.Flags().BoolVar(&write, "write", false, "regenerate the notice file")
	cmd.Flags().BoolVar(&check, "check", false, "fail if the notice file differs from the generated one (CI mode)")
	return cmd
}
