package corecli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/modbackup"
)

// moduleDataCmd is what the dump and restore Jobs of the wrapped modules run, in this image, in
// the module's namespace (specs/046-wrapped-backups). It talks to can-core's module API with the
// pod's projected token (CAN_API_URL, CAN_MODULE) and reads no can-core state.
func moduleDataCmd(getenv func(string) string, stdout io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "module-data", Short: "Dump or restore a wrapped module's data (run in its hook Job)", Hidden: true}
	sub := func(use, short string, f func(context.Context, modbackup.Wrapped, func(string) string, io.Writer) error) *cobra.Command {
		var w modbackup.Wrapped
		c := &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				parent := c.Context()
				if parent == nil {
					parent = context.Background()
				}
				ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
				defer stop()
				limit := 14 * time.Minute // inside the Job's 15
				if c.Name() == "export" {
					limit = 55 * time.Minute // inside the export Job's hour
				}
				ctx, cancel := context.WithTimeout(ctx, limit)
				defer cancel()
				err := f(ctx, w, getenv, stdout)
				switch {
				case errors.Is(err, modbackup.ErrNoData):
					return &exitError{modbackup.ExitNoData, err.Error()}
				case err != nil:
					return fail("%v", err)
				}
				return nil
			}}
		c.Flags().StringVar(&w.DataDir, "data", "", "where the module's volume is mounted")
		c.Flags().StringVar(&w.SQLite, "sqlite", "", "SQLite database file on the volume")
		c.Flags().BoolVar(&w.Files, "files", false, "archive the whole volume")
		c.Flags().StringVar(&w.PGDump, "pgdump", "", "pg_dump archive: read by backup, written by restore")
		c.Flags().StringVar(&w.WorkDir, "work", "", "scratch directory on disk")
		return c
	}
	cmd.AddCommand(sub("backup", "Upload the module's dump set", modbackup.BackupWrapped),
		sub("restore", "Download, verify and install the module's dump set (restore mode)", modbackup.RestoreWrapped),
		sub("export", "Send the module's data to can export", modbackup.ExportWrapped))
	return cmd
}
