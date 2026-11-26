package launch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/canbin"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/routing"
	"github.com/dodwmd/companyinacan/core/site"
)

// Phase ids, in the order Run executes them. The ids are the keys of .can/state.json
// launch.phases, and what `can status` and the docs call them.
const (
	PhaseBase    = "base"
	PhaseImages  = "images"
	PhaseCompany = "company"
	PhaseRouting = "routing"
	PhaseCore    = "core"
	PhaseAdmin   = "admin"
	PhaseSite    = "site"
	PhaseRoutes  = "routes"
	PhaseCLI     = "cli"
	PhaseBackup  = "backup"
	PhaseReady   = "ready"
)

// Timeouts bound every wait; nothing in `can up` waits forever.
type Timeouts struct {
	Rollout time.Duration // a Deployment to become available
	CRD     time.Duration // k3s registering its CRDs after the node is Ready
	Module  time.Duration // can-core installing the forms module (reconcile interval + image + pod)
}

// DefaultTimeouts are the budget `can up` allows each wait.
func DefaultTimeouts() Timeouts {
	return Timeouts{Rollout: 3 * time.Minute, CRD: 3 * time.Minute, Module: 5 * time.Minute}
}

const pollEvery = 5 * time.Second

// BackupInstall asks for the host backup timer and the recovery kit.
type BackupInstall struct {
	// Input holds the bucket and credentials. Its Password is ignored: the kit gets a fresh restic
	// password, or on resume the one already on the host.
	Input    backup.KitInput
	Mods     map[string]*config.Module
	Settings backup.Settings
}

// Options describe the company being launched.
type Options struct {
	Dir         string
	Company     *config.Company
	CompanyYAML []byte // the validated company.yaml, as on disk
	Images      ImageSet
	Routes      RouteOptions
	ACMEEmail   string
	AdminUser   string // default admin
	// DefaultBundle adds the forms module to the pushed company when company.yaml does not mention it.
	DefaultBundle bool
	// SiteForms are the forms of the forms module, for site.contact; nil means the shipped ones.
	SiteForms []config.HostedForm
	// ForceCompany overwrites a company.yaml in the cluster that changed since `can up` last pushed it.
	ForceCompany bool
	SiteBaseURL  string       // canonical origin baked into the site (sitemap, canonical links)
	SMTP         *SMTPRelay   // nil leaves the relay Secret as it is
	IMAP         *SupportIMAP // nil leaves the support mailbox Secret as it is
	Backup       *BackupInstall
	// CLI supplies the `can` installed on the server (specs/061-can-on-server). Required when
	// Backup is set and there is a server.
	CLI canbin.Source
	// Mods are the manifests of the company's modules that could be read, for backup warnings.
	Mods     map[string]*config.Module
	Timeouts Timeouts
}

// Deps are the seams.
type Deps struct {
	Kube     Kube
	Loader   ImageLoader
	Host     provider.Runner // the server, for the backup timer; nil when there is none to reach
	Store    *provider.Store
	Redactor *provider.Redactor
	Rand     io.Reader // default crypto/rand
	Out      io.Writer
	Sleep    func(ctx context.Context, d time.Duration)
	Now      func() time.Time
}

// Credentials are the one-time admin login. They live in memory only.
type Credentials struct {
	Username   string
	Password   string
	TOTPSecret string
	TOTPURI    string
}

// PhaseResult is one line of the run report.
type PhaseResult struct {
	Name     string
	Skipped  bool
	Note     string
	Duration time.Duration
}

// Result is what Run hands back for the summary.
type Result struct {
	Phases      []PhaseResult
	Admin       *Credentials // nil when no new credentials were issued
	AdminNote   string
	Kit         *backup.Kit // nil when none is to be shown
	BundleAdded bool
	SiteSHA256  string
	FormsRouted bool
	// CLI is the `can` on the server; nil when it was not (or could not be) installed.
	CLI *canbin.Result
	// CompanyYAML is the document pushed to the cluster (with the default bundle, if it was added).
	CompanyYAML []byte
	Warnings    []string
	Total       time.Duration
}

// PhaseError says which phase failed, how to look, and how to resume.
type PhaseError struct {
	Phase string
	Err   error
}

