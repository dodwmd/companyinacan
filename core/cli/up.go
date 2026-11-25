package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/validate"
)

// lifecycle holds the seams tests replace: provider construction, the DNS
// resolver used for the sslip.io check, and the clock.
var lifecycle = struct {
	deps     func() provider.Deps
	resolver provider.Resolver
	now      func() time.Time
}{
	deps:     func() provider.Deps { return provider.Deps{} },
	resolver: net.DefaultResolver,
	now:      time.Now,
}

// instantSubdomains are served on the instant sslip.io URL (SPEC.md section 3.4).
var instantSubdomains = []string{"admin", "help"}

type target struct {
	dir     string
	company *config.Company
	spec    provider.Spec
	prov    provider.Provider
	store   *provider.Store
	deps    provider.Deps
	redact  *provider.Redactor
}

// load validates company.yaml in dir and builds the provider.
func load(cmd *cobra.Command, g *globalOpts, dir, modulesDir string) (*target, error) {
	file := filepath.Join(dir, "company.yaml")
	if ps := validate.CompanyFile(file, resolveModulesDir(modulesDir, dir)); problem.HasErrors(ps) {
		if err := printProblems(cmd, g, ps); err != nil {
			return nil, err
		}
		return nil, silent(ExitFailure)
	}
	doc, ps := config.Load(file)
	if problem.HasErrors(ps) {
		return nil, failure("%s", ps[0].String())
	}
	co, err := config.DecodeCompany(doc)
	if err != nil {
		return nil, failure("%s: %v", file, err)
	}
	t := &target{dir: dir, company: co, redact: provider.NewRedactor(), store: provider.NewStore(dir)}
	t.deps = lifecycle.deps()
	t.deps.Redactor = t.redact
	t.spec = provider.Spec{Dir: dir, Name: co.Name, Domain: co.Domain, Options: co.Provider.Options}
	if t.prov, err = provider.New(co.Provider.Type, t.deps); err != nil {
		return nil, usage("%v", err)
	}
	return t, nil
}

// fail converts a provider error to an exit error, redacting secrets and
// mapping "not implemented" to exit code 3.
func (t *target) fail(err error) error {
	msg := t.redact.Redact(err.Error())
	if errors.Is(err, provider.ErrNotImplemented) {
		return &exitError{ExitNotImplemented, msg}
	}
	return &exitError{ExitFailure, msg}
}

// writers returns the progress and result writers. In JSON mode progress goes
// to stderr so stdout stays a single JSON document.
func (t *target) writers(cmd *cobra.Command, g *globalOpts) (progress io.Writer, flush func()) {
	dst := cmd.OutOrStdout()
	if g.json() {
		dst = cmd.ErrOrStderr()
	}
	w := t.redact.Writer(dst)
	return w, func() { _ = w.(provider.Flusher).Flush() }
}

func (t *target) emitJSON(cmd *cobra.Command, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), t.redact.Redact(string(b)))
	return err
}

type upResult struct {
	Provider   string                `json:"provider"`
	Actions    []provider.Action     `json:"actions"`
	Address    string                `json:"address,omitempty"`
	Instant    *provider.InstantPlan `json:"instant,omitempty"`
	Records    []provider.Record     `json:"dns_records,omitempty"`
	Kubeconfig string                `json:"kubeconfig,omitempty"`
	Next       []string              `json:"next,omitempty"`
	Planned    bool                  `json:"planned"`
}

