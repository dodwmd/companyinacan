package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/provider"
)

type cliRunner struct {
	mu     map[string]string // substring of script -> output
	ran    []string
	copied map[string][]byte
}

func (r *cliRunner) Run(_ context.Context, script string) (string, error) {
	r.ran = append(r.ran, script)
	for k, v := range r.mu {
		if strings.Contains(script, k) {
			return v, nil
		}
	}
	return "", errors.New("unexpected script: " + strings.SplitN(script, "\n", 2)[0])
}

func (r *cliRunner) Copy(_ context.Context, p string, d []byte, _ string) error {
	if r.copied == nil {
		r.copied = map[string][]byte{}
	}
	r.copied[p] = d
	return nil
}

type fixedResolver map[string][]string

func (f fixedResolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if v, ok := f[h]; ok {
		return v, nil
	}
	return nil, errors.New("no such host")
}

const probeOK = "uid=0\nbootstrapped=\nos_id=ubuntu\nos_version=24.04\narch=x86_64\nmem_mb=1950\ndisk_free_mb=30000\nsystemd=yes\nport_80=free\nport_443=free\nport_6443=free\n"

const kubeRaw = "apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: default\ncontexts: []\ncurrent-context: default\nusers: []\n"

func newRunner() *cliRunner {
	return &cliRunner{mu: map[string]string{
		"id -u":            probeOK,
		"can-bootstrap.sh": "CAN_BOOTSTRAP_DONE\n",
		"k3s.yaml":         kubeRaw,
		"k3s_active":       "bootstrapped=" + provider.DefaultK3sVersion + "\nk3s_active=active\nnodes_ready=1\n",
		"k3s-uninstall.sh": "CAN_UNINSTALL_DONE\n",
	}}
}

// useLifecycle swaps the package seams for one test.
func useLifecycle(t *testing.T, d func() provider.Deps, res provider.Resolver) {
	t.Helper()
	old := lifecycle
	lifecycle.deps = d
	if res != nil {
		lifecycle.resolver = res
	}
	lifecycle.now = func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { lifecycle = old })
}

func sshCompany(t *testing.T) string {
	return writeCompany(t, "schema: 1\nname: Acme Co\ndomain: acme.example\nprovider:\n  type: ssh\n  options:\n    host: 203.0.113.7\n")
}

func goodResolver() provider.Resolver {
	return fixedResolver{"203-0-113-7.sslip.io": {"203.0.113.7"}}
}

func TestUpPlanChangesNothing(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	code, out, errs := run(t, "up", dir, "--plan", "--modules-dir", "../../modules")
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	for _, want := range []string{"nothing will be changed", "[todo]", "OS, RAM", "k3s", "kubeconfig", "sslip.io", "DNS records"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q:\n%s", want, out)
		}
	}
	if len(r.ran) != 0 || len(r.copied) != 0 {
		t.Fatal("--plan touched the host")
	}
	if _, err := os.Stat(filepath.Join(dir, ".can")); !os.IsNotExist(err) {
		t.Fatal("--plan wrote .can/")
	}
}

