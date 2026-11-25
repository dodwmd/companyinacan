// Package cli implements the `can` command line.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Exit codes (contracts/cli.md).
const (
	ExitOK             = 0
	ExitFailure        = 1
	ExitUsage          = 2
	ExitNotImplemented = 3
)

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func failure(format string, a ...any) error {
	return &exitError{ExitFailure, fmt.Sprintf(format, a...)}
}
func usage(format string, a ...any) error {
	return &exitError{ExitUsage, fmt.Sprintf(format, a...)}
}

// silent fails with a code but prints nothing extra (the command already reported).
func silent(code int) error { return &exitError{code, ""} }

type globalOpts struct {
	output  string
	noColor bool
}

func (g *globalOpts) json() bool { return g.output == "json" }

// Execute runs the CLI and returns the process exit code.
func Execute(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	g := &globalOpts{}
	root := &cobra.Command{
		Use:           "can",
		Short:         "Company in a Can — manage a company's online presence",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	root.PersistentFlags().StringVar(&g.output, "output", "text", "output format: text or json")
	root.PersistentFlags().BoolVar(&g.noColor, "no-color", false, "disable colored output")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usage("%s", err) })
	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		if g.output != "text" && g.output != "json" {
			return usage("invalid --output %q: want text or json", g.output)
		}
		return nil
	}
	root.AddCommand(
		newVersionCmd(g),
		newInitCmd(g),
		newValidateCmd(g),
		newDoctorCmd(g),
		newApplyCmd(),
		newStubCmd("dev", "Create a local k3s company cluster", "E02", "local dev cluster"),
		newStubCmd("up", "Provision a server and deploy the company", "E06", "server providers and make up"),
		newLicencesCmd(g),
	)
	err := root.Execute()
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(stderr, "error:", ee.msg)
		}
		return ee.code
	}
	fmt.Fprintln(stderr, "error:", err)
	if strings.HasPrefix(err.Error(), "unknown command") || strings.HasPrefix(err.Error(), "unknown flag") || strings.Contains(err.Error(), "accepts") {
		return ExitUsage
	}
	return ExitFailure
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644) //nolint:gosec // the notice is a public file
}
