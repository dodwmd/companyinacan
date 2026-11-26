package launch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/provider"
)

// fakeKube records every kubectl call and answers from rules (first match wins on the joined args).
type fakeKube struct {
	mu    sync.Mutex
	calls []kubeCall
	rules []kubeRule
}

type kubeCall struct {
	Args  []string
	Stdin string
}

type kubeRule struct {
	contains string
	out      string
	err      error
	times    int // 0 = always; n = the first n matching calls only
	used     int
}

func (f *fakeKube) Run(_ context.Context, stdin []byte, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, kubeCall{Args: args, Stdin: string(stdin)})
	line := strings.Join(args, " ")
	defaults := []kubeRule{{contains: "get ingressroutes.traefik.io forms-public", out: formsRoute}}
	for i := range f.rules {
		r := &f.rules[i]
		if !strings.Contains(line, r.contains) || (r.times > 0 && r.used >= r.times) {
			continue
		}
		r.used++
		return r.out, r.err
	}
	for _, r := range defaults {
		if strings.Contains(line, r.contains) {
			return r.out, r.err
		}
	}
	return "", nil
}

func (f *fakeKube) called(sub string) []kubeCall {
	var out []kubeCall
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c.Args, " "), sub) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeKube) index(sub string) int {
	for i, c := range f.calls {
		if strings.Contains(strings.Join(c.Args, " "), sub) {
			return i
		}
	}
	return -1
}

type fakeLoader struct {
	sets []ImageSet
	err  error
}

func (l *fakeLoader) Load(_ context.Context, s ImageSet, _ func(string)) error {
	l.sets = append(l.sets, s)
	return l.err
}

// counterRand is a deterministic, non-secret entropy source for tests.
type counterRand struct{ n byte }

func (c *counterRand) Read(p []byte) (int, error) {
	for i := range p {
		c.n++
		p[i] = c.n
	}
	return len(p), nil
}

const bootstrapOut = "Admin \"admin\" created.\nTOTP secret (shown once): JBSWY3DPEHPK3PXP\nProvisioning URI: otpauth://totp/x\n"
const resetOut = "Password for \"admin\" reset; all sessions ended.\nNew TOTP secret (shown once): KRSXG5CTMVRXEZLU\nProvisioning URI: otpauth://totp/x\n"

func testCompany(t *testing.T) (dir string, co *config.Company, yamlSrc []byte) {
	t.Helper()
	dir = t.TempDir()
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "examples", "acme-co", "company.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	src = []byte(strings.Replace(string(src), "type: local", "type: ssh\n  options: {host: 203.0.113.7}", 1))
	src = []byte(strings.Replace(string(src), "modules:\n  example: {}\n", "", 1))
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, ps := config.Load(filepath.Join(dir, "company.yaml"))
	if doc == nil {
		t.Fatal(ps)
	}
	co, err = config.DecodeCompany(doc)
	if err != nil {
		t.Fatal(err)
	}
	return dir, co, src
}

// formsRoute is what can-core's /_forms route looks like for the test's routes.
const formsRoute = `{"metadata":{"labels":{"can.dev/route":"true"}},"spec":{"entryPoints":["web"],"routes":[{"match":"Host(` + "`acme.example`" + `) && PathPrefix(` + "`/_forms`" + `)"}]}}`

func newStack(t *testing.T, k *fakeKube, l *fakeLoader) (Options, Deps, *bytes.Buffer) {
	t.Helper()
	dir, co, src := testCompany(t)
	set, err := LoadDir(imagesDir(t, false))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	red := provider.NewRedactor()
	o := Options{
		Dir: dir, Company: co, CompanyYAML: src, Images: set, DefaultBundle: true,
		Routes:   RouteOptions{SiteHosts: []string{"acme.example"}, AdminHosts: []string{"admin.acme.example"}},
		Timeouts: Timeouts{Rollout: time.Minute, CRD: time.Minute, Module: time.Minute},
	}
	d := Deps{
		Kube: k, Loader: l, Store: provider.NewStore(dir), Redactor: red, Rand: &counterRand{},
		Out: red.Writer(&out), Sleep: func(context.Context, time.Duration) {}, Now: time.Now,
	}
	return o, d, &out
}