func TestUpPlanJSON(t *testing.T) {
	dir := sshCompany(t)
	useLifecycle(t, func() provider.Deps { return provider.Deps{} }, goodResolver())
	code, out, errs := run(t, "up", dir, "--plan", "--output", "json", "--modules-dir", "../../modules")
	var res upResult
	if code != ExitOK || json.Unmarshal([]byte(out), &res) != nil || !res.Planned || len(res.Actions) != 5 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestUpEndToEndWithFakeHost(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	code, out, errs := run(t, "up", dir, "--modules-dir", "../../modules")
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	for _, want := range []string{
		"Server is up.", "203.0.113.7",
		"https://203-0-113-7.sslip.io", "https://admin.203-0-113-7.sslip.io",
		"TYPE", "acme.example", "admin.acme.example", "help.acme.example",
		"epic E03",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	kc, err := os.ReadFile(filepath.Join(dir, ".can", "kubeconfig"))
	if err != nil || !strings.Contains(string(kc), "127.0.0.1:16443") || !strings.Contains(string(kc), "acme-co") {
		t.Fatalf("kubeconfig: %v\n%s", err, kc)
	}
	if _, err := os.Stat(filepath.Join(dir, ".can", "state.json")); err != nil {
		t.Fatal("state.json missing")
	}

	// second run: idempotent, no second bootstrap upload
	r.mu["id -u"] = strings.Replace(probeOK, "bootstrapped=", "bootstrapped="+provider.DefaultK3sVersion, 1)
	r.copied = nil
	if code, out, errs := run(t, "up", dir, "--modules-dir", "../../modules"); code != ExitOK || len(r.copied) != 0 {
		t.Fatalf("second up: code=%d copied=%d out=%q err=%q", code, len(r.copied), out, errs)
	}
}

func TestUpFallsBackToHTTPWhenSSLIPUnavailable(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, fixedResolver{})
	code, out, _ := run(t, "up", dir, "--modules-dir", "../../modules")
	if code != ExitOK || !strings.Contains(out, "http://203.0.113.7") || !strings.Contains(out, "plain HTTP fallback") || !strings.Contains(out, "SSH tunnel") {
		t.Fatalf("code=%d\n%s", code, out)
	}
	if strings.Contains(out, "https://admin.") {
		t.Fatal("admin must not be advertised over the fallback")
	}
}

func TestUpPreflightFailureExitsOne(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	r.mu["id -u"] = strings.Replace(probeOK, "mem_mb=1950", "mem_mb=400", 1)
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	code, _, errs := run(t, "up", dir, "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(errs, "ram") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestUpExitCodes(t *testing.T) {
	useLifecycle(t, func() provider.Deps { return provider.Deps{} }, goodResolver())
	// not implemented provider types: 3
	for typ, epic := range map[string]string{"local": "E02", "static": "E18"} {
		dir := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: "+typ+"}\n")
		code, out, errs := run(t, "up", dir, "--modules-dir", "../../modules")
		if code != ExitNotImplemented || out != "" || !strings.Contains(errs, epic) {
			t.Errorf("%s: code=%d out=%q err=%q", typ, code, out, errs)
		}
	}
	// invalid company.yaml: 1
	bad := writeCompany(t, "schema: 2\n")
	if code, _, _ := run(t, "up", bad, "--modules-dir", "../../modules"); code != ExitFailure {
		t.Errorf("invalid company: code=%d", code)
	}
	// missing company.yaml: 1
	if code, _, _ := run(t, "up", t.TempDir(), "--modules-dir", "../../modules"); code != ExitFailure {
		t.Errorf("missing company: code=%d", code)
	}
	// usage errors: 2
	ok := sshCompany(t)
	if code, _, _ := run(t, "up", ok, "--bogus"); code != ExitUsage {
		t.Errorf("unknown flag: code=%d", code)
	}
	if code, _, _ := run(t, "up", ok, "--dns", "route66", "--modules-dir", "../../modules"); code != ExitFailure {
		t.Errorf("unknown dns plugin: code=%d", code)
	}
	// cloudflare without a token fails before any server work (exit 1, not 3)
	if code, _, errs := run(t, "up", ok, "--dns", "cloudflare", "--modules-dir", "../../modules"); code != ExitFailure || !strings.Contains(errs, "CLOUDFLARE_API_TOKEN") {
		t.Errorf("cloudflare: code=%d err=%q", code, errs)
	}
	if code, _, errs := run(t, "up", ok, "--dns", "provider-native", "--modules-dir", "../../modules"); code != ExitNotImplemented || !strings.Contains(errs, "E12") {
		t.Errorf("provider-native: code=%d err=%q", code, errs)
	}
	if code, _, _ := run(t, "up", "a", "b"); code != ExitUsage {
		t.Errorf("too many args: code=%d", code)
	}
}

func TestDownRequiresYes(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	code, out, errs := run(t, "down", dir, "--modules-dir", "../../modules")
	if code != ExitUsage || !strings.Contains(errs, "--yes") || !strings.Contains(out, "uninstalls k3s") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if len(r.ran) != 0 {
		t.Fatal("down ran without --yes")
	}
	code, out, _ = run(t, "down", dir, "--plan", "--modules-dir", "../../modules")
	if code != ExitOK || !strings.Contains(out, "left as they are") || len(r.ran) != 0 {
		t.Fatalf("plan: code=%d out=%q", code, out)
	}
}

func TestUpStatusDownLifecycle(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	args := func(a ...string) []string { return append(a, dir, "--modules-dir", "../../modules") }

	if code, out, _ := run(t, args("status")...); code != ExitFailure || !strings.Contains(out, "absent") {
		t.Fatalf("status before up: code=%d out=%q", code, out)
	}
	if code, out, errs := run(t, args("up")...); code != ExitOK {
		t.Fatalf("up: %d %q %q", code, out, errs)
	}
	code, out, _ := run(t, args("status")...)
	if code != ExitOK || !strings.Contains(out, "ready") || !strings.Contains(out, "203.0.113.7") || !strings.Contains(out, "[ok  ] node ready") {
		t.Fatalf("status: code=%d out=%q", code, out)
	}
	code, out, _ = run(t, args("status", "--output", "json")...)
	var sr statusResult
	if code != ExitOK || json.Unmarshal([]byte(out), &sr) != nil || sr.Phase != provider.PhaseReady || sr.Address != "203.0.113.7" {
		t.Fatalf("status json: code=%d out=%q", code, out)
	}
	code, out, errs := run(t, args("down", "--yes")...)
	if code != ExitOK {
		t.Fatalf("down: %d %q %q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, ".can", "state.json")); !os.IsNotExist(err) {
		t.Error("state not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, ".can", "kubeconfig")); !os.IsNotExist(err) {
		t.Error("kubeconfig not removed")
	}
	if code, _, _ := run(t, args("down", "--yes")...); code != ExitOK {
		t.Errorf("second down: %d", code)
	}
}

func TestStatusDegradedExitsOne(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	_, _, _ = run(t, "up", dir, "--modules-dir", "../../modules")
	r.mu["k3s_active"] = "bootstrapped=" + provider.DefaultK3sVersion + "\nk3s_active=failed\nnodes_ready=0\n"
	code, out, _ := run(t, "status", dir, "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(out, "degraded") || !strings.Contains(out, "FAIL") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// --- secret redaction, end to end through the CLI ---------------------------

const cliToken = "hcloud_live_ABCDEFGHIJKLMNOP0123"

func hetznerCompany(t *testing.T) string {
	return writeCompany(t, "schema: 1\nname: Acme Co\ndomain: acme.example\nprovider:\n  type: hetzner\n  options:\n    ssh_public_key_file: /keys/id.pub\n")
}

func TestHetznerTokenNeverReachesOutput(t *testing.T) {
	// The API echoes the token back in its error message, the worst case.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"token ` + cliToken + ` has no write access"}}`))
	}))
	defer api.Close()
	dir := hetznerCompany(t)
	useLifecycle(t, func() provider.Deps {
		return provider.Deps{
			APIBase: api.URL,
			Getenv:  func(k string) string { return map[string]string{"HCLOUD_TOKEN": cliToken}[k] },
			ReadFile: func(string) ([]byte, error) {
				return []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ2o8e4mJw4mxk8t9S8YxqY0k3q8bX9r1Zk5c2n7t4Qd x\n"), nil
			},
			Sleep: func(context.Context, time.Duration) {},
		}
	}, goodResolver())
	for _, mode := range [][]string{{}, {"--output", "json"}} {
		args := append([]string{"up", dir, "--modules-dir", "../../modules"}, mode...)
		code, out, errs := run(t, args...)
		if code != ExitFailure {
			t.Fatalf("code=%d", code)
		}
		if strings.Contains(out+errs, cliToken) {
			t.Fatalf("token leaked (%v):\nstdout=%q\nstderr=%q", mode, out, errs)
		}
		if !strings.Contains(errs, provider.Redacted) {
			t.Errorf("expected redaction marker in %q", errs)
		}
	}
	code, out, errs := run(t, "status", dir, "--modules-dir", "../../modules")
	if code != ExitFailure || strings.Contains(out+errs, cliToken) {
		t.Fatalf("status leaked or wrong code %d: %q %q", code, out, errs)
	}
}

func TestHetznerMissingTokenIsAFailureNotALeak(t *testing.T) {
	dir := hetznerCompany(t)
	useLifecycle(t, func() provider.Deps {
		return provider.Deps{Getenv: func(string) string { return "" }, ReadFile: func(string) ([]byte, error) { return []byte("ssh-ed25519 AAAA x"), nil }}
	}, goodResolver())
	code, _, errs := run(t, "up", dir, "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(errs, "HCLOUD_TOKEN") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestRemoteOutputSecretsAreRedactedInCLI(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	r.mu["can-bootstrap.sh"] = "k3s token: K10abcdefghijklmnop::server:secretsecret\npassword=hunter2hunter2\n"
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	code, out, errs := run(t, "up", dir, "--modules-dir", "../../modules")
	if code != ExitOK {
		t.Fatalf("code=%d %q", code, errs)
	}
	if strings.Contains(out, "hunter2hunter2") || strings.Contains(out, "secretsecret") {
		t.Fatalf("secret in output:\n%s", out)
	}
}

func TestKubeName(t *testing.T) {
	for in, want := range map[string]string{"Acme Co": "acme-co", "  ": "company", "A_B--C": "a-b-c", "Zoë's Shop!": "zo-s-shop"} {
		if got := kubeName(in); got != want {
			t.Errorf("kubeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := kubeName(strings.Repeat("a", 100)); len(got) != 60 {
		t.Errorf("long name: %d", len(got))
	}
}

func TestJSONOutputFlagWorksForAllLifecycleCommands(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	mod := []string{dir, "--modules-dir", "../../modules", "--output", "json"}
	for _, c := range [][]string{{"up"}, {"status"}, {"down", "--plan"}, {"down", "--yes"}} {
		code, out, errs := run(t, append(c[:1:1], append(c[1:], mod...)...)...)
		var v map[string]any
		if code != ExitOK || json.Unmarshal([]byte(out), &v) != nil {
			t.Errorf("%v: code=%d out=%q err=%q", c, code, out, errs)
		}
	}
}

func TestConcurrentUpIsRefused(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	release, err := provider.NewStore(dir).LockOperation()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, c := range [][]string{{"up"}, {"down", "--yes"}} {
		code, _, errs := run(t, append(c, dir, "--modules-dir", "../../modules")...)
		if code != ExitFailure || !strings.Contains(errs, "already running") {
			t.Errorf("%v: code=%d err=%q", c, code, errs)
		}
	}
	if len(r.ran) != 0 {
		t.Fatal("ran despite the lock")
	}
}

func TestDownLocalDelegatesToDevDown(t *testing.T) {
	dir := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\n")
	code, out, errs := run(t, "down", dir, "--plan", "--modules-dir", "../../modules")
	if code != ExitOK || !strings.Contains(out, "k3d") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestDownRemovesKnownHosts(t *testing.T) {
	dir := sshCompany(t)
	r := newRunner()
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	_, _, _ = run(t, "up", dir, "--modules-dir", "../../modules")
	kh := filepath.Join(dir, ".can", "known_hosts")
	if err := os.WriteFile(kh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run(t, "down", dir, "--yes", "--modules-dir", "../../modules"); code != ExitOK {
		t.Fatal(code)
	}
	if _, err := os.Stat(kh); !os.IsNotExist(err) {
		t.Fatal("known_hosts left behind")
	}
}