func newUpCmd(g *globalOpts) *cobra.Command {
	var plan bool
	var modulesDir, dnsName string
	cmd := &cobra.Command{
		Use:   "up [dir]",
		Short: "Provision a server and bootstrap k3s (ssh, hetzner)",
		Long: `Provision the server named by company.yaml (provider.type), install k3s,
fetch the kubeconfig, and print the instant URL and the DNS records to add.

Use --plan to print the planned actions without changing anything.
Exit codes: 0 ok, 1 failure, 2 usage, 3 not implemented (local, static, digitalocean).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			t, err := load(cmd, g, dir, modulesDir)
			if err != nil {
				return err
			}
			dns, err := provider.NewDNS(dnsName)
			if err != nil {
				return t.fail(err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return t.up(ctx, cmd, g, plan, dns)
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "print the planned actions and change nothing")
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "", "modules directory (default: ./modules)")
	cmd.Flags().StringVar(&dnsName, "dns", "manual", "DNS plugin: manual")
	return cmd
}

func (t *target) up(ctx context.Context, cmd *cobra.Command, g *globalOpts, planOnly bool, dns provider.DNS) error {
	out, flush := t.writers(cmd, g)
	defer flush()
	res := upResult{Provider: t.prov.Name(), Planned: planOnly}

	acts, err := t.prov.Plan(ctx, t.spec, t.store)
	if err != nil {
		return t.fail(err)
	}
	acts = append(acts,
		provider.Action{ID: "kubeconfig", Description: fmt.Sprintf("fetch the k3s kubeconfig over ssh into %s (API reached via SSH tunnel on 127.0.0.1:%d; 6443 stays closed)", provider.KubeconfigPath(t.dir), provider.DefaultTunnelPort)},
		provider.Action{ID: "instant-url", Description: "decide the instant URL: https on <ip>.sslip.io, or plain-http fallback (sslip.io check + ACME rate limits)"},
		provider.Action{ID: "dns", Description: "print DNS records for " + t.company.Domain + " (" + dns.Name() + " plugin)"},
	)
	res.Actions = acts
	if planOnly {
		if g.json() {
			return t.emitJSON(cmd, res)
		}
		fmt.Fprintf(out, "Plan for provider %q (nothing will be changed):\n", t.prov.Name())
		for i, a := range acts {
			mark := "todo"
			if a.Done {
				mark = "done"
			}
			fmt.Fprintf(out, "%2d. [%s] %s\n", i+1, mark, a.Description)
		}
		return nil
	}

	if err := t.prov.Create(ctx, t.spec, t.store, out); err != nil {
		return t.fail(err)
	}
	addr, err := t.prov.Address(t.store)
	if err != nil {
		return t.fail(err)
	}
	res.Address = addr

	tgt, err := provider.TargetFor(t.spec, t.store, t.deps)
	if err != nil {
		return t.fail(err)
	}
	if t.spec.Options != nil {
		if tgt.KeyFile == "" {
			if k, ok := t.spec.Options["ssh_private_key_file"].(string); ok {
				tgt.KeyFile = k
			}
		}
	}
	runner := t.deps.Runner
	if runner == nil {
		runner = func(tg provider.Target) provider.Runner { return provider.NewSSHRunner(tg) }
	}
	kc, err := provider.FetchKubeconfig(ctx, runner(tgt), t.dir, kubeName(t.company.Name), provider.DefaultTunnelPort)
	if err != nil {
		return t.fail(err)
	}
	res.Kubeconfig = kc

	state, err := t.store.Load()
	if err != nil {
		return t.fail(err)
	}
	instant := provider.PlanInstant(ctx, addr, instantSubdomains, lifecycle.now(), state.Issued, lifecycle.resolver)
	res.Instant = &instant

	recs, err := provider.Records(t.company.Domain, addr, instantSubdomains)
	if err != nil {
		// a hostname instead of an IP: records are CNAMEs the operator adds by hand
		fmt.Fprintf(out, "note: server address %q is not an IP, so no A records are generated\n", addr)
	} else {
		res.Records = recs
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Server is up.")
	fmt.Fprintf(out, "  address:     %s\n  kubeconfig:  %s\n", addr, kc)
	if instant.Mode == provider.ModeHTTPS {
		fmt.Fprintln(out, "  instant URL (https once the site is deployed):")
		for _, u := range instant.URLs {
			fmt.Fprintf(out, "    %s\n", u)
		}
	} else {
		fmt.Fprintf(out, "  instant URL: %s (plain HTTP fallback, public site only)\n  reason:      %s\n  admin UI:    reachable only through the SSH tunnel until DNS/TLS is set\n", instant.URLs[0], instant.Reason)
	}
	fmt.Fprintln(out)
	if len(recs) > 0 {
		if err := dns.Apply(ctx, t.company.Domain, recs, out); err != nil {
			return t.fail(err)
		}
	}
	res.Next = []string{"can-core install and `can apply` land in epic E03; until then the server runs bare k3s"}
	fmt.Fprintln(out, "\nNext: can-core install and `can apply` arrive in epic E03; until then the server runs bare k3s.")
	if g.json() {
		return t.emitJSON(cmd, res)
	}
	return nil
}

// kubeName makes a DNS-label-safe kubeconfig context name from the company name.
func kubeName(name string) string {
	out := make([]byte, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, byte(r))
		case r >= 'A' && r <= 'Z':
			out = append(out, byte(r-'A'+'a'))
		default:
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	s := string(trimDash(out))
	if s == "" {
		return "company"
	}
	if len(s) > 60 {
		s = string(trimDash([]byte(s[:60])))
	}
	return s
}

func trimDash(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	return b
}

func newDownCmd(g *globalOpts) *cobra.Command {
	var plan, yes bool
	var modulesDir string
	cmd := &cobra.Command{
		Use:   "down [dir]",
		Short: "Tear down what `can up` created",
		Long: `For the ssh provider this uninstalls k3s and can-core and leaves the server alone.
For hetzner it deletes the server, firewall and (if can uploaded it) the SSH key: all data on the server is lost.

Destructive: requires --yes. Use --plan to see what would happen.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			t, err := load(cmd, g, dir, modulesDir)
			if err != nil {
				return err
			}
			out, flush := t.writers(cmd, g)
			defer flush()
			warning := downWarning(t.prov.Name())
			if plan || !yes {
				fmt.Fprintln(out, warning)
				if plan {
					return nil
				}
				return usage("refusing to continue without --yes")
			}
			fmt.Fprintln(out, warning)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			if err := t.prov.Destroy(ctx, t.spec, t.store, out); err != nil {
				return t.fail(err)
			}
			_ = os.Remove(provider.KubeconfigPath(dir))
			fmt.Fprintln(out, "done")
			return nil
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "print what would be removed and change nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm the teardown")
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "", "modules directory (default: ./modules)")
	return cmd
}