func TestRunExecutesPhasesInOrderAndCheckpoints(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	l := &fakeLoader{}
	o, d, _ := newStack(t, k, l)
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range res.Phases {
		got = append(got, p.Name)
	}
	want := "base,images,company,routing,core,admin,site,routes,ready"
	if strings.Join(got, ",") != want {
		t.Fatalf("phases %v, want %s", got, want)
	}
	for _, a := range [][2]string{
		{"get crd/helmchartconfigs", "apply -f -"}, {"apply -f -", "rollout status deploy/can-core"},
		{"server-side", "rollout status deploy/can-core"}, {"rollout status deploy/can-core", "admin bootstrap"}, {"admin bootstrap", "rollout status deploy/site"},
	} {
		if i, j := k.index(a[0]), k.index(a[1]); i < 0 || j < 0 || i > j {
			t.Errorf("%q must come before %q (%d, %d)", a[0], a[1], i, j)
		}
	}
	st, _ := d.Store.Load()
	for _, p := range []string{"base", "images", "core", "admin", "company", "site", "routes", "ready"} {
		if _, ok := st.Launch.Phases[p]; !ok {
			t.Errorf("no checkpoint for %s: %v", p, st.Launch.Phases)
		}
	}
	if !st.Launch.AdminCreated || st.Launch.SiteSHA256 == "" || st.Launch.ImagesFingerprint == "" {
		t.Fatalf("%+v", st.Launch)
	}
	if res.Admin == nil || res.Admin.Username != "admin" || res.Admin.TOTPSecret != "JBSWY3DPEHPK3PXP" || len(res.Admin.Password) < 20 {
		t.Fatalf("admin credentials: %+v", res.Admin)
	}
	if len(l.sets) != 1 || l.sets[0].Source != SourceDir {
		t.Fatalf("the images must be loaded once: %v", l.sets)
	}
}

func TestNoSecretInArgvStateOrProgress(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, out := newStack(t, k, &fakeLoader{})
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	pw := res.Admin.Password
	for _, c := range k.calls {
		if strings.Contains(strings.Join(c.Args, " "), pw) || strings.Contains(strings.Join(c.Args, " "), res.Admin.TOTPSecret) {
			t.Fatalf("a secret reached kubectl argv: %v", c.Args)
		}
	}
	boot := k.called("admin bootstrap")
	if len(boot) != 1 || strings.TrimSpace(boot[0].Stdin) != pw {
		t.Fatalf("the password must be sent on stdin only: %+v", boot)
	}
	raw, _ := os.ReadFile(provider.StatePath(o.Dir))
	if strings.Contains(string(raw), pw) || strings.Contains(string(raw), res.Admin.TOTPSecret) {
		t.Fatalf("state.json holds a secret:\n%s", raw)
	}
	if strings.Contains(out.String(), pw) || strings.Contains(out.String(), res.Admin.TOTPSecret) {
		t.Fatalf("progress output holds a secret:\n%s", out.String())
	}
}

func TestAdminIsNotRecreatedOnceCredentialsWereShown(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if err := MarkAdminShown(d.Store); err != nil {
		t.Fatal(err)
	}
	k.calls = nil
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Admin != nil || len(k.called("admin bootstrap")) != 0 || len(k.called("reset-password")) != 0 {
		t.Fatalf("a re-run must never touch an admin whose credentials were handed over: %+v %v", res.Admin, k.calls)
	}
	if len(k.called("apply")) == 0 {
		t.Fatal("the idempotent apply phases still run, so a changed company.yaml converges")
	}
	if len(k.called("exec")) != 0 {
		t.Fatalf("unexpected exec: %v", k.called("exec"))
	}
}

func TestResumeAfterCrashBeforeCredentialsShownRotatesThem(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{
		{contains: "admin bootstrap", out: "Error: admin \"admin\" already exists\n", err: errors.New("exit status 1: admin \"admin\" already exists")},
		{contains: "reset-password", out: resetOut},
	}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Admin == nil || res.Admin.TOTPSecret != "KRSXG5CTMVRXEZLU" {
		t.Fatalf("a crash before the credentials were shown must issue fresh ones: %+v", res.Admin)
	}
	rs := k.called("reset-password")
	if len(rs) != 1 || !strings.Contains(strings.Join(rs[0].Args, " "), "--rotate-totp") || strings.TrimSpace(rs[0].Stdin) != res.Admin.Password {
		t.Fatalf("%+v", rs)
	}
}

