package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/launch"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/routing"
	"github.com/dodwmd/companyinacan/core/validate"
)

// stackFlags are the `can up` flags for everything after the server exists (epic E24).
type stackFlags struct {
	provisionOnly     bool
	imagesFrom        string
	releaseManifest   string
	printSecrets      bool
	noDefaultBundle   bool
	writeBundle       bool
	forceCompany      bool
	allowBrokenBackup bool
	domainLive        bool
	acmeEmail         string
	backupEndpoint    string
	backupBucket      string
	backupRegion      string
	backupCreds       string
	kitOut            string
	kitPassFile       string
	modulesDir        string
	smtpCreds         string
	noEdgeLimits      bool
}

func (f *stackFlags) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.BoolVar(&f.provisionOnly, "provision-only", false, "stop after the server, k3s, kubeconfig and DNS (the pre-E24 behaviour); deploy nothing")
	fl.StringVar(&f.imagesFrom, "images-from", "", "directory written by `make images`: tarballs are verified, shipped over ssh and imported (no registry needed)")
	fl.StringVar(&f.releaseManifest, "release-manifest", "", "release manifest pinning every image by digest (default deploy/release-images.json when present)")
	fl.BoolVar(&f.printSecrets, "print-secrets", false, "print the admin password, TOTP secret and recovery kit without a terminal (CI); never the default")
	fl.BoolVar(&f.noDefaultBundle, "no-default-bundle", false, "do not add the forms module to the company pushed to the cluster")
	fl.BoolVar(&f.allowBrokenBackup, "allow-broken-backup", false, "deprecated, does nothing (kept so existing commands keep working)")
	fl.BoolVar(&f.writeBundle, "write-bundle", false, "also write the default bundle (the forms module) into company.yaml on disk, so `can apply` keeps it")
	fl.BoolVar(&f.forceCompany, "force-company", false, "overwrite the company.yaml in the cluster even if it was changed there (admin UI, `can apply`) since the last `can up`")
	fl.BoolVar(&f.domainLive, "domain-live", false, "the company domain's DNS records resolve to the server: also route and certify it (implied by dns.provider: cloudflare)")
	fl.BoolVar(&f.noEdgeLimits, "no-edge-limits", false, "no per-address rate limit on sign-in and public routes: for a server behind your own proxy or CDN (implied by dns.proxied; remembered, --no-edge-limits=false turns the limits on again)")
	fl.StringVar(&f.acmeEmail, "acme-email", "", "registration address for Let's Encrypt certificates (optional)")
	fl.StringVar(&f.backupEndpoint, "backup-endpoint", "", "S3 endpoint URL; with --backup-bucket and --backup-credentials-file turns the backup timer and recovery kit on")
	fl.StringVar(&f.backupBucket, "backup-bucket", "", "backup bucket name")
	fl.StringVar(&f.backupRegion, "backup-region", "", "backup bucket region (default: what the server already uses, otherwise auto)")
	fl.StringVar(&f.backupCreds, "backup-credentials-file", "", "KEY=VALUE file with AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	fl.StringVar(&f.kitOut, "kit-out", "", "write the recovery kit encrypted to this new file (outside any git tree) instead of showing it")
	fl.StringVar(&f.kitPassFile, "kit-passphrase-file", "", "passphrase file for --kit-out (default: prompt)")
	fl.StringVar(&f.smtpCreds, "smtp-credentials-file", "", "KEY=VALUE file with SMTP_USERNAME and SMTP_PASSWORD for company.yaml's email.relay (stored in a Kubernetes Secret)")
}

func (f *stackFlags) backupWanted() bool {
	return f.backupBucket != "" || f.backupEndpoint != "" || f.backupCreds != ""
}

// stackPrep is everything validated before a server is paid for.
type stackPrep struct {
	images      launch.ImageSet
	backup      *launch.BackupInstall
	companyYAML []byte
	mods        map[string]*config.Module
	smtp        *launch.SMTPRelay
}

