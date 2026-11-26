package corecli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/server/companyexport"
	"github.com/dodwmd/companyinacan/core/server/runtime"
)

// exportCmd is what `can export` runs with kubectl exec: it asks the running can-core, over its
// Unix socket, for an export and writes the archive to standard output.
func exportCmd(getenv func(string) string, stdout, stderr io.Writer) *cobra.Command {
	var o companyexport.Options
	cmd := &cobra.Command{
		Use: "export", Short: "Write an export of the company's data (tar.gz) to standard output", Args: cobra.NoArgs,
		Long: "Streams the export from the running can-core. Used by `can export`, which also checks and encrypts it.\n" +
			"Nothing is stored on the server.",
	}
	db := dbFlag(cmd, getenv)
	cmd.Flags().BoolVar(&o.IncludeSealed, "include-sealed", false, "decrypt the sealed fields into the export")
	cmd.Flags().StringArrayVar(&o.Only, "module", nil, "export only this module (repeatable; `core` is can-core's own data)")
	cmd.Flags().StringArrayVar(&o.Skip, "skip", nil, "leave this module out (repeatable)")
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if o.IncludeSealed {
			fmt.Fprintln(stderr, "warning: this export holds the sealed fields in clear")
		}
		if err := companyexport.Fetch(ctx, runtime.ExportSocket(*db), o, stdout); err != nil {
			return fail("%v", err)
		}
		return nil
	}
	return cmd
}