func TestExistingAdminAfterCredsShownIsLeftAlone(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", err: errors.New("admin \"admin\" already exists")}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	_ = d.Store.Mutate(func(s *provider.State) error {
		s.Launch = &provider.LaunchState{AdminUser: "admin", AdminCreated: true, AdminCredsShown: true}
		return nil
	})
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Admin != nil || len(k.called("reset-password")) != 0 {
		t.Fatal("never reset an admin whose password the operator already holds")
	}
}

func TestImagesSkippedWhenAlreadyLoaded(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	l := &fakeLoader{}
	o, d, _ := newStack(t, k, l)
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if len(l.sets) != 1 {
		t.Fatalf("the second run must not re-upload the tarballs, loads: %d", len(l.sets))
	}
}

func TestPhaseFailureNamesPhaseAndRemedyAndStopsThere(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{
		{contains: "rollout status deploy/can-core", err: errors.New("timed out waiting for the condition")},
	}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	_, err := Run(context.Background(), o, d)
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != "core" {
		t.Fatalf("want a PhaseError for core, got %v", err)
	}
	if !strings.Contains(err.Error(), "kubectl -n can-system logs deploy/can-core") || !strings.Contains(err.Error(), "can up") {
		t.Fatalf("the error must say how to look and how to resume: %v", err)
	}
	if len(k.called("admin bootstrap")) != 0 {
		t.Fatal("no later phase may run after a failure")
	}
	st, _ := d.Store.Load()
	if _, ok := st.Launch.Phases["core"]; ok {
		t.Fatal("a failed phase must not be checkpointed")
	}
	if _, ok := st.Launch.Phases["base"]; !ok {
		t.Fatal("finished phases stay checkpointed")
	}
}

func TestImageLoadFailureIsFatalAndRemediated(t *testing.T) {
	o, d, _ := newStack(t, &fakeKube{}, &fakeLoader{err: errors.New("sha256 mismatch on the server")})
	_, err := Run(context.Background(), o, d)
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != "images" || !strings.Contains(err.Error(), "make images") {
		t.Fatalf("%v", err)
	}
}

func TestFormsRoutedAfterModuleAppears(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{
		{contains: "admin bootstrap", out: bootstrapOut},
		{contains: "get svc forms", err: errors.New("NotFound"), times: 2},
	}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if len(k.called("get svc forms")) != 3 {
		t.Fatalf("must poll until the service exists: %d", len(k.called("get svc forms")))
	}
	if len(k.called("get ingressroutes.traefik.io forms-public")) == 0 {
		t.Fatal("did not wait for can-core to route /_forms")
	}
}

// A re-run with an unchanged company.yaml and other routes (--domain-live: TLS and the domain) must
// rewrite the facts, and must not accept the route left from the run before.
func TestRerunWithChangedRoutesRewritesTheFacts(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	first, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	k.rules = append(k.rules, kubeRule{contains: "get configmap company", out: string(first.CompanyYAML)})
	k.calls = nil
	o.Routes = RouteOptions{SiteHosts: []string{"acme.example"}, AdminHosts: []string{"admin.acme.example"}, TLS: true}
	_, err = Run(context.Background(), o, d)
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != "routes" || !strings.Contains(err.Error(), "want websecure") {
		t.Fatalf("the stale route was accepted: %v", err)
	}
	var written bool
	for _, c := range k.calls {
		if strings.Contains(c.Stdin, "company.yaml:") {
			t.Fatal("test setup: company.yaml should be unchanged")
		}
		written = written || strings.Contains(c.Stdin, "name: can-routing") && strings.Contains(c.Stdin, `tls: "true"`)
	}
	if !written {
		t.Fatal("the routing facts were not rewritten")
	}
}

