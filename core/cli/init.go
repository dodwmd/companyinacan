package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
	"golang.org/x/term"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/schema"
)

// stdinIsTerminal reports whether r is an interactive terminal (overridable in tests).
var stdinIsTerminal = func(r any) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd())) //nolint:gosec // file descriptors fit in int
}

type newCompany struct {
	Schema   int    `yaml:"schema"`
	Name     string `yaml:"name"`
	Domain   string `yaml:"domain"`
	Provider struct {
		Type string `yaml:"type"`
	} `yaml:"provider"`
}

func newInitCmd(_ *globalOpts) *cobra.Command {
	var name, domain, providerType string
	var force, nonInteractive bool
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Create a company.yaml in dir (default .)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			target := filepath.Join(dir, "company.yaml")
			if _, err := os.Stat(target); err == nil && !force {
				return failure("%s already exists; use --force to overwrite", target)
			}
			interactive := !nonInteractive && stdinIsTerminal(cmd.InOrStdin())
			if interactive {
				in := bufio.NewReader(cmd.InOrStdin())
				out := cmd.OutOrStdout()
				name = ask(in, out, "Company name", name)
				domain = ask(in, out, "Domain (e.g. example.com)", domain)
				providerType = ask(in, out, "Provider (local, ssh, hetzner, digitalocean, static)", providerType)
			}
			var missing []string
			if strings.TrimSpace(name) == "" {
				missing = append(missing, "--name")
			}
			if strings.TrimSpace(domain) == "" {
				missing = append(missing, "--domain")
			}
			if len(missing) > 0 {
				return usage("missing %s (required when not running interactively)", strings.Join(missing, ", "))
			}
			var c newCompany
			c.Schema, c.Name, c.Domain = 1, strings.TrimSpace(name), strings.TrimSpace(domain)
			c.Provider.Type = providerType
			body, err := yaml.Marshal(c)
			if err != nil {
				return err
			}
			doc, ps := config.LoadBytes(target, body)
			if doc != nil {
				ps = schema.Validate(schema.Company, doc)
			}
			if problem.HasErrors(ps) {
				var b strings.Builder
				for _, p := range ps {
					fmt.Fprintf(&b, "\n  %s: %s", strings.TrimPrefix(p.Path, "/"), p.Message)
				}
				return usage("invalid value:%s", b.String())
			}
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return err
			}
			content := append([]byte("# Company description. Validate with `can validate`.\n"), body...)
			// A static host runs no forms gateway, so it gets no contact form.
			if providerType != "static" {
				content = append(content, fmt.Sprintf(initContactForm, c.Domain)...)
				if doc, ps := config.LoadBytes(target, content); doc == nil || problem.HasErrors(append(ps, schema.Validate(schema.Company, doc)...)) {
					return failure("the generated company.yaml is invalid; this is a bug")
				}
			}
			if err := os.WriteFile(target, content, 0o600); err != nil {
				return err
			}
			gi := filepath.Join(dir, ".gitignore")
			if _, err := os.Stat(gi); os.IsNotExist(err) {
				if err := os.WriteFile(gi, []byte(".can/\n"), 0o600); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", target)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "company name")
	cmd.Flags().StringVar(&domain, "domain", "", "company domain, e.g. example.com")
	cmd.Flags().StringVar(&providerType, "provider", "local", "provider: local, ssh, hetzner, digitalocean, static")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing company.yaml")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "never prompt; require flags")
	return cmd
}

// initContactForm is the working contact form of a new company (docs/site-contact.md): the forms
// module and the section that links to it. Mail needs a relay, which only the operator knows, so
// that part is commented out. %[1]s is the company domain.
const initContactForm = `modules:
  forms: {}
  # To be emailed about every new message, use this line instead of the one above, and set the relay
  # under email: below.
  # forms: { settings: { notify_email: [hello@%[1]s] } }
# email:
#   relay: { host: smtp.example.com, from: no-reply@%[1]s }
# SMTP credentials never go in this file: can up --smtp-credentials-file smtp.env (docs/email.md)
site:
  contact:
    title: Contact us
    text: Send us a message and we will get back to you.
`

func ask(in *bufio.Reader, out io.Writer, label, def string) string {
	if def != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}
