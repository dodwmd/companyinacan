package cli

import "github.com/spf13/cobra"

func newStubCmd(use, short, epic, epicName string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short + " (not implemented yet)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return &exitError{ExitNotImplemented, "can " + use + " is not implemented yet (planned in epic " + epic + ": " + epicName + ")"}
		},
	}
}