// stackPlanActions lists the phases after the server exists, for --plan.
func stackPlanActions(f *stackFlags) []provider.Action {
	a := []provider.Action{
		{ID: "stack.images", Description: "images: ship and verify the tarballs from --images-from over ssh, or use the digests of the release manifest"},
		{ID: "stack.base", Description: "apply the platform base (namespaces, network policies, RBAC, admission policy, Traefik) through the SSH tunnel"},
		{ID: "stack.company", Description: "push company.yaml as ConfigMap can-system/company (default bundle: site + forms)"},
		{ID: "stack.core", Description: "deploy can-core and wait for it"},
		{ID: "stack.admin", Description: "create the admin account inside can-core with a generated password and TOTP secret (password on stdin, never in argv)"},
		{ID: "stack.site", Description: "build the site and publish it to the site pod (ConfigMap site-content, unpacked by an init container)"},
		{ID: "stack.routes", Description: "route the site, the admin UI and the same-origin /_forms path through Traefik (on TLS installs plain http:// page loads redirect to https://)"},
	}
	if f.smtpCreds != "" {
		a = append(a, provider.Action{ID: "stack.email", Description: "store the SMTP relay credentials in Secret can-system/can-smtp-relay"})
	}
	if f.backupWanted() {
		a = append(a, provider.Action{ID: "stack.backup", Description: "install the restic backup timer on the host and prepare the recovery kit"})
	}
	return append(a,
		provider.Action{ID: "stack.ready", Description: "wait for can-core, the site and forms to be available (bounded timeouts)"},
		provider.Action{ID: "summary", Description: "print the URLs, the one-time admin credentials (terminal or --print-secrets only) and the recovery kit once"},
	)
}

func (t *target) stackPreflight(f *stackFlags) (*stackPrep, error) {
	if _, err := lifecycle.lookPath("kubectl"); err != nil {
		return nil, fmt.Errorf("kubectl was not found in PATH: `can up` applies the company to the cluster with it; install kubectl (https://kubernetes.io/docs/tasks/tools/) and run `can up` again; nothing has been provisioned yet")
	}
	set, err := launch.ResolveImages(launch.ImageOptions{FromDir: f.imagesFrom, ReleaseManifest: f.releaseManifest})
	if err != nil {
		return nil, fmt.Errorf("%w (nothing has been provisioned yet)", err)
	}
	raw, err := os.ReadFile(filepath.Join(t.dir, "company.yaml"))
	if err != nil {
		return nil, err
	}
	p := &stackPrep{images: set, companyYAML: raw, mods: map[string]*config.Module{}}
	if p.smtp, err = t.smtpRelay(f.smtpCreds); err != nil {
		return nil, err
	}
	// Best effort, for warnings: what cannot be read is judged by the install below, if any.
	md := resolveModulesDir(f.modulesDir, t.dir)
	for id := range t.company.Modules {
		if m, err := loadModule(md, id); err == nil {
			p.mods[id] = m
		}
	}
	if !f.backupWanted() {
		return p, nil
	}
	if f.backupEndpoint == "" || f.backupBucket == "" || f.backupCreds == "" {
		return nil, errors.New("backup needs all of --backup-endpoint, --backup-bucket and --backup-credentials-file (or none of them)")
	}
	if !lifecycle.isTTY() && !f.printSecrets && f.kitOut == "" {
		return nil, errors.New("backup is on, so a recovery kit will be produced, and this is not a terminal: pass --kit-out FILE with --kit-passphrase-file FILE to store it encrypted, or --print-secrets to print it (CI). The kit is never written inside a git tree")
	}
	if f.kitOut != "" && f.kitPassFile == "" && !lifecycle.isTTY() {
		return nil, errors.New("--kit-out needs --kit-passphrase-file when there is no terminal to ask on")
	}
	env, err := backup.LoadEnvFile(f.backupCreds)
	if err != nil {
		return nil, fmt.Errorf("--backup-credentials-file: %w", err)
	}
	in := backup.KitInput{
		CompanyName: t.company.Name, Domain: t.company.Domain, Endpoint: f.backupEndpoint, Bucket: f.backupBucket, Region: f.backupRegion,
		ProviderType: t.company.Provider.Type, ProviderOptions: t.company.Provider.Options,
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		switch k {
		case "AWS_ACCESS_KEY_ID":
			in.AccessKeyID = v
		case "AWS_SECRET_ACCESS_KEY":
			in.SecretAccessKey = v
		}
	}
	t.redact.Add(in.SecretAccessKey)
	if _, err := backup.NewKit(in); err != nil { // validates the endpoint, bucket and credentials shape
		return nil, fmt.Errorf("backup settings: %w", err)
	}
	for id, e := range t.company.Modules {
		if e.IsEnabled() && p.mods[id] == nil {
			if _, err := loadModule(md, id); err != nil {
				return nil, err
			}
		}
	}
	p.backup = &launch.BackupInstall{Input: in, Mods: p.mods, Settings: backup.DefaultSettings()}
	return p, nil
}

