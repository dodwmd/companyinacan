package corecli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/server/installer"
)

// renderChartCmd is the hidden entry point can-core re-executes to render a module chart in a
// resource-limited child process (installer/child.go). It reads one JSON job on stdin and writes one
// JSON result on stdout; it is not meant to be run by hand.
func renderChartCmd(stdin io.Reader, stdout io.Writer) *cobra.Command {
	var asMB, heapMB int
	cmd := &cobra.Command{
		Use: "render-chart", Hidden: true, Args: cobra.NoArgs,
		Short: "internal: render a module chart in a sandboxed child process",
		RunE: func(*cobra.Command, []string) error {
			if code := installer.RunRenderChild(stdin, stdout, asMB, heapMB); code != 0 {
				return &exitError{code, ""}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&asMB, "as-mb", 0, "address-space limit in MiB (0 = none)")
	cmd.Flags().IntVar(&heapMB, "heap-mb", 0, "exit when the live heap exceeds this many MiB (0 = off)")
	return cmd
}
