package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/doctor"
)

// newExecutor builds the command runner; tests replace it with a fake.
var newExecutor = func(cmd *cobra.Command) cluster.Executor {
	return cluster.ExecExecutor{Stderr: cmd.ErrOrStderr()}
}

type devOpts struct {
	cfg    cluster.Config
	dryRun bool
}

func newDevCmd(g *globalOpts) *cobra.Command {
	o := &devOpts{cfg: cluster.DefaultConfig()}
	dev := &cobra.Command{
		Use:   "dev",
		Short: "Manage the local k3d company cluster (up, down, status, doctor)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return usage("can dev needs a subcommand: up, down, status or doctor")
		},
	}
	f := dev.PersistentFlags()
	f.StringVar(&o.cfg.Name, "name", o.cfg.Name, "k3d cluster name")
	f.StringVar(&o.cfg.K3dConfig, "k3d-config", o.cfg.K3dConfig, "k3d cluster config file")
	f.StringVar(&o.cfg.ManifestsDir, "manifests", o.cfg.ManifestsDir, "kustomize directory with the base manifests")
	dev.AddCommand(newDevUpCmd(o), newDevDownCmd(o), newDevStatusCmd(g, o), newDevDoctorCmd(g))
	return dev
}

func devContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(cmd.Context(), os.Interrupt)
}

func printPlan(cmd *cobra.Command, p cluster.Plan) {
	for _, l := range p.Lines() {
		fmt.Fprintln(cmd.OutOrStdout(), l)
	}
}

func newDevUpCmd(o *devOpts) *cobra.Command {
	c := &cobra.Command{
		Use:   "up",
		Short: "Create the local cluster (if needed) and apply the base manifests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.cfg.Validate(); err != nil {
				return usage("%s", err)
			}
			if o.dryRun {
				printPlan(cmd, o.cfg.CreatePlan())
				return nil
			}
			if miss := cluster.Prerequisites(lookPath); len(miss) > 0 {
				for _, m := range miss {
					fmt.Fprintf(cmd.ErrOrStderr(), "missing %s: %s\n", m.Tool, m.Remedy)
				}
				return failure("prerequisites missing; run `can dev doctor`")
			}
			for _, p := range []string{o.cfg.K3dConfig, o.cfg.ManifestsDir} {
				if _, err := os.Stat(p); err != nil {
					return failure("%s not found (run from the repository root, or pass --k3d-config/--manifests)", p)
				}
			}
			ctx, stop := devContext(cmd)
			defer stop()
			if err := cluster.New(o.cfg, newExecutor(cmd)).Up(ctx); err != nil {
				return failure("%s", err)
			}
			h, _ := cluster.URL("admin", false)
			fmt.Fprintf(cmd.OutOrStdout(), "cluster %s is up (context k3d-%s); hostnames: *.%s, e.g. %s\n", o.cfg.Name, o.cfg.Name, cluster.Domain, h)
			return nil
		},
	}
	c.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the commands that would run and exit without running them")
	return c
}

func newDevDownCmd(o *devOpts) *cobra.Command {
	c := &cobra.Command{
		Use:   "down",
		Short: "Delete the local cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.cfg.Validate(); err != nil {
				return usage("%s", err)
			}
			if o.dryRun {
				printPlan(cmd, o.cfg.DeletePlan())
				return nil
			}
			if miss := cluster.Prerequisites(lookPath); len(miss) > 0 {
				return failure("prerequisites missing; run `can dev doctor`")
			}
			ctx, stop := devContext(cmd)
			defer stop()
			if err := cluster.New(o.cfg, newExecutor(cmd)).Down(ctx); err != nil {
				return failure("%s", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cluster %s deleted\n", o.cfg.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the commands that would run and exit without running them")
	return c
}

func newDevStatusCmd(g *globalOpts, o *devOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the local cluster is running (exit 1 if not)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.cfg.Validate(); err != nil {
				return usage("%s", err)
			}
			if miss := cluster.Prerequisites(lookPath); len(miss) > 0 {
				return failure("prerequisites missing; run `can dev doctor`")
			}
			ctx, stop := devContext(cmd)
			defer stop()
			st, err := cluster.New(o.cfg, newExecutor(cmd)).Status(ctx)
			if err != nil {
				return failure("%s", err)
			}
			w := cmd.OutOrStdout()
			if g.json() {
				if err := json.NewEncoder(w).Encode(map[string]any{"name": o.cfg.Name, "exists": st.Exists, "running": st.Running,
					"servers": st.Servers, "serversRunning": st.ServersRunning}); err != nil {
					return err
				}
			} else {
				state := "absent"
				switch {
				case st.Running:
					state = "running"
				case st.Exists:
					state = "stopped"
				}
				fmt.Fprintf(w, "%s: %s\n", o.cfg.Name, state)
			}
			if !st.Running {
				return silent(ExitFailure)
			}
			return nil
		},
	}
}

func newDevDoctorCmd(g *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the prerequisites for the local cluster (docker or podman, k3d, kubectl)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			miss := cluster.Prerequisites(lookPath)
			w := cmd.OutOrStdout()
			if g.json() {
				if miss == nil {
					miss = []doctor.Result{}
				}
				if err := json.NewEncoder(w).Encode(miss); err != nil {
					return err
				}
			} else if len(miss) == 0 {
				fmt.Fprintln(w, "ok: docker or podman, k3d and kubectl found")
			} else {
				for _, m := range miss {
					fmt.Fprintf(w, "missing %-17s %s\n", m.Tool, m.Remedy)
				}
			}
			if len(miss) > 0 {
				return silent(ExitFailure)
			}
			return nil
		},
	}
}