func TestFormsTimeoutIsAFailureWithRemedy(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{
		{contains: "admin bootstrap", out: bootstrapOut},
		{contains: "get svc forms", err: errors.New("NotFound")},
	}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	n := 0
	d.Sleep = func(context.Context, time.Duration) { n++ }
	o.Timeouts.Module = 30 * time.Second
	_, err := Run(context.Background(), o, d)
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != "routes" || !strings.Contains(err.Error(), "logs deploy/can-core") {
		t.Fatalf("%v", err)
	}
	if n == 0 || n > 20 {
		t.Fatalf("the wait is bounded and polled: %d sleeps", n)
	}
}

func TestDefaultBundleIsPushedNotWrittenToDisk(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if !res.BundleAdded {
		t.Fatal("the example company has no forms: the default bundle adds it")
	}
	var pushed string
	for _, c := range k.calls {
		if strings.Contains(c.Stdin, "kind: ConfigMap") && strings.Contains(c.Stdin, "company.yaml: ") {
			pushed = c.Stdin
		}
	}
	if !strings.Contains(pushed, "forms") {
		t.Fatalf("forms not in the pushed company:\n%s", pushed)
	}
	disk, _ := os.ReadFile(filepath.Join(o.Dir, "company.yaml"))
	if strings.Contains(string(disk), "forms") {
		t.Fatal("company.yaml on disk must not be modified")
	}
}

// --- backup -------------------------------------------------------------------------------

type hostFake struct {
	mu       sync.Mutex
	ran      []string
	copied   map[string][]byte
	out      map[string]string
	streamed []string
}

func (h *hostFake) Run(_ context.Context, script string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ran = append(h.ran, script)
	for k, v := range h.out {
		if strings.Contains(script, k) {
			return v, nil
		}
	}
	if strings.HasPrefix(script, "if [ -e ") {
		// what an earlier run copied there
		body := string(h.copied[strings.Fields(script)[3]])
		if !strings.Contains(script, "grep ") {
			return body, nil
		}
		for _, l := range strings.Split(body, "\n") {
			if strings.HasPrefix(l, "export RESTIC_REPOSITORY=") {
				return l, nil
			}
		}
		return "", nil
	}
	return "CAN_BACKUP_OK\nCAN_IMAGE_OK\nCAN_CREDS_OK\nCAN_TIMER_OK\n", nil
}

func (h *hostFake) Copy(_ context.Context, p string, b []byte, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.copied == nil {
		h.copied = map[string][]byte{}
	}
	h.copied[p] = b
	return nil
}

func backupInstall(t *testing.T, o Options) *BackupInstall {
	t.Helper()
	return &BackupInstall{
		Input:    backup.KitInput{CompanyName: o.Company.Name, Domain: o.Company.Domain, Endpoint: "https://s3.example", Bucket: "bucket", Region: "auto", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "s3cr3t-value-0123456789", ProviderType: "ssh"},
		Mods:     map[string]*config.Module{},
		Settings: backup.DefaultSettings(),
	}
}

func TestBackupInstalledOnHostAndKitReturned(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, out := newStack(t, k, &fakeLoader{})
	h := &hostFake{}
	d.Host = h
	o.Backup = backupInstall(t, o)
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kit == nil || res.Kit.Restic.Password == "" {
		t.Fatalf("no kit: %+v", res.Kit)
	}
	for _, p := range []string{"/usr/local/sbin/can-backup", "/etc/systemd/system/can-backup.timer", "/etc/systemd/system/can-backup.service", "/etc/can/backup.d/restic.password", "/etc/can/backup.d/restic.env"} {
		if _, ok := h.copied[p]; !ok {
			t.Errorf("not installed on the host: %s (have %v)", p, keys(h.copied))
		}
	}
	if string(h.copied["/etc/can/backup.d/restic.password"]) != res.Kit.Restic.Password {
		t.Fatal("the password on the host must be the one in the kit")
	}
	st, _ := d.Store.Load()
	if !st.Launch.BackupInstalled {
		t.Fatal("not recorded")
	}
	raw, _ := os.ReadFile(provider.StatePath(o.Dir))
	if strings.Contains(string(raw), res.Kit.Restic.Password) || strings.Contains(out.String(), res.Kit.Restic.Password) || strings.Contains(out.String(), "s3cr3t-value-0123456789") {
		t.Fatal("the kit's secrets leaked into state or progress output")
	}
	joined := strings.Join(h.ran, "\n")
	if strings.Contains(joined, res.Kit.Restic.Password) || strings.Contains(joined, "s3cr3t-value-0123456789") {
		t.Fatal("a secret appears in a script run over ssh (it would be visible to ps on the host)")
	}
	if !strings.Contains(joined, "enable --now can-backup.timer") {
		t.Fatalf("the timer is not enabled:\n%s", joined)
	}
	for _, want := range []string{"install -d -o 65532 -g 65532 -m 0700 '/var/lib/can/dumps'", "sha256sum -c", backup.ResticVersion} {
		if !strings.Contains(joined, want) {
			t.Errorf("the host is not prepared: %q missing from the scripts run", want)
		}
	}
	if res.Kit.Note != "" {
		t.Errorf("no caveat on a working backup: %q", res.Kit.Note)
	}
}

