package corecli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/server/restoreops"
	"github.com/dodwmd/companyinacan/core/server/runtime"
)

// restoreInput is what `can restore` sends on stdin.
type restoreInput struct {
	Accept       []string       `json:"accept,omitempty"`
	MissingBlobs []string       `json:"missing_blobs,omitempty"`
	Entries      []backup.Entry `json:"entries,omitempty"`
}

// restoreCmd is the in-cluster half of `can restore` (specs/042-restore), run with kubectl exec
// in the can-core pod. Each subcommand prints its report as one JSON line on stdout.
func restoreCmd(stdin io.Reader, getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{Use: "restore", Short: "Restore operations (run by `can restore`, only in restore mode)"}
	run := func(c *cobra.Command, f func(context.Context, *restoreops.Ops, restoreInput) (any, error), input bool) error {
		cfg, err := runtime.FromEnv(getenv)
		if err != nil {
			return fail("%v", err)
		}
		var in restoreInput
		if input {
			if err := json.NewDecoder(io.LimitReader(stdin, 32<<20)).Decode(&in); err != nil {
				return fail("input: %v", err)
			}
		}
		ctx := c.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		ops, done, err := cfg.RestoreOps(ctx, cfg.Logger(os.Stderr))
		if err != nil {
			return fail("%v", err)
		}
		defer done()
		rep, err := f(ctx, ops, in)
		if rep != nil {
			_ = json.NewEncoder(c.OutOrStdout()).Encode(rep)
		}
		if err != nil {
			return fail("%v", strings.TrimSpace(err.Error()))
		}
		return nil
	}
	var overwrite bool
	apply := &cobra.Command{Use: "apply", Short: "Re-create Secrets and modules with workloads at 0", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return run(c, func(ctx context.Context, o *restoreops.Ops, _ restoreInput) (any, error) {
				return o.Apply(ctx, overwrite)
			}, false)
		}}
	apply.Flags().BoolVar(&overwrite, "overwrite", false, "replace existing Secrets with the backed-up ones")
	cmd.AddCommand(apply,
		&cobra.Command{Use: "hooks", Short: "Run the module restore hooks", Args: cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return run(c, func(ctx context.Context, o *restoreops.Ops, in restoreInput) (any, error) {
					return o.RunHooks(ctx, backup.HookOptions{Accept: in.Accept, MissingBlobs: in.MissingBlobs})
				}, true)
			}},
		&cobra.Command{Use: "erase", Short: "Replay the erasure tombstones", Args: cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return run(c, func(ctx context.Context, o *restoreops.Ops, in restoreInput) (any, error) {
					return o.Erase(ctx, in.Entries, in.Accept)
				}, true)
			}},
		&cobra.Command{Use: "complete", Short: "Leave restore mode at the next start", Args: cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return run(c, func(ctx context.Context, o *restoreops.Ops, _ restoreInput) (any, error) { return o.Complete(ctx) }, false)
			}},
		&cobra.Command{Use: "status", Short: "Print whether can-core is in restore mode", Args: cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				cfg, err := runtime.FromEnv(getenv)
				if err != nil {
					return fail("%v", err)
				}
				s, err := restoreops.Status(cfg.DBPath)
				if err != nil {
					return fail("%v", err)
				}
				fmt.Fprintln(c.OutOrStdout(), s)
				return nil
			}})
	return cmd
}