// loadModule reads the manifest of module id from the modules directory md.
func loadModule(md, id string) (*config.Module, error) {
	mp, ok := validate.ManifestPath(md, id)
	if !ok {
		return nil, fmt.Errorf("module %q has no manifest in %s", id, md)
	}
	doc, ps := config.Load(mp)
	if problem.HasErrors(ps) {
		return nil, errors.New(ps[0].String())
	}
	m, err := config.DecodeModule(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", mp, err)
	}
	return m, nil
}

// stackRoutes decides which hostnames are routed, and how.
func (t *target) stackRoutes(instant provider.InstantPlan, ip string, f *stackFlags, dnsName string) (launch.RouteOptions, string) {
	domainLive := f.domainLive || dnsName == "cloudflare"
	o := launch.RouteOptions{}
	base := ""
	switch instant.Mode {
	case provider.ModeHTTPS:
		o.TLS = true
		o.SiteHosts = []string{instant.Hosts[0]}
		for _, h := range instant.Hosts[1:] {
			if strings.HasPrefix(h, "admin.") {
				o.AdminHosts = append(o.AdminHosts, h)
			}
		}
		base = "https://" + instant.Hosts[0]
	default:
		o.CatchAll = true
		base = "http://" + ip
	}
	if domainLive {
		o.TLS = true
		o.SiteHosts = append(o.SiteHosts, t.company.Domain)
		o.AdminHosts = append(o.AdminHosts, "admin."+t.company.Domain)
		base = "https://" + t.company.Domain
	}
	return o, base
}

type credsJSON struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	TOTPSecret string `json:"totp_secret"`
	TOTPURI    string `json:"totp_uri,omitempty"`
}

type phaseJSON struct {
	Name     string  `json:"name"`
	Skipped  bool    `json:"skipped,omitempty"`
	Seconds  float64 `json:"seconds"`
	Note     string  `json:"note,omitempty"`
	Finished bool    `json:"finished"`
}

func hostURLs(scheme string, hosts []string) []string {
	var out []string
	for _, h := range hosts {
		out = append(out, scheme+"://"+h)
	}
	return out
}

