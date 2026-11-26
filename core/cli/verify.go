package cli

// `can verify --server` (specs/052-real-server-verification): the automatable part of
// docs/real-server-verification.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/launch"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/verify"
)

// verifyNet is how the probes reach the network; tests replace it.
var verifyNet = verify.DefaultNet

const verifyTimeout = 8 * time.Minute

type verifyFlags struct {
	server, public, rate, clientIP, repo, smtp, sso bool
	companyDir, modulesDir, out                     string
}

func (f *verifyFlags) probes() []string {
	out := []string{}
	for _, p := range []struct {
		on   bool
		name string
	}{{f.public, "public"}, {f.rate, "rate-limit"}, {f.clientIP, "client-ip"}, {f.repo, "backup-repo"}, {f.smtp, "smtp"}, {f.sso, "sso"}} {
		if p.on {
			out = append(out, p.name)
		}
	}
	return out
}

func newVerifyCmd(g *globalOpts) *cobra.Command {
	f := &verifyFlags{}
	cmd := &cobra.Command{
		Use:   "verify --server",
		Short: "Check an installed server and write a report that is safe to share",
		Long: "Checks the server recorded in .can/state.json and prints pass, fail or skip for each item\n" +
			"(docs/real-server-verification.md covers what a person has to check). By default it only runs\n" +
			"read-only commands over ssh. Anything that sends traffic elsewhere needs its --probe flag; no\n" +
			"flag sends mail, signs in or deletes anything. The report holds no secrets, addresses, domain\n" +
			"or user names; read it before you share it all the same.\n" +
			"Exit codes: 0 no check failed, 1 at least one failed, 2 usage.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !f.server {
				return usage("can verify needs --server (local prerequisites: can doctor)")
			}
			return runVerify(cmd, g, f)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.server, "server", false, "check the installed server over ssh (read-only)")
	fl.BoolVar(&f.public, "probe-public", false, "also look up DNS and connect to the server from this machine: ports, certificates, redirect, site, admin")
	fl.BoolVar(&f.rate, "probe-rate-limit", false, "also request the sign-in page until the edge refuses (up to 90 requests; limits your address for about 30 s)")
	fl.BoolVar(&f.clientIP, "probe-client-ip", false, "also make ONE failed sign-in for a user that does not exist (adds an audit entry) to see which address can-core records")
	fl.BoolVar(&f.repo, "probe-backup-repo", false, "also list the snapshots in the backup bucket from the server (read-only, no lock)")
	fl.BoolVar(&f.smtp, "probe-smtp", false, "also open a TLS session from the server to email.relay (no sign-in, no mail)")
	fl.BoolVar(&f.sso, "probe-sso", false, "also fetch sso.issuer's discovery document from the server (no sign-in)")
	fl.StringVar(&f.companyDir, "company-dir", ".", "directory holding company.yaml and .can/")
	fl.StringVar(&f.modulesDir, "modules-dir", "", "modules directory (default: ./modules, else the manifests built into can)")
	fl.StringVar(&f.out, "out", "", "also write the JSON report here (an existing file is never overwritten)")
	return cmd
}

