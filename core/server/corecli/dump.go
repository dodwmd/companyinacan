package corecli

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/runtime"
)

// dumpRequestCmd asks the running can-core for a dump run now (kubectl exec deploy/can-core --
// /can-core dump-request). It only creates a file next to the state database, so it needs no
// API, token or network, and only someone who may exec into the pod can use it.
func dumpRequestCmd(getenv func(string) string) *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use: "dump-request", Short: "Ask the running can-core to produce backup dumps now", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := runtime.FromEnv(getenv)
			if err != nil {
				return fail("%v", err)
			}
			if cfg.DumpsDir == "" {
				return fail("CAN_DUMPS_DIR is not set: this can-core produces no dumps")
			}
			marker := filepath.Join(cfg.DumpsDir, backup.MarkerName)
			before, _ := backup.ReadMarker(marker)
			if err := dumps.Request(cfg.DumpRequestFile); err != nil {
				return fail("%v", err)
			}
			if wait <= 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "dump requested")
				return nil
			}
			deadline := time.Now().Add(wait)
			for {
				if m, err := backup.ReadMarker(marker); err == nil && m.RunID != before.RunID {
					fmt.Fprintf(cmd.OutOrStdout(), "dump run %s complete\n", m.RunID)
					if run, err := backup.ReadDumpRun(cfg.DumpsDir); err == nil {
						for _, s := range run.Skipped {
							fmt.Fprintln(cmd.OutOrStdout(), "WARNING:", s.Message())
						}
					}
					return nil
				}
				if time.Now().After(deadline) {
					return fail("no new completion marker after %s; see the can-core log", wait)
				}
				time.Sleep(50 * time.Millisecond)
			}
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 0, "wait until a new completion marker appears (fails after this long)")
	return cmd
}
