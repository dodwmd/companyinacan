package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/launch"
	"github.com/dodwmd/companyinacan/core/provider"
)

const (
	testPassword = "pw-Zk3-never-in-logs-0001"
	testTOTP     = "JBSWY3DPEHPK3PXP"
)

func testImagesDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	idx := `{"schema":1,"images":{`
	for i, n := range launch.ImageNames {
		body := "tar-" + n
		if err := os.WriteFile(filepath.Join(dir, n+".tar"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256([]byte(body))
		if i > 0 {
			idx += ","
		}
		idx += `"` + n + `":{"ref":"` + n + `:dev","file":"` + n + `.tar","sha256":"` + hex.EncodeToString(h[:]) + `"}`
	}
	if err := os.WriteFile(filepath.Join(dir, "images.json"), []byte(idx+"}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

type stackSpy struct {
	opts   launch.Options
	deps   launch.Deps
	called int
}

// useStack replaces the in-cluster seams: no kubectl, no tunnel, no network.
func useStack(t *testing.T, tty bool, res *launch.Result, err error) *stackSpy {
	t.Helper()
	spy := &stackSpy{}
	old := lifecycle
	lifecycle.lookPath = func(string) (string, error) { return "/usr/bin/kubectl", nil }
	lifecycle.access = func(context.Context, provider.Target) (func(), error) { return func() {}, nil }
	lifecycle.kube = func(string, string) launch.Kube { return nopKube{} }
	lifecycle.probe = func(context.Context, string) error { return nil }
	lifecycle.isTTY = func() bool { return tty }
	lifecycle.stack = func(_ context.Context, o launch.Options, d launch.Deps) (*launch.Result, error) {
		spy.called++
		spy.opts, spy.deps = o, d
		// like the real stack, leave a checkpoint behind
		_ = d.Store.Mutate(func(s *provider.State) error {
			if s.Launch == nil {
				s.Launch = &provider.LaunchState{}
			}
			if s.Launch.Phases == nil {
				s.Launch.Phases = map[string]string{"base": "2026-11-25T12:00:00Z"}
			}
			return nil
		})
		if d.Redactor != nil && res != nil && res.Admin != nil {
			d.Redactor.Add(res.Admin.Password)
			d.Redactor.Add(res.Admin.TOTPSecret)
		}
		return res, err
	}
	t.Cleanup(func() { lifecycle = old })
	return spy
}

type nopKube struct{}

func (nopKube) Run(context.Context, []byte, ...string) (string, error) { return "", nil }

func stackResult() *launch.Result {
	return &launch.Result{
		Phases: []launch.PhaseResult{{Name: "base", Duration: 2 * time.Second}, {Name: "core", Duration: 20 * time.Second}},
		Admin:  &launch.Credentials{Username: "admin", Password: testPassword, TOTPSecret: testTOTP, TOTPURI: "otpauth://totp/x"},
	}
}

func stackArgs(dir string, extra ...string) []string {
	return append([]string{"up", dir, "--images-from", testImagesDir2, "--modules-dir", "../../modules"}, extra...)
}

var testImagesDir2 string

func setup(t *testing.T) (dir string, r *cliRunner) {
	t.Helper()
	dir = sshCompany(t)
	r = newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	testImagesDir2 = testImagesDir(t)
	return dir, r
}

func TestUpPlanListsEveryStackPhase(t *testing.T) {
	dir, r := setup(t)
	code, out, errs := run(t, "up", dir, "--plan", "--modules-dir", "../../modules")
	if code != ExitOK || len(r.ran) != 0 {
		t.Fatalf("code=%d err=%q ran=%d", code, errs, len(r.ran))
	}
	for _, want := range []string{"images:", "platform base", "company.yaml as ConfigMap", "deploy can-core", "admin account", "build the site", "/_forms", "wait for", "one-time admin"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stack.backup") || strings.Contains(out, "backup timer") {
		t.Fatal("backup is not part of the plan unless asked for")
	}
}

func TestUpFailsClosedBeforeProvisioningWithoutImages(t *testing.T) {
	dir, r := setup(t)
	spy := useStack(t, false, stackResult(), nil)
	code, _, errs := run(t, "up", dir, "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(errs, "make images") || !strings.Contains(errs, "nothing has been provisioned") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if len(r.ran) != 0 || spy.called != 0 {
		t.Fatalf("no server work may happen before the preflight passes: ran=%d", len(r.ran))
	}
}

func TestUpFailsClosedWithoutKubectl(t *testing.T) {
	dir, r := setup(t)
	useStack(t, false, stackResult(), nil)
	lifecycle.lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	code, _, errs := run(t, stackArgs(dir)...)
	if code != ExitFailure || !strings.Contains(errs, "kubectl") || len(r.ran) != 0 {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestUpPrintsNoSecretsWithoutATerminal(t *testing.T) {
	dir, _ := setup(t)
	spy := useStack(t, false, stackResult(), nil)
	code, out, errs := run(t, stackArgs(dir)...)
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if spy.called != 1 {
		t.Fatal("the stack did not run")
	}
	if strings.Contains(out+errs, testPassword) || strings.Contains(out+errs, testTOTP) {
		t.Fatalf("secrets were printed without a terminal or --print-secrets:\n%s\n%s", out, errs)
	}
	if !strings.Contains(out, "NOT shown") {
		t.Fatalf("the operator must be told why:\n%s", out)
	}
	st, _ := provider.NewStore(dir).Load()
	if st.Launch != nil && st.Launch.AdminCredsShown {
		t.Fatal("credentials that were not shown must not be marked as shown")
	}
}

func TestUpJSONNeverPrintsSecretsUnlessAsked(t *testing.T) {
	dir, _ := setup(t)
	useStack(t, false, stackResult(), nil)
	code, out, _ := run(t, append(stackArgs(dir), "--output", "json")...)
	if code != ExitOK {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if strings.Contains(out, testPassword) || strings.Contains(out, testTOTP) {
		t.Fatalf("json holds a secret:\n%s", out)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil || v["admin_user"] != "admin" || v["phases"] == nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, ok := v["admin"]; ok {
		t.Fatal("no admin object without --print-secrets")
	}
}

func TestUpPrintSecretsEmitsCredentialsOnceAndMarksThemShown(t *testing.T) {
	dir, _ := setup(t)
	useStack(t, false, stackResult(), nil)
	code, out, errs := run(t, append(stackArgs(dir), "--output", "json", "--print-secrets")...)
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	var v struct {
		Admin struct {
			Username   string `json:"username"`
			Password   string `json:"password"`
			TOTPSecret string `json:"totp_secret"`
		} `json:"admin"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Admin.Password != testPassword || v.Admin.TOTPSecret != testTOTP || v.Admin.Username != "admin" {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(errs, testPassword) {
		t.Fatal("secrets must not reach stderr")
	}
	st, _ := provider.NewStore(dir).Load()
	if st.Launch == nil || !st.Launch.AdminCredsShown {
		t.Fatalf("shown credentials must be recorded: %+v", st.Launch)
	}
	raw, _ := os.ReadFile(provider.StatePath(dir))
	if strings.Contains(string(raw), testPassword) || strings.Contains(string(raw), testTOTP) {
		t.Fatal("state.json holds a secret")
	}
}

func TestUpTerminalAsksForConfirmation(t *testing.T) {
	dir, _ := setup(t)
	useStack(t, true, stackResult(), nil)
	// stdin is empty in run(): the confirmation cannot be given, so the credentials count as not handed over.
	code, out, _ := run(t, stackArgs(dir)...)
	if code != ExitOK || !strings.Contains(out, testPassword) || !strings.Contains(out, "Type 'saved'") || !strings.Contains(out, "Not confirmed") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	st, _ := provider.NewStore(dir).Load()
	if st.Launch != nil && st.Launch.AdminCredsShown {
		t.Fatal("an unconfirmed display must not mark the credentials shown")
	}
}

func TestUpStackFailureNamesThePhaseAndExitsOne(t *testing.T) {
	dir, _ := setup(t)
	useStack(t, false, stackResult(), &launch.PhaseError{Phase: "core", Err: os.ErrDeadlineExceeded})
	code, out, errs := run(t, stackArgs(dir)...)
	if code != ExitFailure || !strings.Contains(errs, `phase "core"`) || !strings.Contains(errs, "can up") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if strings.Contains(out, "Your company is up") {
		t.Fatal("a failed run must not print the success summary")
	}
}

func TestUpRoutesFollowTheInstantURLDecision(t *testing.T) {
	dir, _ := setup(t)
	spy := useStack(t, false, stackResult(), nil)
	if code, _, errs := run(t, stackArgs(dir, "--acme-email", "ops@acme.example")...); code != ExitOK {
		t.Fatal(errs)
	}
	ro := spy.opts.Routes
	if !ro.TLS || len(ro.SiteHosts) != 1 || ro.SiteHosts[0] != "203-0-113-7.sslip.io" || len(ro.AdminHosts) != 1 || ro.AdminHosts[0] != "admin.203-0-113-7.sslip.io" {
		t.Fatalf("https mode routes: %+v", ro)
	}
	if spy.opts.ACMEEmail != "ops@acme.example" || !spy.opts.DefaultBundle {
		t.Fatalf("%+v", spy.opts)
	}
	// sslip.io does not resolve: plain HTTP on the IP, public site only, admin through the tunnel.
	dir2 := sshCompany(t)
	useLifecycle(t, func() provider.Deps {
		return provider.Deps{Runner: func(provider.Target) provider.Runner { return newRunner() }}
	}, fixedResolver{})
	spy2 := useStack(t, false, stackResult(), nil)
	code, out, errs := run(t, stackArgs(dir2)...)
	if code != ExitOK {
		t.Fatal(errs)
	}
	ro = spy2.opts.Routes
	if ro.TLS || !ro.CatchAll || len(ro.AdminHosts) != 0 {
		t.Fatalf("fallback routes: %+v", ro)
	}
	if !strings.Contains(out, "port-forward") {
		t.Fatalf("the admin UI must be explained as tunnel-only:\n%s", out)
	}
	// --domain-live routes the company domain with TLS too.
	dir3 := sshCompany(t)
	spy3 := useStack(t, false, stackResult(), nil)
	if code, _, errs := run(t, stackArgs(dir3, "--domain-live")...); code != ExitOK {
		t.Fatal(errs)
	}
	if got := strings.Join(spy3.opts.Routes.SiteHosts, ","); !strings.Contains(got, "acme.example") || !spy3.opts.Routes.TLS {
		t.Fatalf("domain-live: %+v", spy3.opts.Routes)
	}
}

func TestBackupFlagsAreAllOrNothingAndKitNeedsADestination(t *testing.T) {
	dir, r := setup(t)
	useStack(t, false, stackResult(), nil)
	code, _, errs := run(t, stackArgs(dir, "--backup-bucket", "b", "--allow-broken-backup")...)
	if code != ExitFailure || !strings.Contains(errs, "--backup-endpoint") || len(r.ran) != 0 {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	creds := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(creds, []byte("AWS_ACCESS_KEY_ID=AKIAEXAMPLE\nAWS_SECRET_ACCESS_KEY=s3cr3t-value-0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	full := []string{"--backup-endpoint", "https://s3.example", "--backup-bucket", "b", "--backup-credentials-file", creds, "--allow-broken-backup"}
	code, _, errs = run(t, stackArgs(dir, full...)...)
	if code != ExitFailure || !strings.Contains(errs, "--kit-out") || !strings.Contains(errs, "git tree") || len(r.ran) != 0 {
		t.Fatalf("a kit with nowhere to go must stop before provisioning: code=%d err=%q", code, errs)
	}
	spy := useStack(t, false, stackResult(), nil)
	if code, _, errs = run(t, stackArgs(dir, append(full, "--print-secrets", "--output", "json")...)...); code != ExitOK {
		t.Fatal(errs)
	}
	if spy.opts.Backup == nil || spy.opts.Backup.Input.Bucket != "b" || spy.opts.Backup.Input.AccessKeyID != "AKIAEXAMPLE" {
		t.Fatalf("%+v", spy.opts.Backup)
	}
}

func TestKitOutIsEncryptedAndRefusedInsideAGitTree(t *testing.T) {
	dir, _ := setup(t)
	res := stackResult()
	spy := useStack(t, false, res, nil)
	creds := filepath.Join(t.TempDir(), "creds")
	_ = os.WriteFile(creds, []byte("AWS_ACCESS_KEY_ID=AKIAEXAMPLE\nAWS_SECRET_ACCESS_KEY=s3cr3t-value-0123456789\n"), 0o600)
	pass := filepath.Join(t.TempDir(), "pass")
	_ = os.WriteFile(pass, []byte("correct horse battery"), 0o600)
	full := []string{"--backup-endpoint", "https://s3.example", "--backup-bucket", "b", "--backup-credentials-file", creds, "--kit-passphrase-file", pass, "--allow-broken-backup"}

	// the fake stack hands back a kit, as the real one does after installing the timer
	lifecycle.stack = func(_ context.Context, o launch.Options, d launch.Deps) (*launch.Result, error) {
		spy.opts = o
		k, err := newTestKit(o)
		if err != nil {
			return nil, err
		}
		r := *res
		r.Kit = k
		return &r, nil
	}
	repo := filepath.Join(t.TempDir(), "repo")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	code, _, errs := run(t, stackArgs(dir, append(full, "--kit-out", filepath.Join(repo, "kit.enc"))...)...)
	if code != ExitFailure || !strings.Contains(errs, "git working tree") {
		t.Fatalf("a kit must never be written inside a git tree: code=%d err=%q", code, errs)
	}
	out := filepath.Join(t.TempDir(), "kit.enc")
	code, stdout, errs := run(t, stackArgs(dir, append(full, "--kit-out", out)...)...)
	if code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	b, err := os.ReadFile(out)
	if err != nil || strings.Contains(string(b), "s3cr3t-value") || strings.Contains(stdout, "s3cr3t-value") {
		t.Fatalf("the kit file must be encrypted: %v", err)
	}
	st, _ := provider.NewStore(dir).Load()
	if st.Launch == nil || !st.Launch.KitShown {
		t.Fatalf("a written kit counts as handed over: %+v", st.Launch)
	}
}

func TestStatusReportsStackAndDegradesWhenItIsDown(t *testing.T) {
	dir, r := setup(t)
	useStack(t, false, stackResult(), nil)
	if code, _, errs := run(t, stackArgs(dir)...); code != ExitOK {
		t.Fatal(errs)
	}
	_ = r
	code, out, _ := run(t, "status", dir, "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(out, "degraded") || !strings.Contains(out, "can-core") {
		t.Fatalf("the fake cluster reports nothing available: code=%d out=%q", code, out)
	}
	lifecycle.kube = func(string, string) launch.Kube { return allAvailable{} }
	st := provider.NewStore(dir)
	_ = st.Mutate(func(s *provider.State) error {
		s.Launch.Phases["ready"] = "2026-11-25T12:00:00Z"
		return nil
	})
	code, out, _ = run(t, "status", dir, "--modules-dir", "../../modules")
	if code != ExitOK || !strings.Contains(out, "[ok  ] can-core") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func newTestKit(o launch.Options) (*backup.Kit, error) {
	in := o.Backup.Input
	in.Password = ""
	return backup.NewKit(in)
}

func TestUpJSONOnATerminalWithoutPrintSecretsDoesNotMarkCredentialsShown(t *testing.T) {
	dir, _ := setup(t)
	useStack(t, true, stackResult(), nil)
	code, out, _ := run(t, append(stackArgs(dir), "--output", "json")...)
	if code != ExitOK {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if strings.Contains(out, testPassword) || strings.Contains(out, testTOTP) {
		t.Fatalf("json holds a secret:\n%s", out)
	}
	if !strings.Contains(out, "not shown") {
		t.Fatalf("the operator must be told:\n%s", out)
	}
	st, _ := provider.NewStore(dir).Load()
	if st.Launch != nil && st.Launch.AdminCredsShown {
		t.Fatal("credentials the operator never saw must stay re-issuable")
	}
}

func TestWriteBundleForceCompanyAndPinnedContext(t *testing.T) {
	dir, _ := setup(t)
	res := stackResult()
	res.BundleAdded = true
	res.CompanyYAML = []byte("schema: 1\nname: Acme Co\ndomain: acme.example\nprovider:\n  type: ssh\n  options:\n    host: 203.0.113.7\nmodules:\n  forms: {}\n")
	res.Warnings = []string{"BACKUPS DO NOT WORK YET"}
	spy := useStack(t, false, res, nil)
	var gotKubeconfig, gotCtx string
	lifecycle.kube = func(kc, c string) launch.Kube { gotKubeconfig, gotCtx = kc, c; return nopKube{} }
	code, out, errs := run(t, stackArgs(dir, "--write-bundle", "--force-company")...)
	if code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "company.yaml"))
	if !strings.Contains(string(b), "forms") || !spy.opts.ForceCompany {
		t.Fatalf("write-bundle/force-company not honoured: %s %+v", b, spy.opts)
	}
	if gotCtx != "acme-co" || !strings.HasSuffix(gotKubeconfig, filepath.Join(".can", "kubeconfig")) {
		t.Fatalf("kubectl must be pinned to the company's kubeconfig and context: %q %q", gotKubeconfig, gotCtx)
	}
	if !strings.Contains(out, "BACKUPS DO NOT WORK YET") {
		t.Fatalf("stack warnings must reach the summary:\n%s", out)
	}
}

func TestBackupIsRefusedWithoutAllowBrokenBackupAndKitCarriesTheCaveat(t *testing.T) {
	dir, r := setup(t)
	res := stackResult()
	k, _ := backup.NewKit(backup.KitInput{CompanyName: "A", Domain: "a.example", Endpoint: "https://s3.example", Bucket: "b", Region: "auto", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "s3cr3t-value-0123456789", ProviderType: "ssh"})
	k.Note = launch.KitCaveat
	res.Kit = k
	useStack(t, false, res, nil)
	creds := filepath.Join(t.TempDir(), "creds")
	_ = os.WriteFile(creds, []byte("AWS_ACCESS_KEY_ID=AKIAEXAMPLE\nAWS_SECRET_ACCESS_KEY=s3cr3t-value-0123456789\n"), 0o600)
	full := []string{"--backup-endpoint", "https://s3.example", "--backup-bucket", "b", "--backup-credentials-file", creds, "--print-secrets"}
	code, _, errs := run(t, stackArgs(dir, full...)...)
	if code != ExitUsage || !strings.Contains(errs, "--allow-broken-backup") || len(r.ran) != 0 {
		t.Fatalf("code=%d err=%q ran=%d", code, errs, len(r.ran))
	}
	code, out, errs := run(t, stackArgs(dir, append(full, "--allow-broken-backup")...)...)
	if code != ExitOK {
		t.Fatal(errs)
	}
	if strings.Count(out, "no data yet") < 2 {
		t.Fatalf("the caveat must be in the kit header and next to the hand-over:\n%s", out)
	}
	if !strings.Contains(k.Text(), "CAVEAT: "+launch.KitCaveat) {
		t.Fatal("kit header lacks the caveat")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestSecretsNotMarkedShownWhenTheWriteFails(t *testing.T) {
	dir, _ := setup(t)
	useStack(t, false, stackResult(), nil)
	var errb strings.Builder
	code := Execute(stackArgs(dir, "--print-secrets"), strings.NewReader(""), failWriter{}, &errb)
	if code != ExitFailure {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	st, _ := provider.NewStore(dir).Load()
	if st.Launch != nil && st.Launch.AdminCredsShown {
		t.Fatal("credentials that never reached the operator must not be marked shown")
	}
}

type allAvailable struct{}

func (allAvailable) Run(context.Context, []byte, ...string) (string, error) { return "1", nil }

func TestDownRemovesTheBackupTimerOnTheSSHServer(t *testing.T) {
	dir, r := setup(t)
	r.mu["CAN_BACKUP_REMOVED"] = "" // placeholder to keep the map literal usage explicit
	delete(r.mu, "CAN_BACKUP_REMOVED")
	r.mu["disable --now can-backup.timer"] = "CAN_BACKUP_REMOVED\n"
	useStack(t, false, stackResult(), nil)
	if code, _, errs := run(t, stackArgs(dir)...); code != ExitOK {
		t.Fatal(errs)
	}
	_ = provider.NewStore(dir).Mutate(func(s *provider.State) error {
		s.Launch.BackupInstalled = true
		return nil
	})
	code, out, errs := run(t, "down", dir, "--yes", "--modules-dir", "../../modules")
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	removed := false
	for _, s := range r.ran {
		if strings.Contains(s, "disable --now can-backup.timer") && strings.Contains(s, "/etc/can/backup.d") {
			removed = true
		}
	}
	if !removed {
		t.Fatal("down must remove the backup timer and its credentials from the server")
	}
}
