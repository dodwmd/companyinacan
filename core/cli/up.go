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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/launch"
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
	// The in-cluster half (epic E24): the phase machine, the SSH tunnel to the API, kubectl, the
	// post-deploy reachability probe, terminal detection and the kubectl lookup.
	stack    func(context.Context, launch.Options, launch.Deps) (*launch.Result, error)
	access   func(context.Context, provider.Target) (func(), error)
	kube     func(kubeconfig, context string) launch.Kube
	probe    func(ctx context.Context, url string) error
	isTTY    func() bool
	lookPath func(string) (string, error)
}{
	deps:     func() provider.Deps { return provider.Deps{} },
	resolver: net.DefaultResolver,
	now:      time.Now,
	stack:    launch.Run,
	access:   defaultAccess,
	kube:     defaultKube,
	probe:    defaultProbe,
	isTTY:    defaultIsTTY,
	lookPath: defaultLookPath,
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
	dnsCfg  provider.DNSConfig
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
	if t.dnsCfg, err = provider.DNSConfigFrom(co.DNS); err != nil {
		return nil, failure("%s: %v", file, err)
	}
	t.spec = provider.Spec{Dir: dir, Name: co.Name, Domain: co.Domain, Options: co.Provider.Options}
	if t.prov, err = provider.New(co.Provider.Type, t.deps); err != nil {
		return nil, usage("%v", err)
	}
	return t, nil
}

// dnsPlugin builds the DNS plugin: the --dns flag wins, then company.yaml's
// dns.provider, then manual.
func (t *target) dnsPlugin(flag string) (provider.DNS, error) {
	name := flag
	if name == "" {
		name = t.dnsCfg.Provider
	}
	return provider.NewDNSWith(name, t.dnsCfg, t.deps, t.store)
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

	// Set when the company stack was deployed (everything but --plan and --provision-only).
	Phases        []phaseJSON `json:"phases,omitempty"`
	SiteURLs      []string    `json:"site_urls,omitempty"`
	AdminURLs     []string    `json:"admin_urls,omitempty"`
	SiteReachable bool        `json:"site_reachable,omitempty"`
	AdminUser     string      `json:"admin_user,omitempty"`
	// Admin and Kit are present only with --print-secrets (or never, without a terminal).
	Admin        *credsJSON `json:"admin,omitempty"`
	Kit          string     `json:"recovery_kit,omitempty"`
	KitFile      string     `json:"recovery_kit_file,omitempty"`
	Warnings     []string   `json:"warnings,omitempty"`
	TotalSeconds float64    `json:"total_seconds,omitempty"`
}