func (e *PhaseError) Error() string {
	return fmt.Sprintf("phase %q failed: %v\n  look:   %s\n  resume: run `can up` again; finished phases are skipped or re-applied harmlessly, nothing is wiped",
		e.Phase, e.Err, hints[e.Phase])
}

// Unwrap exposes the cause.
func (e *PhaseError) Unwrap() error { return e.Err }

var hints = map[string]string{
	PhaseBase:    "kubectl --kubeconfig .can/kubeconfig get pods -A  (is the SSH tunnel up? `can status`)",
	PhaseImages:  "rebuild the tarballs with `make images` and check free disk on the server (df -h /var/lib/rancher); for a release manifest check the server can reach the registry",
	PhaseCompany: "kubectl -n can-system get configmap company -o yaml",
	PhaseRouting: "kubectl -n can-system get configmap can-routing -o yaml",
	PhaseCore:    "kubectl -n can-system logs deploy/can-core; kubectl -n can-system describe pod -l app.kubernetes.io/name=can-core",
	PhaseAdmin:   "kubectl -n can-system logs deploy/can-core; the admin is created with `can-core admin bootstrap` inside the pod",
	PhaseSite:    "kubectl -n can-system describe pod -l app.kubernetes.io/name=site; kubectl -n can-system logs deploy/site -c extract",
	PhaseRoutes:  "kubectl -n can-system logs deploy/can-core (can-core installs the forms module); kubectl -n mod-forms get pods",
	PhaseCLI:     "ssh to the server and check /usr/local/bin/can and the free disk space there",
	PhaseBackup:  "the can binary for the server (--release-manifest, --can-binary or a source checkout with Go); check that the server can reach github.com (pinned restic download) and apt (sqlite3), and the bucket endpoint and credentials; journalctl -u can-backup on the server",
	PhaseReady:   "kubectl -n can-system get pods; kubectl -n mod-forms get pods",
}

type runner struct {
	o   Options
	d   Deps
	res *Result
	n   int
	tot int
}

func (r *runner) say(format string, a ...any) {
	if r.d.Out != nil {
		fmt.Fprintf(r.d.Out, format+"\n", a...)
	}
}

func (r *runner) sleep(ctx context.Context, dur time.Duration) {
	if r.d.Sleep != nil {
		r.d.Sleep(ctx, dur)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(dur):
	}
}

func (r *runner) kube(ctx context.Context, stdin []byte, args ...string) (string, error) {
	return r.d.Kube.Run(ctx, stdin, args...)
}

func (r *runner) now() time.Time {
	if r.d.Now != nil {
		return r.d.Now()
	}
	return time.Now()
}

func secs(d time.Duration) string { return fmt.Sprintf("--timeout=%ds", int(d.Seconds())) }

// poll runs get until it succeeds, at most timeout/pollEvery times: the bound is a count, so a
// fake clock cannot make it loop forever.
func (r *runner) poll(ctx context.Context, timeout time.Duration, get func() error) error {
	tries := int(timeout/pollEvery) + 1
	var err error
	for i := 0; i < tries; i++ {
		if err = get(); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if i < tries-1 {
			r.sleep(ctx, pollEvery)
		}
	}
	return fmt.Errorf("gave up after %s: %w", timeout, err)
}

func (r *runner) phase(ctx context.Context, name, title string, fn func() (skipped bool, note string, err error)) error {
	r.n++
	start := r.now()
	r.say("[%d/%d] %s", r.n, r.tot, title)
	skipped, note, err := fn()
	pr := PhaseResult{Name: name, Skipped: skipped, Note: note, Duration: r.now().Sub(start)}
	if err != nil {
		if r.d.Redactor != nil {
			err = r.d.Redactor.Err(err)
		}
		return &PhaseError{Phase: name, Err: err}
	}
	if err := r.d.Store.Mutate(func(s *provider.State) error {
		l := launchOf(s)
		l.Phases[name] = r.now().UTC().Format(time.RFC3339)
		return nil
	}); err != nil {
		return &PhaseError{Phase: name, Err: fmt.Errorf("record checkpoint: %w", err)}
	}
	r.res.Phases = append(r.res.Phases, pr)
	verb := "done"
	if skipped {
		verb = "already done, skipped"
	}
	if note != "" {
		verb += " (" + note + ")"
	}
	r.say("      %s in %s", verb, pr.Duration.Round(100*time.Millisecond))
	return nil
}