// finishStack runs the in-cluster phases and prints the summary. It returns the exit error.
func (t *target) finishStack(ctx context.Context, cmd *cobra.Command, g *globalOpts, out io.Writer, res *upResult, prep *stackPrep, f *stackFlags, dns provider.DNS, tgt provider.Target, instant provider.InstantPlan, ip string) error {
	started := lifecycle.now()
	runner := t.deps.Runner
	if runner == nil {
		runner = func(tg provider.Target) provider.Runner { return provider.NewSSHRunner(tg) }
	}
	host := runner(tgt)
	closeTunnel, err := lifecycle.access(ctx, tgt)
	if err != nil {
		return t.fail(fmt.Errorf("cannot reach the cluster API through the SSH tunnel: %w; the server is provisioned, run `can up` again", err))
	}
	defer closeTunnel()

	routes, base := t.stackRoutes(instant, ip, f, dns.Name())
	if cmd.Flags().Changed("no-edge-limits") {
		if err := t.store.Mutate(func(s *provider.State) error { s.NoEdgeLimits = f.noEdgeLimits; return nil }); err != nil {
			return t.fail(err)
		}
	}
	state, err := t.store.Load()
	if err != nil {
		return t.fail(err)
	}
	routes.NoEdgeLimit, routes.NoEdgeLimitReason = routing.EdgeLimits(state.NoEdgeLimits, t.dnsCfg.Proxied)
	res.SiteURLs = hostURLs(schemeOf(routes.TLS), routes.SiteHosts)
	res.AdminURLs = hostURLs(schemeOf(routes.TLS), routes.AdminHosts)
	if routes.CatchAll {
		res.SiteURLs = append(res.SiteURLs, "http://"+ip)
	}
	o := launch.Options{
		Dir: t.dir, Company: t.company, CompanyYAML: prep.companyYAML, Images: prep.images, Routes: routes,
		ACMEEmail: f.acmeEmail, DefaultBundle: !f.noDefaultBundle, ForceCompany: f.forceCompany, SiteBaseURL: base, Backup: prep.backup, Mods: prep.mods, SMTP: prep.smtp,
	}
	d := launch.Deps{
		Kube: lifecycle.kube(provider.KubeconfigPath(t.dir), kubeName(t.company.Name)), Loader: launch.SSHImageLoader{Host: host}, Host: host,
		Store: t.store, Redactor: t.redact, Out: t.redact.Writer(out), Now: lifecycle.now,
	}
	lr, runErr := lifecycle.stack(ctx, o, d)
	if lr != nil {
		for _, p := range lr.Phases {
			res.Phases = append(res.Phases, phaseJSON{Name: p.Name, Skipped: p.Skipped, Seconds: p.Duration.Seconds(), Note: p.Note, Finished: true})
		}
	}
	if runErr != nil {
		return t.fail(runErr)
	}
	res.Warnings = append(res.Warnings, lr.Warnings...)
	if lr.BundleAdded {
		if f.writeBundle {
			if err := writeFileAtomic(filepath.Join(t.dir, "company.yaml"), lr.CompanyYAML); err != nil {
				return t.fail(fmt.Errorf("--write-bundle: %w", err))
			}
			res.Warnings = append(res.Warnings, "added the forms module to company.yaml (--write-bundle); yaml comments in that file may have been reformatted")
		} else {
			res.Warnings = append(res.Warnings, "company.yaml does not list the forms module: the default bundle enabled it in the cluster only. Add `forms: {}` under modules: (or run `can up --write-bundle`) so `can apply` keeps it; a later `can apply` would disable it again")
		}
	}

	if len(res.Records) > 0 {
		if err := dns.Apply(ctx, t.company.Domain, res.Records, out); err != nil {
			return t.fail(err)
		}
	}

	// Public reachability: informative, never fatal (DNS and certificates have their own clocks).
	if len(res.SiteURLs) > 0 {
		if err := lifecycle.probe(ctx, res.SiteURLs[0]); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s did not answer yet (%v); a new certificate or DNS record can take a few minutes: check again shortly, or `can status`", res.SiteURLs[0], err))
		} else {
			res.SiteReachable = true
		}
	}
	if !routes.TLS && len(res.AdminURLs) == 0 {
		res.AdminURLs = []string{"not exposed over plain HTTP: kubectl --kubeconfig " + provider.KubeconfigPath(t.dir) + " -n can-system port-forward svc/can-core 8080:8080, then open http://localhost:8080"}
	}
	res.TotalSeconds = lifecycle.now().Sub(started).Seconds()
	return t.summarize(cmd, g, out, res, lr, f)
}

func schemeOf(tls bool) string {
	if tls {
		return "https"
	}
	return "http"
}