func downWarning(name string) string {
	switch name {
	case "ssh":
		return "can down (ssh): uninstalls k3s and can-core from the server; the server itself, its firewall and swap are left as they are."
	case "hetzner":
		return "can down (hetzner): DELETES the server, its cloud firewall and the SSH key can uploaded. All data on the server is lost."
	}
	return "can down (" + name + ")"
}

type statusResult struct {
	Provider string           `json:"provider"`
	Phase    provider.Phase   `json:"phase"`
	Address  string           `json:"address,omitempty"`
	Checks   []provider.Check `json:"checks,omitempty"`
}

func newStatusCmd(g *globalOpts) *cobra.Command {
	var modulesDir string
	cmd := &cobra.Command{
		Use:   "status [dir]",
		Short: "Show the state of the server",
		Long:  "Exit codes: 0 ready, 1 anything else (absent, provisioning, degraded), 3 provider not implemented.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			t, err := load(cmd, g, dir, modulesDir)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			st, err := t.prov.Status(ctx, t.spec, t.store)
			if err != nil {
				return t.fail(err)
			}
			res := statusResult{Provider: t.prov.Name(), Phase: st.Phase, Checks: st.Checks}
			if a, err := t.prov.Address(t.store); err == nil {
				res.Address = a
			}
			if g.json() {
				if err := t.emitJSON(cmd, res); err != nil {
					return err
				}
			} else {
				w := t.redact.Writer(cmd.OutOrStdout())
				fmt.Fprintf(w, "provider: %s\nstatus:   %s\n", res.Provider, res.Phase)
				if res.Address != "" {
					fmt.Fprintf(w, "address:  %s\n", res.Address)
				}
				for _, c := range res.Checks {
					mark := "ok  "
					if !c.OK {
						mark = "FAIL"
					}
					fmt.Fprintf(w, "  [%s] %s %s\n", mark, c.Name, c.Detail)
				}
				_ = w.(provider.Flusher).Flush()
			}
			if st.Phase != provider.PhaseReady {
				return silent(ExitFailure)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "", "modules directory (default: ./modules)")
	return cmd
}