func launchOf(s *provider.State) *provider.LaunchState {
	if s.Launch == nil {
		s.Launch = &provider.LaunchState{}
	}
	if s.Launch.Phases == nil {
		s.Launch.Phases = map[string]string{}
	}
	return s.Launch
}

// MarkAdminShown records that the operator has the admin credentials (after the summary printed
// them), so no later run ever replaces them.
func MarkAdminShown(st *provider.Store) error {
	return st.Mutate(func(s *provider.State) error { launchOf(s).AdminCredsShown = true; return nil })
}

// MarkKitShown records that the recovery kit was handed over; it is never shown again.
func MarkKitShown(st *provider.Store) error {
	return st.Mutate(func(s *provider.State) error { launchOf(s).KitShown = true; return nil })
}

func (r *runner) formsWanted() bool {
	if r.res.BundleAdded {
		return true
	}
	e, ok := r.o.Company.Modules["forms"]
	return ok && e.IsEnabled()
}

// Run executes the in-cluster phases against whatever cluster d.Kube points at. It is the same
// code for a real server (through the SSH tunnel) and for the k3d cluster in CI. Re-running it is
// safe at any point: applies converge, and the steps that are not idempotent (loading images,
// creating the admin, installing the backup timer) are guarded by the checkpoints in state.json.
func Run(ctx context.Context, o Options, d Deps) (*Result, error) {
	if o.Timeouts == (Timeouts{}) {
		o.Timeouts = DefaultTimeouts()
	}
	if o.AdminUser == "" {
		o.AdminUser = "admin"
	}
	if d.Rand == nil {
		d.Rand = rand.Reader
	}
	r := &runner{o: o, d: d, res: &Result{}}
	started := r.now()

	body := o.CompanyYAML
	if o.DefaultBundle {
		nb, added, err := WithDefaultBundle(body)
		if err != nil {
			return nil, &PhaseError{Phase: PhaseCompany, Err: err}
		}
		body, r.res.BundleAdded = nb, added
	}
	core := CoreOptions{Image: o.Images.Ref(ImageCore), ImageSHA256: o.Images.Images[ImageCore].SHA256, CompanyName: o.Company.Name, Domain: o.Company.Domain, CookieSecure: o.Routes.TLS}

	steps := []struct {
		name, title string
		fn          func() (bool, string, error)
	}{
		{PhaseBase, "platform base: namespaces, network policies, RBAC, admission policy, Traefik", func() (bool, string, error) { return false, "", r.base(ctx) }},
		{PhaseImages, "container images", func() (bool, string, error) { return r.images(ctx) }},
		{PhaseCompany, "company.yaml as ConfigMap can-system/company", func() (bool, string, error) { return false, "", r.company(ctx, body) }},
		{PhaseRouting, "routing facts as ConfigMap can-system/can-routing", func() (bool, string, error) { return false, "", r.routingFacts(ctx) }},
		{PhaseCore, "can-core", func() (bool, string, error) { return false, "", r.core(ctx, core) }},
		{PhaseAdmin, "admin account", func() (bool, string, error) { return r.admin(ctx) }},
		{PhaseSite, "build and publish the site", func() (bool, string, error) { return false, "", r.site(ctx) }},
		{PhaseRoutes, "routes (site, admin, /_forms)", func() (bool, string, error) { return false, "", r.routes(ctx) }},
	}
	// The server's `can` is part of the backup: the backup phase installs it, and without the
	// backup flags it is still kept up to date.
	if o.Backup == nil && d.Host != nil && o.CLI != nil {
		st, err := d.Store.Load()
		if err != nil {
			return nil, err
		}
		name, title := PhaseCLI, "can CLI on the server"
		if st.Launch != nil && st.Launch.BackupInstalled {
			name, title = PhaseBackup, "can CLI for the server's backup timer"
		}
		steps = append(steps, struct {
			name, title string
			fn          func() (bool, string, error)
		}{name, title, func() (bool, string, error) { return r.cli(ctx, name == PhaseBackup) }})
	}
	if o.Backup != nil {
		steps = append(steps, struct {
			name, title string
			fn          func() (bool, string, error)
		}{PhaseBackup, "backup timer and recovery kit", func() (bool, string, error) { return r.backup(ctx) }})
	}
	steps = append(steps, struct {
		name, title string
		fn          func() (bool, string, error)
	}{PhaseReady, "wait for readiness", func() (bool, string, error) { return false, "", r.ready(ctx) }})

	r.tot = len(steps)
	// A run in progress is not "ready", whatever an earlier run recorded.
	if err := d.Store.Mutate(func(s *provider.State) error { delete(launchOf(s).Phases, PhaseReady); return nil }); err != nil {
		return nil, err
	}
	for _, s := range steps {
		if err := ctx.Err(); err != nil {
			return r.res, err
		}
		if err := r.phase(ctx, s.name, s.title, s.fn); err != nil {
			r.res.Total = r.now().Sub(started)
			return r.res, err
		}
	}
	r.res.Total = r.now().Sub(started)
	return r.res, nil
}