// summarize prints the result. Secrets go to the raw stdout writer, never through the redacting
// progress writer, and only in a terminal (after a confirmation) or with --print-secrets.
func (t *target) summarize(cmd *cobra.Command, g *globalOpts, out io.Writer, res *upResult, lr *launch.Result, f *stackFlags) error {
	ew := &errWriter{w: cmd.OutOrStdout()}
	var raw io.Writer = ew
	tty := lifecycle.isTTY()
	showSecrets := tty || f.printSecrets

	var adminShown, kitShown bool
	if lr.Admin != nil {
		res.AdminUser = lr.Admin.Username
		// JSON output is machine output: a terminal does not make it a safe place to hand over
		// secrets (no confirmation, and it is redacted without --print-secrets). Only
		// --print-secrets writes them, and only then are they counted as handed over.
		if g.json() && f.printSecrets {
			res.Admin = &credsJSON{Username: lr.Admin.Username, Password: lr.Admin.Password, TOTPSecret: lr.Admin.TOTPSecret, TOTPURI: lr.Admin.TOTPURI}
			adminShown = true
		} else if g.json() {
			res.Warnings = append(res.Warnings, "admin credentials were created but not shown (--output json without --print-secrets); run `can up` again without --output json in a terminal, or with --print-secrets: a new password and TOTP secret are issued until they have been shown")
		}
	}
	if lr.Kit != nil && f.kitOut != "" {
		pass, err := kitPassphrase(f)
		if err != nil {
			return t.fail(err)
		}
		if err := backup.WriteKitFile(f.kitOut, lr.Kit, pass); err != nil {
			return t.fail(fmt.Errorf("%w (the server's backup timer is installed; run `can up` again to be offered the kit again)", err))
		}
		res.KitFile = f.kitOut
		kitShown = true
	} else if lr.Kit != nil && f.printSecrets && g.json() {
		res.Kit = lr.Kit.Text()
		kitShown = true
	} else if lr.Kit != nil && g.json() {
		res.Warnings = append(res.Warnings, "the recovery kit was not shown (--output json without --print-secrets or --kit-out); run `can up` again to be offered it")
	}

	if g.json() {
		if err := t.emitSecretJSON(cmd, res, f.printSecrets); err != nil {
			return err
		}
	} else {
		p := func(format string, a ...any) { fmt.Fprintf(raw, format, a...) }
		p("\nYour company is up (%s).\n\n", (time.Duration(res.TotalSeconds * float64(time.Second))).Round(time.Second))
		p("  Site:   %s\n", strings.Join(orNone(res.SiteURLs), "\n          "))
		p("  Admin:  %s\n", strings.Join(orNone(res.AdminURLs), "\n          "))
		for _, w := range res.Warnings {
			p("  note:   %s\n", t.redact.Redact(w))
		}
		if lr.Admin != nil {
			if showSecrets {
				p("\nAdmin login (shown once; store it in a password manager now):\n")
				p("  username:     %s\n  password:     %s\n  TOTP secret:  %s\n", lr.Admin.Username, lr.Admin.Password, lr.Admin.TOTPSecret)
				if lr.Admin.TOTPURI != "" {
					p("  TOTP URI:     %s\n", lr.Admin.TOTPURI)
				}
				if tty && !f.printSecrets {
					if confirm(cmd.InOrStdin(), raw, "Type 'saved' once you have stored the admin login: ") {
						adminShown = true
					} else {
						p("Not confirmed: the next `can up` issues a new password and TOTP secret.\n")
					}
				} else {
					adminShown = true
				}
			} else {
				p("\nAdmin credentials were created but NOT shown: this is not a terminal. Run `can up` again in a terminal (it issues new ones), or pass --print-secrets.\n")
			}
		} else if lr.AdminNote != "" {
			p("\nAdmin: %s. Lost it? kubectl -n can-system exec -i deploy/can-core -- /can-core admin reset-password --rotate-totp (password on stdin).\n", lr.AdminNote)
		}
		if lr.Kit != nil {
			switch {
			case f.kitOut != "":
				p("\nRecovery kit written, encrypted, to %s. Keep it and its passphrase away from this server.\n", f.kitOut)
			case showSecrets && tty && !f.printSecrets:
				if err := backup.PrintOnce(cmd.InOrStdin(), raw, lr.Kit); err == nil {
					kitShown = true
				} else {
					p("The recovery kit was not acknowledged; `can up` will offer it again.\n")
				}
			case showSecrets:
				p("\n%s", lr.Kit.Text())
				kitShown = true
			}
		}
		p("\nPhases:\n")
		for _, ph := range res.Phases {
			status := "done"
			if ph.Skipped {
				status = "skipped"
			}
			p("  %-8s %-8s %6.1fs\n", ph.Name, status, ph.Seconds)
		}
		p("\nNext: add DNS records for your domain if you have not (above), then `can up --domain-live`; connect email (docs/email-policy.md); `can status` shows health.\n")
	}
	if ew.err != nil {
		return t.fail(fmt.Errorf("writing the summary to stdout failed (%v): the credentials and kit are not counted as shown; run `can up` again", ew.err))
	}
	if adminShown {
		if err := launch.MarkAdminShown(t.store); err != nil {
			return t.fail(err)
		}
	}
	if kitShown {
		if err := launch.MarkKitShown(t.store); err != nil {
			return t.fail(err)
		}
	}
	return nil
}

func orNone(s []string) []string {
	if len(s) == 0 {
		return []string{"(none yet)"}
	}
	return s
}

func kitPassphrase(f *stackFlags) (string, error) {
	ask := newPassphraseSource(f.kitPassFile)
	pass, err := ask("New kit passphrase: ")
	if err != nil {
		return "", err
	}
	if f.kitPassFile == "" {
		again, err := ask("Confirm passphrase: ")
		if err != nil {
			return "", err
		}
		if again != pass {
			return "", errors.New("passphrases do not match; the kit was not written")
		}
	}
	if len(pass) < 8 {
		return "", errors.New("the kit passphrase must be at least 8 characters")
	}
	return pass, nil
}

func confirm(in io.Reader, out io.Writer, prompt string) bool {
	r := bufio.NewReader(in)
	for i := 0; i < 3; i++ {
		fmt.Fprint(out, prompt)
		l, _ := r.ReadString('\n')
		if strings.EqualFold(strings.TrimSpace(l), "saved") {
			return true
		}
	}
	return false
}

