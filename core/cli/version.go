package cli

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Set at build time with -ldflags (see Makefile).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func newVersionCmd(g *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the can version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := map[string]string{
				"version": version, "commit": commit, "date": date,
				"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
			}
			if g.json() {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(info)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "can %s (%s, %s) %s %s/%s\n", version, commit, date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}