func (r *runner) apply(ctx context.Context, manifest []byte, serverSide bool) (string, error) {
	args := []string{"apply", "-f", "-"}
	if serverSide {
		args = append(args, "--server-side", "--field-manager=can-cli", "--force-conflicts")
	}
	return r.kube(ctx, manifest, args...)
}

func (r *runner) base(ctx context.Context) error {
	const crd = "crd/helmchartconfigs.helm.cattle.io"
	// k3s's helm-controller registers this CRD a little after the node is Ready.
	if err := r.poll(ctx, r.o.Timeouts.CRD, func() error { _, err := r.kube(ctx, nil, "get", crd); return err }); err != nil {
		return fmt.Errorf("k3s did not register %s: %w", crd, err)
	}
	if _, err := r.kube(ctx, nil, "wait", "--for=condition=Established", crd, "--timeout=60s"); err != nil {
		return err
	}
	m, err := BaseManifests(BaseOptions{ACMEEmail: r.o.ACMEEmail})
	if err != nil {
		return err
	}
	out, err := r.apply(ctx, m, false)
	if err != nil {
		return err
	}
	// CoreDNS only picks up coredns-custom on restart; do it when that ConfigMap actually changed,
	// not on every re-run (a restart is a brief DNS blip).
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "coredns-custom") && !strings.Contains(l, "unchanged") {
			if _, err := r.kube(ctx, nil, "-n", "kube-system", "rollout", "restart", "deployment/coredns"); err != nil {
				return err
			}
			_, err := r.kube(ctx, nil, "-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=120s")
			return err
		}
	}
	return nil
}

func (r *runner) images(ctx context.Context) (bool, string, error) {
	fp := r.o.Images.Fingerprint()
	st, err := r.d.Store.Load()
	if err != nil {
		return false, "", err
	}
	if len(r.o.Images.Upload()) == 0 {
		return false, "from the release manifest; the server pulls them by digest", nil
	}
	if st.Launch != nil && st.Launch.ImagesFingerprint == fp {
		return true, "same image set already on the server", nil
	}
	if r.d.Loader == nil {
		return false, "", errors.New("no way to reach the server to upload images")
	}
	if err := r.d.Loader.Load(ctx, r.o.Images, func(s string) { r.say("      %s", s) }); err != nil {
		return false, "", err
	}
	return false, "", r.d.Store.Mutate(func(s *provider.State) error { launchOf(s).ImagesFingerprint = fp; return nil })
}