func (h *hostFake) CopyFrom(ctx context.Context, p string, r io.Reader, mode string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.streamed = append(h.streamed, p)
	h.mu.Unlock()
	return h.Copy(ctx, p, b, mode)
}

func TestImagesAreStreamedWhenTheRunnerCanAndReverified(t *testing.T) {
	set, err := LoadDir(imagesDir(t, false))
	if err != nil {
		t.Fatal(err)
	}
	h := &hostFake{}
	if err := (SSHImageLoader{Host: h}).Load(context.Background(), set, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if len(h.streamed) != 3 {
		t.Fatalf("streamed %v", h.streamed)
	}
	// a tarball changed after LoadDir is refused before any byte is sent
	if err := os.WriteFile(set.Images[ImageCore].path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	h2 := &hostFake{}
	if err := (SSHImageLoader{Host: h2}).Load(context.Background(), set, func(string) {}); err == nil || len(h2.streamed) != 0 {
		t.Fatalf("a tampered tarball must be refused before upload: %v", err)
	}
}

func TestBackupReusesPasswordAlreadyOnHost(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	h := &hostFake{out: map[string]string{"cat /etc/can/backup.d/restic.password": "left-by-crashed-run-0123456789\n"}}
	d.Host = h
	o.Backup = backupInstall(t, o)
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kit.Restic.Password != "left-by-crashed-run-0123456789" || string(h.copied["/etc/can/backup.d/restic.password"]) != res.Kit.Restic.Password {
		t.Fatalf("a crash after restic init must not mint a second password: %q", res.Kit.Restic.Password)
	}
	if res.Kit.Note != "" || strings.Contains(strings.Join(res.Warnings, "\n"), "DO NOT WORK") {
		t.Fatalf("backups work: no caveat expected: %q %v", res.Kit.Note, res.Warnings)
	}
}

func TestCompanyChangedInClusterIsNotOverwrittenWithoutForce(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	// someone edited the company in the admin UI
	k.rules = append([]kubeRule{{contains: "get configmap company", out: "name: edited in the UI\n"}}, k.rules...)
	_, err := Run(context.Background(), o, d)
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != "company" || !strings.Contains(err.Error(), "--force-company") {
		t.Fatalf("want a refusal naming --force-company, got %v", err)
	}
	o.ForceCompany = true
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatalf("forced: %v", err)
	}
}

func TestCompanyReadFailureFailsClosed(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "get configmap company", err: errors.New("Unable to connect to the server: tunnel down")}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	_, err := Run(context.Background(), o, d)
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != "company" {
		t.Fatalf("want a company PhaseError, got %v", err)
	}
	for _, c := range k.calls {
		if strings.Contains(c.Stdin, "company.yaml: ") {
			t.Fatal("must not apply a document it could not compare with the cluster's")
		}
	}
	if !strings.Contains(strings.Join(k.calls[len(k.calls)-1].Args, " "), "--ignore-not-found") && len(k.called("--ignore-not-found")) == 0 {
		t.Fatal("NotFound must be handled by --ignore-not-found, not by swallowing errors")
	}
}

