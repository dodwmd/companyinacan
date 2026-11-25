package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/footprint"
	"github.com/dodwmd/companyinacan/core/problem"
)

func newFootprintCmd(g *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "footprint",
		Short: "Idle-RAM measurement helpers (report, check); see dev/footprint",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newFootprintReportCmd(), newFootprintCheckCmd(g))
	return cmd
}

func newFootprintReportCmd() *cobra.Command {
	var podsFile, samplesDir, topFile, label string
	c := &cobra.Command{
		Use:   "report",
		Short: "Aggregate raw node samples (or kubectl top output) into a JSON report on stdout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var rep footprint.Report
			switch {
			case topFile != "" && samplesDir == "":
				b, err := os.ReadFile(topFile)
				if err != nil {
					return failure("%v", err)
				}
				rows, err := footprint.ParseTop(string(b))
				if err != nil {
					return failure("%v", err)
				}
				rep = footprint.FromTop(label, rows)
			case samplesDir != "" && podsFile != "" && topFile == "":
				pb, err := os.ReadFile(podsFile)
				if err != nil {
					return failure("%v", err)
				}
				pods, err := footprint.ParsePods(pb)
				if err != nil {
					return failure("%v", err)
				}
				files, err := filepath.Glob(filepath.Join(samplesDir, "*.txt"))
				if err != nil || len(files) == 0 {
					return failure("no *.txt samples in %s", samplesDir)
				}
				sort.Strings(files)
				var raws []footprint.Raw
				for _, f := range files {
					b, err := os.ReadFile(f) //nolint:gosec // operator-supplied sample directory
					if err != nil {
						return failure("%v", err)
					}
					r, err := footprint.ParseRaw(string(b))
					if err != nil {
						return failure("%s: %v", f, err)
					}
					raws = append(raws, r)
				}
				if rep, err = footprint.Build(label, pods, raws); err != nil {
					return failure("%v", err)
				}
			default:
				return usage("give either --samples DIR with --pods FILE, or --top FILE")
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(rep)
		},
	}
	f := c.Flags()
	f.StringVar(&podsFile, "pods", "", "kubectl get pods -A -o json output")
	f.StringVar(&samplesDir, "samples", "", "directory of raw sample files (*.txt) written by dev/footprint/sample.sh")
	f.StringVar(&topFile, "top", "", "kubectl top pods -A --no-headers output (clusters with metrics-server)")
	f.StringVar(&label, "label", "", "free-text label stored in the report")
	return c
}

func newFootprintCheckCmd(g *globalOpts) *cobra.Command {
	var reportFile, moduleFile, ns, prefix string
	var ramMB int
	var tol float64
	c := &cobra.Command{
		Use:   "check",
		Short: "Fail when measured idle RAM exceeds the module's declared ram_mb by more than the tolerance",
		Long: "Compares the summed median working set of the pods in --namespace (optionally with a name\n" +
			"prefix) against ram_mb x (1 + tolerance). The declared figure comes from --module (a\n" +
			"module.yaml, resources.ram_mb) or --ram-mb. Constitution II sets the tolerance at 20%.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if reportFile == "" || ns == "" || (moduleFile == "") == (ramMB == 0) {
				return usage("need --report, --namespace and exactly one of --module or --ram-mb")
			}
			declared := ramMB
			if moduleFile != "" {
				doc, ps := config.Load(moduleFile)
				if doc == nil {
					return printProblemsThenFail(cmd, g, ps)
				}
				m, err := config.DecodeModule(doc)
				if err != nil {
					return failure("%s: %v", moduleFile, err)
				}
				declared = m.Resources.RAMMB
			}
			b, err := os.ReadFile(reportFile) //nolint:gosec // operator-supplied report
			if err != nil {
				return failure("%v", err)
			}
			var rep footprint.Report
			if err := json.Unmarshal(b, &rep); err != nil {
				return failure("%s: %v", reportFile, err)
			}
			res, err := footprint.Check(rep, footprint.Selector{Namespace: ns, NamePrefix: prefix}, declared, tol)
			if err != nil {
				return failure("%v", err)
			}
			if g.json() {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(res); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), res.Message)
			}
			if !res.Pass {
				return silent(ExitFailure)
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&reportFile, "report", "", "JSON report from `can footprint report`")
	f.StringVar(&moduleFile, "module", "", "module.yaml whose resources.ram_mb is the declared figure")
	f.IntVar(&ramMB, "ram-mb", 0, "declared idle RAM in MB (alternative to --module)")
	f.StringVar(&ns, "namespace", "", "namespace holding the workload")
	f.StringVar(&prefix, "pod-prefix", "", "only pods whose name starts with this")
	f.Float64Var(&tol, "tolerance", footprint.DefaultTolerance, "allowed excess over the declared figure (0.2 = 20%)")
	return c
}

func printProblemsThenFail(cmd *cobra.Command, g *globalOpts, ps []problem.Problem) error {
	if err := printProblems(cmd, g, ps); err != nil {
		return err
	}
	return silent(ExitFailure)
}