func (r *runner) company(ctx context.Context, body []byte) error {
	r.res.CompanyYAML = body
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	st, err := r.d.Store.Load()
	if err != nil {
		return err
	}
	last := ""
	if st.Launch != nil {
		last = st.Launch.CompanySHA256
	}
	// What is in the cluster now? Changes made there (the admin UI, `can apply`) must not be
	// silently overwritten by a re-run of `can up`.
	cur, gerr := r.kube(ctx, nil, "-n", SystemNS, "get", "configmap", "company", "--ignore-not-found", "-o", `jsonpath={.data.company\.yaml}`)
	if gerr != nil {
		// NotFound is not an error with --ignore-not-found, so this is the API, the tunnel or RBAC:
		// applying blind could overwrite a document we could not read.
		return fmt.Errorf("cannot read the company.yaml in the cluster, so cannot tell whether applying would overwrite changes: %w", gerr)
	}
	if gerr == nil && strings.TrimSpace(cur) != "" {
		cs := sha256.Sum256([]byte(cur))
		have := hex.EncodeToString(cs[:])
		switch {
		case have == want:
			return r.d.Store.Mutate(func(s *provider.State) error { launchOf(s).CompanySHA256 = want; return nil })
		case have != last && !r.o.ForceCompany && !r.seenCompany(ctx, st, have):
			return errors.New("the company.yaml in the cluster differs from the one `can up` last pushed (it was changed through the admin UI or `can apply`); applying this directory's company.yaml would discard those changes. Bring them into this directory with `can pull` (`can pull --out other.yaml` to compare first), or run `can up --force-company` to overwrite them")
		}
	}
	cm, err := CompanyConfigMap(string(body))
	if err != nil {
		return err
	}
	// Before can-core starts: a ConfigMap volume is mounted with its content at pod start, whereas
	// an update reaches a running pod only after the kubelet's next sync (a minute or more).
	if _, err = r.apply(ctx, []byte(cm), true); err != nil {
		return err
	}
	return r.d.Store.Mutate(func(s *provider.State) error { launchOf(s).CompanySHA256 = want; return nil })
}

// seenCompany reports whether `can apply` or `can pull` of this directory last saw the document
// the cluster has (its digest is have).
func (r *runner) seenCompany(ctx context.Context, st *provider.State, have string) bool {
	if st.Launch == nil || len(st.Launch.CompanyBases) == 0 {
		return false
	}
	uid, err := r.kube(ctx, nil, "-n", SystemNS, "get", "configmap", "company", "-o", "jsonpath={.metadata.uid}")
	return err == nil && st.Launch.CompanyBases[strings.TrimSpace(uid)] == have
}

func (r *runner) facts() routing.Config {
	return routing.Config{Domain: r.o.Company.Domain, TLS: r.o.Routes.TLS, SiteHosts: r.o.Routes.SiteHosts, CatchAll: r.o.Routes.CatchAll, NoEdgeLimit: r.o.Routes.NoEdgeLimit, NoEdgeLimitReason: r.o.Routes.NoEdgeLimitReason}
}

// routingFacts writes which hosts serve the site, and whether over TLS, as ConfigMap
// can-system/can-routing: can-core routes modules from it and may not write it itself. It runs on
// every `can up`, whether or not company.yaml changed: the flags (--domain-live) and the version of
// can change it, and without it the admission policy refuses every module route.
func (r *runner) routingFacts(ctx context.Context) error {
	facts := r.facts()
	if facts.NoEdgeLimitReason == routing.ReasonProxied {
		unknown, err := r.edgeLimitsOffForUnknownReason(ctx)
		if err != nil {
			return err
		}
		if unknown {
			facts.NoEdgeLimitReason = ""
		}
	}
	m, err := facts.Manifest()
	if err != nil {
		return err
	}
	_, err = r.apply(ctx, m, true)
	return err
}

// edgeLimitsOffForUnknownReason reports routing facts written by an older can with the limits off:
// it may have had --no-edge-limits as well as dns.proxied, so the reason is not guessed.
func (r *runner) edgeLimitsOffForUnknownReason(ctx context.Context) (bool, error) {
	out, err := r.kube(ctx, nil, "-n", routing.Namespace, "get", "configmap", routing.ConfigMapName, "--ignore-not-found", "-o", "json")
	if err != nil || strings.TrimSpace(out) == "" {
		return false, err
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &cm); err != nil {
		return false, fmt.Errorf("ConfigMap %s/%s is not readable: %w", routing.Namespace, routing.ConfigMapName, err)
	}
	return cm.Data["edgeLimit"] == "off" && cm.Data["edgeLimitReason"] == "", nil
}

