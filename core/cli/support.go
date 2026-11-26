package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/validate"
	"github.com/dodwmd/companyinacan/core/verify"
)

// `can support` hands the operator what `make up` cannot print: the support inbox is installed by
// can-core after `can up` has returned (specs/059-support-mailbox).

const supportTimeout = 90 * time.Second

// supportKubectl runs kubectl on this machine (--context). A variable so that tests substitute it.
var supportKubectl = func(ctx context.Context, args ...string) (string, error) {
	b, err := exec.CommandContext(ctx, "kubectl", args...).Output() //nolint:gosec // fixed arguments and the operator's context name
	return string(b), err
}

type supportFlags struct {
	context, modulesDir string
}

// run executes kubectl for the company in dir: on this machine with --context, otherwise with
// k3s kubectl on the server over ssh. The arguments are fixed words of this file.
func (f *supportFlags) run(cmd *cobra.Command, g *globalOpts, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), supportTimeout)
	defer cancel()
	if f.context != "" {
		return supportKubectl(ctx, append([]string{"--context", f.context}, args...)...)
	}
	t, err := load(cmd, g, dir, f.modulesDir)
	if err != nil {
		return "", err
	}
	tgt, err := provider.TargetFor(t.spec, t.store, t.deps)
	if err != nil {
		return "", fmt.Errorf("%s (for a cluster of your own, pass --context)", t.redact.Redact(err.Error()))
	}
	runner := t.deps.Runner
	if runner == nil {
		runner = func(tg provider.Target) provider.Runner { return provider.NewSSHRunner(tg) }
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return runner(tgt).Run(ctx, "k3s kubectl "+strings.Join(quoted, " ")+" 2>/dev/null")
}

func supportCompany(cmd *cobra.Command, g *globalOpts, dir string) (*config.Company, config.SupportMail, error) {
	var none config.SupportMail
	doc, ps := config.Load(dir + "/company.yaml")
	if doc == nil || problem.HasErrors(ps) {
		if err := printProblems(cmd, g, validate.CompanyFile(dir+"/company.yaml", validate.BuiltinModules)); err != nil {
			return nil, none, err
		}
		return nil, none, silent(ExitFailure)
	}
	c, err := config.DecodeCompany(doc)
	if err != nil {
		return nil, none, failure("company.yaml: %v", err)
	}
	m, _ := config.SupportMailOf(c)
	if !m.Enabled {
		return nil, none, failure("company.yaml does not enable the support module (modules.support)")
	}
	return c, m, nil
}

func newSupportCmd(g *globalOpts) *cobra.Command {
	f := &supportFlags{}
	root := &cobra.Command{
		Use:   "support",
		Short: "The support inbox: its administrator sign-in and the state of its mail",
		Long: "Reads from the running support module (docs/support.md). Without --context the server\n" +
			"recorded for the company directory is asked over ssh.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return usage("support needs a subcommand: credentials, status") },
	}
	root.PersistentFlags().StringVar(&f.context, "context", "", "use local kubectl with this kube context instead of ssh to the server")
	root.PersistentFlags().StringVar(&f.modulesDir, "modules-dir", "", "modules directory (default: ./modules, else the manifests built into can)")
	dirOf := func(args []string) string {
		if len(args) == 1 {
			return args[0]
		}
		return "."
	}

	var printSecrets bool
	creds := &cobra.Command{
		Use:   "credentials [company-dir]",
		Short: "Show the address, user and generated password of the first FreeScout administrator",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, m, err := supportCompany(cmd, g, dirOf(args))
			if err != nil {
				return err
			}
			show := printSecrets || (lifecycle.isTTY() && !g.json())
			if !show {
				return usage("this prints a password: run it in a terminal, or pass --print-secrets")
			}
			out, err := f.run(cmd, g, dirOf(args), "-n", "mod-support", "get", "secret", "can-module", "-o", "jsonpath={.data.CAN_SUPPORT_ADMIN_PASSWORD}")
			var exit *exitError
			if errors.As(err, &exit) {
				return err
			}
			pw, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(out))
			if err != nil || derr != nil || len(pw) == 0 || strings.IndexFunc(string(pw), unicode.IsControl) >= 0 {
				return failure("the password could not be read: is the support module installed? (kubectl -n mod-support get secret can-module)")
			}
			url := "https://support." + c.Domain
			if g.json() {
				return printJSON(cmd.OutOrStdout(), map[string]string{"url": url, "email": m.AdminEmail, "password": string(pw)})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "URL:      %s\nEmail:    %s\nPassword: %s\n", url, m.AdminEmail, pw)
			fmt.Fprintln(cmd.ErrOrStderr(), "Change the password after the first sign-in: this one stays in the cluster and is not updated.\n"+
				"The email is the one set when the module first started.")
			return nil
		},
	}
	creds.Flags().BoolVar(&printSecrets, "print-secrets", false, "print the password even when the output is not a terminal")

	status := &cobra.Command{
		Use:   "status [company-dir]",
		Short: "Check the mailbox configuration, mail fetch and mail send now; exit 1 when something fails",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, err := supportCompany(cmd, g, dirOf(args)); err != nil {
				return err
			}
			// The command exits 1 when something is failing, with its report on standard output.
			out, err := f.run(cmd, g, dirOf(args), "-n", "mod-support", "exec", "deploy/support", "-c", "app", "--", "php", "/tmp/can-support.php", "status")
			var exit *exitError
			if errors.As(err, &exit) {
				return err
			}
			s, ok := verify.ParseMailStatus(out)
			if !ok {
				return failure("the support module did not report: is it running? (kubectl -n mod-support get pods)")
			}
			if g.json() {
				if err := printJSON(cmd.OutOrStdout(), s); err != nil {
					return err
				}
			} else {
				for _, p := range []struct {
					name string
					part verify.MailPart
				}{{"config", s.Config}, {"fetch", s.Fetch}, {"send", s.Send}} {
					line := fmt.Sprintf("%-7s %s", p.name+":", strings.ReplaceAll(p.part.State, "_", " "))
					if p.part.Detail != "" {
						line += " (" + p.part.Detail + ")"
					}
					fmt.Fprintln(cmd.OutOrStdout(), line)
				}
			}
			if s.Failing() {
				return silent(ExitFailure)
			}
			return nil
		},
	}
	root.AddCommand(creds, status)
	return root
}