func runVerify(cmd *cobra.Command, g *globalOpts, f *verifyFlags) error {
	t, err := load(cmd, g, f.companyDir, f.modulesDir)
	if err != nil {
		return err
	}
	state, err := t.store.Load()
	if err != nil {
		return t.fail(err)
	}
	tgt, err := provider.TargetFor(t.spec, t.store, t.deps)
	if err != nil {
		return t.fail(err)
	}
	bo, err := provider.BootstrapOptionsFrom(t.spec)
	if err != nil {
		return t.fail(err)
	}
	runner := t.deps.Runner
	if runner == nil {
		runner = func(tg provider.Target) provider.Runner { return provider.NewSSHRunner(tg) }
	}
	r := runner(tgt)
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	var out *os.File
	if f.out != "" { // before anything is probed: a report that cannot be written wastes a run
		if out, err = os.OpenFile(f.out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil { //nolint:gosec // operator-chosen output path
			return failure("%v", err)
		}
		defer func() { _ = out.Close() }()
	}

	rep := &verify.Report{Schema: verify.Schema, GeneratedAt: lifecycle.now().UTC().Truncate(time.Second),
		CanVersion: version, Provider: t.prov.Name(), Probes: f.probes()}
	co := t.company
	opts := verify.ServerOptions{
		K3sVersion:      bo.K3sVersion,
		BackupInstalled: state.Launch != nil && state.Launch.BackupInstalled,
		CLI:             newCanSource("", launch.ImageSet{}).Identity(),
	}
	support, issues := config.SupportMailOf(co)
	opts.SupportMail = support.Address != "" || slices.ContainsFunc(issues, func(is config.SupportIssue) bool { return is.Invalid })
	relayHost, relayPort, relayImplicit := "", 587, false
	if co.Email != nil && co.Email.Relay != nil && co.Email.Relay.Host != "" {
		opts.EmailRelay = true
		relayHost = co.Email.Relay.Host
		if co.Email.Relay.Port != 0 {
			relayPort = co.Email.Relay.Port
		}
		relayImplicit = co.Email.Relay.TLS == "implicit" || relayPort == 465
	}
	issuer := ""
	if co.SSO != nil && co.SSO.Enabled {
		issuer = co.SSO.Issuer
		opts.SSOSecret, _, _ = strings.Cut(co.SSO.ClientSecretRef, "/")
	}

	checks, facts := verify.CollectServer(ctx, r, opts)
	rep.Checks = checks
	reachable := len(facts) > 0
	routes := facts.Routes()
	addr := state.Server.IP
	n := verifyNet()

	probe := func(on bool, flag string, ids []string, run func() []verify.Check) {
		why := ""
		switch {
		case !on:
			why = "not run: pass " + flag
		case !reachable:
			why = "not run: the server could not be checked over ssh"
		default:
			rep.Checks = append(rep.Checks, run()...)
			return
		}
		for _, id := range ids {
			rep.Checks = append(rep.Checks, verify.Check{ID: id, Status: verify.Skip, Detail: why})
		}
	}
	probe(f.public, "--probe-public", []string{"public.dns", "public.closed-ports", "public.certificates", "public.site", "public.https-redirect", "public.admin"},
		func() []verify.Check {
			cs := []verify.Check{t.verifyDNS(ctx, addr, f.modulesDir, routes), verify.CheckPorts(ctx, n, addr), verify.CheckTLS(ctx, n, addr, routes)}
			return append(cs, verify.CheckHTTP(ctx, n, addr, co.Domain, routes)...)
		})
	// Before the rate limit probe, which leaves the sign-in paths limited for this address.
	probe(f.clientIP, "--probe-client-ip", []string{"edge.client-address"},
		func() []verify.Check { return []verify.Check{verify.CheckClientIP(ctx, n, r, addr, co.Domain, routes)} })
	probe(f.rate, "--probe-rate-limit", []string{"edge.rate-limit"},
		func() []verify.Check {
			return []verify.Check{verify.CheckRateLimit(ctx, n, addr, co.Domain, routes, len(facts["login_limit"]) > 0 && facts["login_limit"][0] != "")}
		})
	probe(f.repo, "--probe-backup-repo", []string{"backup.repository"},
		func() []verify.Check { return []verify.Check{verify.CheckBackupRepo(ctx, r, lifecycle.now())} })
	probe(f.smtp, "--probe-smtp", []string{"email.relay-tls"},
		func() []verify.Check {
			return []verify.Check{verify.CheckSMTP(ctx, r, relayHost, relayPort, relayImplicit)}
		})
	probe(f.sso, "--probe-sso", []string{"sso.discovery"},
		func() []verify.Check { return []verify.Check{verify.CheckSSO(ctx, r, issuer)} })

	issuerHost := ""
	if u, err := url.Parse(issuer); err == nil {
		issuerHost = u.Hostname()
	}
	scrub := verify.NewScrubber(co.Domain, addr, co.Name, relayHost, issuerHost, state.Server.Name)
	scrub.Routed(co.Domain, addr, routes)
	rep.Finish(scrub)

	if out != nil {
		if err := writeReport(out, rep); err != nil {
			return failure("writing the report: %v", err)
		}
	}
	w := cmd.OutOrStdout()
	if g.json() {
		if err := writeReport(w, rep); err != nil {
			return err
		}
	} else {
		for _, c := range rep.Checks {
			fmt.Fprintf(w, "%-4s  %-28s %s\n", strings.ToUpper(string(c.Status)), c.ID, c.Detail)
		}
		fmt.Fprintf(w, "\n%d passed, %d failed, %d skipped. A skip is not a pass. Manual checks: docs/real-server-verification.md\n",
			rep.Summary.Pass, rep.Summary.Fail, rep.Summary.Skip)
		if f.out != "" {
			fmt.Fprintf(w, "report written to %s; read it before sharing\n", f.out)
		}
	}
	if rep.Summary.Fail > 0 {
		return silent(ExitFailure)
	}
	return nil
}

func writeReport(w io.Writer, rep *verify.Report) error {
	e := json.NewEncoder(w)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	return e.Encode(rep)
}

// verifyDNS looks up the records `can status` lists.
func (t *target) verifyDNS(ctx context.Context, addr, modulesDir string, routes []verify.Route) verify.Check {
	var names []verify.DNSName
	if t.company.Domain != "" {
		if recs, _, err := t.companyRecords(addr, modulesDir); err == nil {
			for _, s := range resolveRecords(ctx, lifecycle.resolver, recs) {
				names = append(names, verify.DNSName{Name: s.Name, State: s.State})
			}
		}
	}
	return verify.CheckDNS(names, t.company.Domain, t.dnsCfg.Proxied, routes)
}