// hostDumpsDir makes the host directory behind the can-dumps volume belong to can-core's user.
// It runs on every `can up`, with or without backups: can-core writes its dumps there anyway,
// and servers bootstrapped by an older `can` have it root-owned.
func (r *runner) hostDumpsDir(ctx context.Context) error {
	if r.d.Host == nil {
		return nil
	}
	dir := backup.DefaultSettings().DumpsDir
	if _, err := r.d.Host.Run(ctx, "install -d -o 65532 -g 65532 -m 0700 "+shq(dir)+"\n"); err != nil {
		return fmt.Errorf("prepare %s on the server: %w", dir, err)
	}
	st, err := r.d.Store.Load()
	if err != nil {
		return err
	}
	if r.o.Backup == nil && st.Launch != nil && st.Launch.BackupInstalled {
		if ok, err := r.timerOnHost(ctx, backup.DefaultSettings()); err != nil {
			return err
		} else if !ok {
			r.res.Warnings = append(r.res.Warnings, "THIS SERVER IS NOT BEING BACKED UP: it has no working backup timer, although this directory installed one (a replaced or restored server). Run `can up` again with --backup-endpoint, --backup-bucket and --backup-credentials-file")
		}
		if st.Launch.BackupVersion < BackupInstallVersion {
			r.res.Warnings = append(r.res.Warnings, "the backup script on the server was installed by an older version: run `can up` again with --backup-endpoint, --backup-bucket and --backup-credentials-file to update it")
		}
		if err := r.warnBackupLimits(ctx, r.o.Mods, backup.DefaultSettings()); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) core(ctx context.Context, c CoreOptions) error {
	if err := r.hostDumpsDir(ctx); err != nil {
		return err
	}
	m, err := CoreManifests(c)
	if err != nil {
		return err
	}
	if err := r.relaySecret(ctx); err != nil {
		return err
	}
	if err := r.imapSecret(ctx); err != nil {
		return err
	}
	if _, err := r.apply(ctx, m, false); err != nil {
		return err
	}
	_, err = r.kube(ctx, nil, "-n", SystemNS, "rollout", "status", "deploy/can-core", secs(r.o.Timeouts.Rollout))
	return err
}

const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // 31 symbols: no look-alikes (0 O 1 l I)

// NewPassword returns a random password of n characters drawn without modulo bias.
func NewPassword(rd io.Reader, n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, 64)
	for len(out) < n {
		if _, err := io.ReadFull(rd, buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) < 256-256%len(alphabet) && len(out) < n {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(out), nil
}

var (
	reTOTP = regexp.MustCompile(`TOTP secret \(shown once\): ([A-Z2-7]+)`)
	reURI  = regexp.MustCompile(`Provisioning URI: (\S+)`)
)

func (r *runner) exec(ctx context.Context, stdin string, args ...string) (string, error) {
	full := append([]string{"-n", SystemNS, "exec", "-i", "deploy/can-core", "--", "/can-core", "admin"}, args...)
	return r.kube(ctx, []byte(stdin+"\n"), full...)
}

func (r *runner) admin(ctx context.Context) (bool, string, error) {
	st, err := r.d.Store.Load()
	if err != nil {
		return false, "", err
	}
	if st.Launch != nil && st.Launch.AdminCreated && st.Launch.AdminCredsShown {
		r.res.AdminNote = "the admin account was created by an earlier run and its credentials were shown then"
		return true, "credentials already handed over", nil
	}
	if st.Launch == nil || !st.Launch.AdminCreated {
		// A server restored from a backup has the accounts it had before, whatever they are
		// called: never create or reset one there.
		s, serr := r.kube(ctx, nil, "-n", SystemNS, "exec", "deploy/can-core", "--", "/can-core", "restore", "status")
		if serr == nil && strings.HasPrefix(strings.TrimSpace(s), "normal restored ") {
			r.res.AdminNote = "this server was restored from a backup: the admin logins are the ones from before the restore"
			return true, "restored accounts kept", nil
		}
		if serr == nil && strings.HasPrefix(strings.TrimSpace(s), "restoring ") {
			return false, "", errors.New("this server is in the middle of a restore: finish `can restore` on it first")
		}
	}
	pw, err := NewPassword(r.d.Rand, 24)
	if err != nil {
		return false, "", err
	}
	if r.d.Redactor != nil {
		r.d.Redactor.Add(pw)
	}
	// The password goes in on stdin: it is in neither the argument vector (visible in `ps`) nor a
	// Kubernetes Secret that would outlive this command.
	out, err := r.exec(ctx, pw, "bootstrap", "--username", r.o.AdminUser)
	note := ""
	if err != nil && strings.Contains(err.Error()+out, "already exists") {
		// Created by a run that stopped before showing the credentials: nobody holds them, so issue new ones.
		out, err = r.exec(ctx, pw, "reset-password", "--username", r.o.AdminUser, "--rotate-totp")
		note = "re-issued: an earlier run created the account but never showed its credentials"
	}
	if err != nil {
		return false, "", fmt.Errorf("can-core admin bootstrap: %w", err)
	}
	m := reTOTP.FindStringSubmatch(out)
	if m == nil {
		return false, "", errors.New("can-core did not print a TOTP secret; the account state is unknown (run `can-core admin reset-password --rotate-totp` in the pod)")
	}
	creds := &Credentials{Username: r.o.AdminUser, Password: pw, TOTPSecret: m[1]}
	if u := reURI.FindStringSubmatch(out); u != nil {
		creds.TOTPURI = u[1]
	}
	if r.d.Redactor != nil {
		r.d.Redactor.Add(creds.TOTPSecret)
		if creds.TOTPURI != "" {
			r.d.Redactor.Add(creds.TOTPURI)
		}
	}
	if err := r.d.Store.Mutate(func(s *provider.State) error {
		l := launchOf(s)
		l.AdminUser, l.AdminCreated = r.o.AdminUser, true
		return nil
	}); err != nil {
		return false, "", err
	}
	r.res.Admin = creds
	return false, note, nil
}

func (r *runner) site(ctx context.Context) error {
	base := r.o.SiteBaseURL
	if base == "" {
		base = "https://" + r.o.Company.Domain
	}
	// The default bundle is what is pushed, not what company.yaml on disk says.
	absent := config.AbsentOff
	if r.o.DefaultBundle {
		absent = config.AbsentAdded
	}
	a, err := BuildSiteArchive(r.o.Dir, site.Options{BaseURL: base, Forms: r.o.SiteForms, AbsentForms: absent}, MaxSiteArchive)
	if err != nil {
		return err
	}
	r.res.Warnings = append(r.res.Warnings, a.Warnings...)
	if _, err := r.apply(ctx, []byte(SiteContentConfigMap(a)), true); err != nil {
		return err
	}
	m, err := SiteManifests(r.o.Images.Ref(ImageStatic), a.SHA256, r.o.Images.Images[ImageStatic].SHA256)
	if err != nil {
		return err
	}
	if _, err := r.apply(ctx, m, false); err != nil {
		return err
	}
	if _, err := r.kube(ctx, nil, "-n", SystemNS, "rollout", "status", "deploy/site", secs(r.o.Timeouts.Rollout)); err != nil {
		return err
	}
	r.res.SiteSHA256 = a.SHA256
	return r.d.Store.Mutate(func(s *provider.State) error { launchOf(s).SiteSHA256 = a.SHA256; return nil })
}

func (r *runner) routes(ctx context.Context) error {
	m, err := SiteRoutes(r.o.Routes)
	if err != nil {
		return err
	}
	if _, err := r.apply(ctx, m, false); err != nil {
		return err
	}
	mode := "http"
	if r.o.Routes.TLS {
		mode = "https"
	}
	if err := r.d.Store.Mutate(func(s *provider.State) error { launchOf(s).Mode = mode; return nil }); err != nil {
		return err
	}
	if err := r.redirect(ctx); err != nil {
		return err
	}
	if !r.formsWanted() || !r.o.Company.Modules["forms"].Exposure().PublicOn() {
		return nil
	}
	// can-core installs the forms module on its next reconcile and then routes /_forms on the site
	// hosts (specs/040-module-routing); wait for both.
	if err := r.poll(ctx, r.o.Timeouts.Module, func() error {
		_, err := r.kube(ctx, nil, "-n", FormsNS, "get", "svc", "forms", "-o", "name")
		return err
	}); err != nil {
		return fmt.Errorf("the forms module was not installed by can-core in time: %w", err)
	}
	if _, err := r.kube(ctx, nil, "-n", FormsNS, "rollout", "status", "deploy/forms", secs(r.o.Timeouts.Rollout)); err != nil {
		return err
	}
	// A route from an earlier run is not enough: it must be the one these routing facts ask for.
	if err := r.poll(ctx, r.o.Timeouts.Module, func() error {
		out, err := r.kube(ctx, nil, "-n", FormsNS, "get", "ingressroutes.traefik.io", "forms-public", "-o", "json")
		if err != nil {
			return err
		}
		return checkFormsRoute([]byte(out), r.facts())
	}); err != nil {
		return fmt.Errorf("can-core did not route /_forms in time: %w", err)
	}
	r.res.FormsRouted = true
	return nil
}

func (r *runner) ready(ctx context.Context) error {
	if _, err := r.kube(ctx, nil, "-n", SystemNS, "wait", "--for=condition=Available", "deploy/can-core", "deploy/site", secs(r.o.Timeouts.Rollout)); err != nil {
		return err
	}
	if r.res.FormsRouted {
		if _, err := r.kube(ctx, nil, "-n", FormsNS, "wait", "--for=condition=Available", "deploy/forms", secs(r.o.Timeouts.Rollout)); err != nil {
			return err
		}
	}
	return nil
}

// checkFormsRoute reports how the route differs from the one can-core makes for the facts.
func checkFormsRoute(raw []byte, c routing.Config) error {
	var got struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			EntryPoints []string `json:"entryPoints"`
			Routes      []struct {
				Match string `json:"match"`
			} `json:"routes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return fmt.Errorf("the route is not readable: %w", err)
	}
	var matches []string
	for _, r := range got.Spec.Routes {
		matches = append(matches, r.Match)
	}
	want := c.SiteMatches(routing.SitePrefixes["forms"])
	slices.Sort(matches)
	slices.Sort(want)
	switch {
	case got.Metadata.Labels["can.dev/route"] != "true":
		return errors.New("route forms-public is not the one can-core makes")
	case !slices.Equal(got.Spec.EntryPoints, []string{c.EntryPoint()}):
		return fmt.Errorf("route forms-public is on %v, want %s", got.Spec.EntryPoints, c.EntryPoint())
	case !slices.Equal(matches, want):
		return fmt.Errorf("route forms-public matches %v, want %v", matches, want)
	}
	return nil
}

// Status reports the checkpoints and, through kube, whether the deployments have available
// replicas. The bool is true only when everything is up. It never changes anything.
func Status(ctx context.Context, st *provider.Store, kube Kube) ([]provider.Check, bool) {
	s, err := st.Load()
	if err != nil {
		return []provider.Check{{Name: "stack", Detail: err.Error()}}, false
	}
	if s.Launch == nil || len(s.Launch.Phases) == 0 {
		return []provider.Check{{Name: "stack", Detail: "no company stack deployed yet; run `can up`"}}, false
	}
	_, ready := s.Launch.Phases[PhaseReady]
	detail := "all phases finished"
	if !ready {
		detail = "the last `can up` did not finish; run `can up` again to resume"
	}
	checks := []provider.Check{{Name: "phases", OK: ready, Detail: detail}}
	all := ready
	avail := func(ns, name string) provider.Check {
		out, err := kube.Run(ctx, nil, "-n", ns, "get", "deploy", name, "-o", "jsonpath={.status.availableReplicas}")
		out = strings.TrimSpace(out)
		switch {
		case err != nil:
			return provider.Check{Name: name, Detail: "cannot query the cluster: " + err.Error()}
		case out == "" || out == "0":
			return provider.Check{Name: name, Detail: "no available replica"}
		}
		return provider.Check{Name: name, OK: true, Detail: out + " available"}
	}
	list := []provider.Check{avail(SystemNS, "can-core"), avail(SystemNS, "site")}
	if _, ok := s.Launch.Phases[PhaseRoutes]; ok {
		list = append(list, avail(FormsNS, "forms"))
	}
	for _, c := range list {
		checks = append(checks, c)
		all = all && c.OK
	}
	return checks, all
}