// emitSecretJSON writes the result as one JSON document. Unlike every other output it is NOT run
// through the redactor when --print-secrets is given: the credentials are the point.
func (t *target) emitSecretJSON(cmd *cobra.Command, res *upResult, secrets bool) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	s := string(b)
	if !secrets {
		s = t.redact.Redact(s)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), s)
	return err
}

// --- seams' defaults --------------------------------------------------------------------------

func defaultAccess(ctx context.Context, tgt provider.Target) (func(), error) {
	tun, err := provider.OpenTunnel(ctx, tgt, provider.DefaultTunnelPort, 30*time.Second)
	if err != nil {
		return nil, err
	}
	return tun.Close, nil
}

// defaultKube pins both the kubeconfig and its context (defence in depth: never the operator's current context).
func defaultKube(kubeconfig, context string) launch.Kube {
	return launch.ExecKube{Kubeconfig: kubeconfig, Context: context}
}

func defaultProbe(ctx context.Context, url string) error {
	deadline := time.Now().Add(60 * time.Second)
	var last error
	for {
		c := http.Client{Timeout: 5 * time.Second}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusNotFound {
				return nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		last = err
		if time.Now().After(deadline) || ctx.Err() != nil {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func defaultIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) //nolint:gosec // fd fits in int
}

var defaultLookPath = exec.LookPath

// stackChecks reports the company stack: the recorded phases, and (through the SSH tunnel) the
// deployments. A tunnel that cannot be opened is a failed check, not a silent pass. The strings are
// warnings: modules can-core could not install or upgrade.
func (t *target) stackChecks(ctx context.Context) ([]provider.Check, bool, []string) {
	tgt, err := provider.TargetFor(t.spec, t.store, t.deps)
	if err != nil {
		return []provider.Check{{Name: "stack", Detail: err.Error()}}, false, nil
	}
	closeTunnel, err := lifecycle.access(ctx, tgt)
	if err != nil {
		cs, _ := launch.Status(ctx, t.store, unreachable{err})
		return cs, false, nil
	}
	defer closeTunnel()
	kube := lifecycle.kube(provider.KubeconfigPath(t.dir), kubeName(t.company.Name))
	checks, ok := launch.Status(ctx, t.store, kube)
	var warnings []string
	for _, c := range checks {
		if c.Name == "can-core" && c.OK {
			warnings = launch.ModuleWarnings(ctx, kube)
		}
	}
	return checks, ok, warnings
}

// unreachable is a Kube that reports why the cluster cannot be queried.
type unreachable struct{ err error }

func (u unreachable) Run(context.Context, []byte, ...string) (string, error) {
	return "", fmt.Errorf("the SSH tunnel to the API could not be opened: %w", u.err)
}

// removeBackupTimer undoes the host side of the backup install (`can down`, ssh provider only: the
// cloud providers delete the machine). The kit-dependent credentials go with it.
func (t *target) removeBackupTimer(ctx context.Context, out io.Writer) error {
	st, err := t.store.Load()
	if err != nil || st.Launch == nil || !st.Launch.BackupInstalled {
		return err
	}
	tgt, err := provider.TargetFor(t.spec, t.store, t.deps)
	if err != nil {
		return err
	}
	runner := t.deps.Runner
	if runner == nil {
		runner = func(tg provider.Target) provider.Runner { return provider.NewSSHRunner(tg) }
	}
	fmt.Fprintln(out, "removing the backup timer and its credentials from the server")
	s := backup.DefaultSettings()
	script := fmt.Sprintf(`set -u
systemctl disable --now can-backup.timer >/dev/null 2>&1 || true
rm -f /etc/systemd/system/can-backup.service /etc/systemd/system/can-backup.timer /etc/systemd/system/can-backup-failure.service '%s'
rm -rf '%s'
systemctl daemon-reload >/dev/null 2>&1 || true
echo CAN_BACKUP_REMOVED
`, s.ScriptPath, s.ConfigDir)
	res, err := runner(tgt).Run(ctx, script)
	if err != nil || !strings.Contains(res, "CAN_BACKUP_REMOVED") {
		return fmt.Errorf("could not remove the backup timer from the server: %v %s", err, strings.TrimSpace(res))
	}
	return nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// errWriter remembers the first write error, so a secret that did not reach the operator (a closed
// pipe, a full disk) is never counted as shown.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(b []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(b)
	if err != nil {
		e.err = err
	}
	return n, err
}