func newUpCmd(g *globalOpts) *cobra.Command {
	var plan bool
	var modulesDir, dnsName string
	var sf stackFlags
	cmd := &cobra.Command{
		Use:   "up [dir]",
		Short: "Make a live company: provision, bootstrap k3s, deploy can-core, the site and forms",
		Long: `Provision the server named by company.yaml (provider.type: ssh, hetzner, digitalocean),
install k3s, then deploy the company: platform base, container images, can-core, the admin account,
company.yaml, the site, the routes, the backup timer (with --backup-*), DNS, and wait until it is ready.
It ends with the URLs, the one-time admin password and TOTP secret and the recovery kit.

Phases are checkpointed in .can/state.json: after an interruption at any point run the same command
again and it resumes. Nothing is ever wiped by running it again.

Use --plan to print the planned actions without changing anything, --provision-only to stop after
k3s. Without a terminal, secrets are printed only with --print-secrets.
Exit codes: 0 ok, 1 failure (the message names the phase and how to resume), 2 usage, 3 not implemented (local, static).
See docs/make-up.md.`,
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
			dns, err := t.dnsPlugin(dnsName)
			if err != nil {
				return t.fail(err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			sf.modulesDir = modulesDir
			return t.up(ctx, cmd, g, plan, dns, &sf)
		},
	}
	sf.register(cmd)
	cmd.Flags().BoolVar(&plan, "plan", false, "print the planned actions and change nothing")
	cmd.Flags().StringVar(&modulesDir, "modules-dir", "", "modules directory (default: ./modules)")
	cmd.Flags().StringVar(&dnsName, "dns", "", "DNS plugin: manual or cloudflare (default: dns.provider in company.yaml, else manual)")
	return cmd
}

func (t *target) up(ctx context.Context, cmd *cobra.Command, g *globalOpts, planOnly bool, dns provider.DNS, sf *stackFlags) error {
	out, flush := t.writers(cmd, g)
	defer flush()
	res := upResult{Provider: t.prov.Name(), Planned: planOnly}
	if !planOnly {
		release, err := t.store.LockOperation()
		if err != nil {
			return t.fail(err)
		}
		defer release()
	}

	acts, err := t.prov.Plan(ctx, t.spec, t.store)
	if err != nil {
		return t.fail(err)
	}
	acts = append(acts,
		provider.Action{ID: "kubeconfig", Description: fmt.Sprintf("fetch the k3s kubeconfig over ssh into %s (API reached via SSH tunnel on 127.0.0.1:%d; 6443 stays closed)", provider.KubeconfigPath(t.dir), provider.DefaultTunnelPort)},
		provider.Action{ID: "instant-url", Description: "decide the instant URL: https on <ip>.sslip.io, or plain-http fallback (sslip.io check + ACME rate limits)"},
	)
	if pl, ok := dns.(provider.DNSPlanner); ok {
		recs, _, _ := t.companyRecords("192.0.2.1", sf.modulesDir)
		for i := range recs {
			recs[i].Value = "<server IP>"
		}
		for _, line := range pl.PlanRecords(t.company.Domain, recs) {
			acts = append(acts, provider.Action{ID: "dns", Description: line})
		}
	} else {
		recs, _, _ := t.companyRecords("192.0.2.1", sf.modulesDir)
		var names []string
		for _, r := range recs {
			names = append(names, r.Name)
		}
		acts = append(acts, provider.Action{ID: "dns", Description: "print DNS records for " + strings.Join(names, ", ") + " (" + dns.Name() + " plugin)"})
	}
	if !sf.provisionOnly {
		acts = append(acts, stackPlanActions(sf)...)
	}
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

	// Fail on a bad DNS token or missing zone before any server is paid for.
	if ck, ok := dns.(provider.DNSChecker); ok {
		if err := ck.Preflight(ctx, t.company.Domain); err != nil {
			return t.fail(err)
		}
	}
	var prep *stackPrep
	if sf.allowBrokenBackup {
		fmt.Fprintln(cmd.ErrOrStderr(), "note: --allow-broken-backup is deprecated and does nothing: backups no longer need an override")
	}
	if !sf.provisionOnly {
		// Everything that can be checked offline is checked before a server exists or is paid for.
		if prep, err = t.stackPreflight(sf); err != nil {
			return t.fail(err)
		}
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

	recs, dnsWarnings, err := t.companyRecords(addr, sf.modulesDir)
	for _, w := range dnsWarnings {
		fmt.Fprintf(out, "warning: %s\n", w)
		res.Warnings = append(res.Warnings, w)
	}
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
	// An existing server is not bootstrapped again, so it may predate the node firewall.
	if w := firewallWarning(ctx, runner(tgt)); w != "" {
		fmt.Fprintf(out, "warning: %s\n\n", w)
		if g.json() {
			res.Warnings = append(res.Warnings, w)
		}
	}
	if !sf.provisionOnly {
		// DNS is applied inside finishStack, after the stack is up, so a Cloudflare record never
		// points at a server that cannot answer yet.
		return t.finishStack(ctx, cmd, g, out, &res, prep, sf, dns, tgt, instant, addr)
	}
	if len(recs) > 0 {
		if err := dns.Apply(ctx, t.company.Domain, recs, out); err != nil {
			return t.fail(err)
		}
	}
	res.Next = []string{"--provision-only: nothing is deployed on the server; run `can up` without it to deploy can-core, the site and forms"}
	fmt.Fprintln(out, "\nNext: --provision-only stopped after k3s; run `can up` without it to deploy can-core, the site and forms.")
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
		Long: `For the local provider this is ` + "`can dev down`" + ` (deletes the k3d cluster; --yes is not needed).
For the ssh provider this uninstalls k3s and can-core and leaves the server alone.
For hetzner and digitalocean it deletes the server, firewall and (if can uploaded it) the SSH key: all data on the server is lost.
With dns.provider: cloudflare the DNS records can created are deleted first (records it adopted or did not create are kept).

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
			if t.company.Provider.Type == "local" {
				// the local provider is the k3d cluster: same as `can dev down`
				return runDevDown(cmd, plan)
			}
			out, flush := t.writers(cmd, g)
			defer flush()
			warning := downWarning(t.prov.Name())
			if t.dnsCfg.Provider == "cloudflare" {
				warning += " Also deletes the Cloudflare DNS records can created (adopted and foreign records are kept)."
			}
			if plan || !yes {
				fmt.Fprintln(out, warning)
				if plan {
					if g.json() {
						return t.emitJSON(cmd, map[string]any{"provider": t.prov.Name(), "planned": true, "warning": warning})
					}
					return nil
				}
				return usage("refusing to continue without --yes")
			}
			fmt.Fprintln(out, warning)
			release, err := t.store.LockOperation()
			if err != nil {
				return t.fail(err)
			}
			defer release()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			// DNS first: records must not keep pointing at an address that is
			// about to be recycled. A failure stops here so the run can be repeated.
			dns, err := t.dnsPlugin("")
			if err != nil {
				return t.fail(err)
			}
			if rm, ok := dns.(provider.DNSRemover); ok {
				if err := rm.Remove(ctx, t.company.Domain, out); err != nil {
					return t.fail(err)
				}
			} else if st, err := t.store.Load(); err == nil && len(st.DNSRecords) > 0 {
				// Destroying would wipe the state that remembers these records.
				return t.fail(fmt.Errorf("%s lists %d DNS record(s) that can created, but company.yaml no longer has dns.provider: cloudflare, so they cannot be removed; restore the dns block and run `can down` again, or delete the records in Cloudflare and then the dns_records entries from .can/state.json", provider.StatePath(t.dir), len(st.DNSRecords)))
			}
			// The ssh provider leaves the machine behind, so the backup timer and the restic
			// credentials the recovery kit stands for must not stay on it. (The cloud providers
			// delete the machine.) Fail closed: a half-removed timer keeps running.
			if t.prov.Name() == "ssh" {
				if err := t.removeBackupTimer(ctx, out); err != nil {
					return t.fail(err)
				}
			}
			if err := t.prov.Destroy(ctx, t.spec, t.store, out); err != nil {
				return t.fail(err)
			}
			_ = os.Remove(provider.KubeconfigPath(dir))
			_ = os.Remove(filepath.Join(dir, ".can", "known_hosts"))
			fmt.Fprintln(out, "done")
			if g.json() {
				return t.emitJSON(cmd, map[string]any{"provider": t.prov.Name(), "destroyed": true})
			}
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
	case "digitalocean":
		return "can down (digitalocean): DELETES the droplet, its cloud firewall and the SSH key can uploaded. All data on the droplet is lost."
	}
	return "can down (" + name + ")"
}

type statusResult struct {
	Provider string           `json:"provider"`
	Phase    provider.Phase   `json:"phase"`
	Address  string           `json:"address,omitempty"`
	Checks   []provider.Check `json:"checks,omitempty"`
	Warnings []string         `json:"warnings,omitempty"`
	DNS      []dnsStatus      `json:"dns_records,omitempty"`
}

func newStatusCmd(g *globalOpts) *cobra.Command {
	var modulesDir string
	cmd := &cobra.Command{
		Use:   "status [dir]",
		Short: "Show the state of the server",
		Long:  "Show the server and stack checks, and the DNS records the company needs with where each name points now\n(informational: it does not change the exit code).\nExit codes: 0 ready, 1 anything else (absent, provisioning, degraded), 3 provider not implemented.",
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
			if st.Phase == provider.PhaseReady {
				// A company deployed by `can up` also has to be up inside the cluster.
				if ls, lerr := t.store.Load(); lerr == nil && ls.Launch != nil {
					checks, ok := t.stackChecks(ctx)
					res.Checks = append(res.Checks, checks...)
					if !ok {
						res.Phase = provider.PhaseDegraded
						st.Phase = provider.PhaseDegraded
					}
				}
			}
			if a, err := t.prov.Address(t.store); err == nil {
				res.Address = a
			}
			if st.Phase != provider.PhaseAbsent && t.company.Domain != "" {
				// a server known by name has no address records to list
				if recs, warnings, err := t.companyRecords(res.Address, modulesDir); err == nil {
					res.DNS = resolveRecords(ctx, lifecycle.resolver, recs)
					res.Warnings = append(res.Warnings, warnings...)
				}
			}
			// While the server is still being set up the bootstrap has not reached the firewall yet.
			if st.Phase != provider.PhaseAbsent && st.Phase != provider.PhaseProvisioning {
				if tgt, terr := provider.TargetFor(t.spec, t.store, t.deps); terr == nil {
					runner := t.deps.Runner
					if runner == nil {
						runner = func(tg provider.Target) provider.Runner { return provider.NewSSHRunner(tg) }
					}
					if w := firewallWarning(ctx, runner(tgt)); w != "" {
						res.Warnings = append(res.Warnings, w)
					}
				}
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
				if len(res.DNS) > 0 {
					fmt.Fprintln(w, "dns records needed:")
					for _, d := range res.DNS {
						state := d.State
						if d.State == dnsElsewhere {
							state = "points to " + strings.Join(d.Seen, ", ")
							if t.dnsCfg.Proxied {
								state = "behind the proxy (" + strings.Join(d.Seen, ", ") + ")"
							}
						}
						fmt.Fprintf(w, "  %-4s %s -> %s  (%s): %s\n", d.Type, d.Name, d.Value, d.Purpose, state)
					}
				}
				for _, m := range res.Warnings {
					fmt.Fprintf(w, "warning: %s\n", m)
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

// runDevDown delegates to `can dev down` (epic E02) for provider type local.
func runDevDown(cmd *cobra.Command, dryRun bool) error {
	o := &devOpts{cfg: cluster.DefaultConfig()}
	d := newDevDownCmd(o)
	o.dryRun = dryRun // after construction: registering the --dry-run flag resets it
	d.SetContext(cmd.Context())
	d.SetOut(cmd.OutOrStdout())
	d.SetErr(cmd.ErrOrStderr())
	return d.RunE(d, nil)
}