func TestCompanyUnchangedInClusterIsNotReapplied(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	k.calls = nil
	k.rules = append([]kubeRule{{contains: "get configmap company", out: string(res.CompanyYAML)}}, k.rules...)
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	for _, c := range k.calls {
		if strings.Contains(c.Stdin, "kind: ConfigMap") && strings.Contains(c.Stdin, "company.yaml: ") {
			t.Fatal("an identical company must not be applied again")
		}
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBackupResumeRebuildsKitFromHostWhenNotShown(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	h := &hostFake{out: map[string]string{"cat /etc/can/backup.d/restic.password": "hostpw-0123456789abcdef\n"}}
	d.Host = h
	o.Backup = backupInstall(t, o)
	_ = d.Store.Mutate(func(s *provider.State) error {
		s.Launch = &provider.LaunchState{BackupInstalled: true, BackupVersion: BackupInstallVersion}
		return nil
	})
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kit == nil || res.Kit.Restic.Password != "hostpw-0123456789abcdef" {
		t.Fatalf("an interrupted run must hand over the same kit the host already uses: %+v", res.Kit)
	}
	for p := range h.copied {
		if p != "/etc/can/backup.d/restic.env" && p != "/etc/can/backup.d/restic.env.new" { // the credentials file follows the flags, tried first
			t.Fatalf("nothing is reinstalled: %s", p)
		}
	}
	if err := MarkKitShown(d.Store); err != nil {
		t.Fatal(err)
	}
	res, err = Run(context.Background(), o, d)
	if err != nil || res.Kit != nil {
		t.Fatalf("a kit that was shown is never shown again: %v %+v", err, res.Kit)
	}
}

func TestBackupWithoutHostRunnerFailsClosed(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	o.Backup = backupInstall(t, o)
	_, err := Run(context.Background(), o, d)
	var pe *PhaseError
	if !errors.As(err, &pe) || pe.Phase != "backup" {
		t.Fatalf("%v", err)
	}
}

func TestSSHImageLoaderUploadsVerifiesAndImports(t *testing.T) {
	set, err := LoadDir(imagesDir(t, false))
	if err != nil {
		t.Fatal(err)
	}
	h := &hostFake{out: map[string]string{}}
	var msgs []string
	if err := (SSHImageLoader{Host: h}).Load(context.Background(), set, func(s string) { msgs = append(msgs, s) }); err != nil {
		t.Fatal(err)
	}
	if len(h.copied) != 3 || len(h.ran) != 4 {
		t.Fatalf("copied %v ran %d", keys(h.copied), len(h.ran))
	}
	if _, ok := h.copied["/var/lib/rancher/k3s/agent/images/can-core.tar"]; !ok {
		t.Fatalf("%v", keys(h.copied))
	}
	if !strings.Contains(h.ran[1], set.Images[ImageCore].SHA256) || !strings.Contains(h.ran[1], "k3s ctr -n k8s.io images import") {
		t.Fatalf("the script must verify the checksum on the server and import: %s", h.ran[1])
	}
	h2 := &hostFake{out: map[string]string{"sha256sum": "CAN_SHA_MISMATCH deadbeef\n"}}
	err = SSHImageLoader{Host: h2}.Load(context.Background(), set, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("a corrupted upload must fail: %v", err)
	}
	if err := (SSHImageLoader{Host: h}).Load(context.Background(), ImageSet{Source: SourceRelease}, func(string) {}); err != nil {
		t.Fatalf("release images are pulled by the server, nothing to upload: %v", err)
	}
}

func TestStatusReportsFromStateAndCluster(t *testing.T) {
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	k2 := &fakeKube{rules: []kubeRule{
		{contains: "get deploy can-core", out: "1"},
		{contains: "get deploy site", out: "0"},
		{contains: "get deploy forms", out: "1"},
	}}
	checks, ok := Status(context.Background(), d.Store, k2)
	if ok {
		t.Fatalf("a site with no available replicas is not ready: %+v", checks)
	}
	var names []string
	for _, c := range checks {
		names = append(names, fmt.Sprintf("%s=%v", c.Name, c.OK))
	}
	s := strings.Join(names, " ")
	for _, want := range []string{"can-core=true", "site=false", "forms=true", "phases=true"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	empty := provider.NewStore(t.TempDir())
	if cs, ok := Status(context.Background(), empty, k2); ok || len(cs) != 1 || !strings.Contains(cs[0].Detail, "can up") {
		t.Fatalf("no checkpoints: say to run can up: %+v", cs)
	}
}
