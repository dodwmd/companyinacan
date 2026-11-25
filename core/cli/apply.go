package cli

import "github.com/spf13/cobra"

func newApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Reconcile company.yaml with the cluster (not implemented yet)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return &exitError{ExitNotImplemented, "can apply is not implemented yet (planned in epic E03: can-core MVP)"}
		},
	}
}
