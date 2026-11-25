//go:build integration

package integration

// Epic E24: the phases `can up` runs after a server exists, against a real k3d cluster.
//
// The provider half (create a server, install k3s, fetch the kubeconfig, SSH tunnel) is verified
// offline against fakes; this test stands in for it: the k3d cluster IS the "server", its
// kubeconfig is what a provider would have returned, and the image loader imports tarballs with
// `k3d image import` instead of shipping them over SSH. Everything after that is the production
// code, unchanged: core/launch.Run applies deploy/base, loads the images, pushes company.yaml,
// deploys can-core, creates the admin, publishes the site, routes it through Traefik, waits.
//
// It needs: a cluster from `make dev`, images from `scripts/build-images.sh build/images`
// (CAN_IT_IMAGES), kubectl, k3d. Skipped unless CAN_IT_MAKEUP=1. CAN_IT_TIMINGS names a file that
// receives a markdown table of the measured phase durations.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/launch"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/server/auth"
)

type k3dLoader struct {
	cluster string
	mu      sync.Mutex
	loads   int
}

func (l *k3dLoader) Load(ctx context.Context, set launch.ImageSet, progress func(string)) error {
	l.mu.Lock()
	l.loads++
	l.mu.Unlock()
	for _, img := range set.Upload() {
		progress("k3d image import " + img.Ref)
		out, err := exec.CommandContext(ctx, "k3d", "image", "import", filepath.Join(os.Getenv("CAN_IT_IMAGES"), img.File), "-c", l.cluster).CombinedOutput() //nolint:gosec // test helper
		if err != nil {
			return fmt.Errorf("k3d image import %s: %v\n%s", img.Name, err, out)
		}
	}
	return nil
}

type recHost struct {
	mu     sync.Mutex
	copied map[string][]byte
	ran    []string
}

func (h *recHost) Run(_ context.Context, script string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ran = append(h.ran, script)
	return "CAN_BACKUP_OK\n", nil
}

func (h *recHost) Copy(_ context.Context, p string, b []byte, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.copied == nil {
		h.copied = map[string][]byte{}
	}
	h.copied[p] = b
	return nil
}

// edgeClient reaches Traefik on the k3d load balancer (localhost:80) whatever host the URL names,
// which is how the same routes a real DNS name would use are exercised without DNS.
func edgeClient() *http.Client {
	return &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:80")
		}},
	}
}

func edge(t *testing.T, method, rawURL string, hdr map[string]string, body io.Reader) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := edgeClient().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestMakeUpPhasesOnK3d(t *testing.T) {
	if os.Getenv("CAN_IT_MAKEUP") != "1" {
		t.Skip("set CAN_IT_MAKEUP=1 (needs `make dev`, CAN_IT_IMAGES from scripts/build-images.sh, kubectl and k3d)")
	}
	root := repoRoot(t)
	imagesDir := os.Getenv("CAN_IT_IMAGES")
	if imagesDir == "" {
		t.Fatal("CAN_IT_IMAGES must name the directory written by scripts/build-images.sh")
	}

	// The company: the shipped example, on an ssh provider (never contacted here), no extra modules,
	// so the default bundle adds the forms module.
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join(root, "examples", "acme-co", "company.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(src), "type: local", "type: ssh\n  options: {host: 203.0.113.7}", 1)
	s = strings.Replace(s, "modules:\n  example: {}\n", "", 1)
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, ps := config.Load(filepath.Join(dir, "company.yaml"))
	if doc == nil {
		t.Fatal(ps)
	}
	co, err := config.DecodeCompany(doc)
	if err != nil {
		t.Fatal(err)
	}
	images, err := launch.LoadDir(imagesDir)
	if err != nil {
		t.Fatal(err)
	}
	formsDoc, ps := config.Load(filepath.Join(root, "modules", "forms", "module.yaml"))
	if formsDoc == nil {
		t.Fatal(ps)
	}
	formsMod, err := config.DecodeModule(formsDoc)
	if err != nil {
		t.Fatal(err)
	}

	const siteHost, adminHost = "acme.example", "admin.acme.example"
	store := provider.NewStore(dir)
	red := provider.NewRedactor()
	var progress bytes.Buffer
	loader := &k3dLoader{cluster: strings.TrimPrefix(kubeContext(), "k3d-")}
	host := &recHost{}
	opts := launch.Options{
		Dir: dir, Company: co, CompanyYAML: []byte(s), Images: images, DefaultBundle: true,
		Routes:      launch.RouteOptions{SiteHosts: []string{siteHost}, AdminHosts: []string{adminHost}}, // plain HTTP: a throwaway cluster cannot get a certificate
		SiteBaseURL: "http://" + siteHost,
		Timeouts:    launch.Timeouts{Rollout: 4 * time.Minute, CRD: 3 * time.Minute, Module: 6 * time.Minute},
		Backup: &launch.BackupInstall{
			Input: backup.KitInput{CompanyName: co.Name, Domain: co.Domain, Endpoint: "https://s3.example.invalid", Bucket: "e2e", Region: "auto",
				AccessKeyID: "AKIAE2EEXAMPLE", SecretAccessKey: "e2e-secret-0123456789abcdef", ProviderType: "ssh"},
			Mods:     map[string]*config.Module{"forms": formsMod},
			Settings: backup.DefaultSettings(),
		},
	}
	deps := launch.Deps{
		Kube: launch.ExecKube{Context: kubeContext()}, Loader: loader, Host: host, Store: store, Redactor: red,
		Out: red.Writer(io.MultiWriter(&progress, os.Stderr)),
	}

	started := time.Now()
	res, err := launch.Run(context.Background(), opts, deps)
	if err != nil {
		t.Fatalf("launch.Run: %v\nprogress:\n%s", err, progress.String())
	}
	t.Logf("launch.Run finished in %s", time.Since(started).Round(time.Second))
	writeTimings(t, res)

	// --- can-core is healthy --------------------------------------------------------------
	if got := mustKubectl(t, "-n", "can-system", "get", "deploy", "can-core", "-o", "jsonpath={.status.availableReplicas}"); got != "1" {
		t.Fatalf("can-core availableReplicas=%q", got)
	}
	if st, body, _ := edge(t, "GET", "http://"+adminHost+"/healthz", nil, nil); st != 200 {
		t.Fatalf("admin /healthz through Traefik: %d %s", st, body)
	}

	// --- the admin can log in with the generated password and TOTP --------------------------
	if res.Admin == nil || res.Admin.Password == "" || res.Admin.TOTPSecret == "" {
		t.Fatalf("no admin credentials issued: %+v", res.Admin)
	}
	key, err := auth.DecodeTOTPSecret(res.Admin.TOTPSecret)
	if err != nil {
		t.Fatal(err)
	}
	loginBody, _ := json.Marshal(map[string]string{"username": res.Admin.Username, "password": res.Admin.Password, "totp": auth.Code(key, auth.SHA1, 6, 30, time.Now())})
	st, body, hdr := edge(t, "POST", "http://"+adminHost+"/api/v1/login", map[string]string{"Content-Type": "application/json"}, bytes.NewReader(loginBody))
	if st != 200 {
		t.Fatalf("admin login with the generated password+TOTP: %d %s", st, body)
	}
	if !strings.Contains(strings.Join(hdr.Values("Set-Cookie"), ";"), auth.CookieName) {
		t.Fatalf("no session cookie: %v", hdr)
	}
	badBody, _ := json.Marshal(map[string]string{"username": res.Admin.Username, "password": res.Admin.Password + "x", "totp": auth.Code(key, auth.SHA1, 6, 30, time.Now())})
	if st, _, _ := edge(t, "POST", "http://"+adminHost+"/api/v1/login", map[string]string{"Content-Type": "application/json"}, bytes.NewReader(badBody)); st == 200 {
		t.Fatal("a wrong password must not log in")
	}

	// --- the site is served through Traefik with the example content ------------------------
	st, body, hdr = edge(t, "GET", "http://"+siteHost+"/", nil, nil)
	if st != 200 || !strings.Contains(body, "Heavy things, handled.") {
		t.Fatalf("site: %d %.200s", st, body)
	}
	if !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("the static server's security headers are missing: %v", hdr)
	}
	if st, _, _ := edge(t, "GET", "http://"+siteHost+"/robots.txt", nil, nil); st != 200 {
		t.Fatalf("robots.txt: %d", st)
	}

	// --- a form post through the same-origin /_forms path creates a lead ---------------------
	if !res.FormsRouted {
		t.Fatal("the forms route was not applied")
	}
	// Traefik loads a new route a moment after it is applied: wait for it, not for a fixed time.
	eventually(t, 45*time.Second, "the /_forms route to be live in Traefik", func() (bool, string) {
		st, body, _ = edge(t, "GET", "http://"+siteHost+"/_forms/contact/token", nil, nil)
		if st == 200 {
			return true, ""
		}
		ir, _ := kubectl("-n", "mod-forms", "get", "ingressroute,middleware", "-o", "yaml")
		return false, fmt.Sprintf("status %d %.120s\n%s", st, body, ir)
	})
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal([]byte(body), &tok)
	time.Sleep(4 * time.Second) // the gateway rejects a submit faster than min_fill_seconds (3)
	form := url.Values{"name": {"E2E Tester"}, "email": {"e2e@acme.example"}, "message": {"hello from the make-up e2e"}, "_t": {tok.Token}}
	st, body, _ = edge(t, "POST", "http://"+siteHost+"/_forms/contact", map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"}, strings.NewReader(form.Encode()))
	if st != 201 && st != 200 {
		t.Fatalf("form post: %d %s", st, body)
	}
	readTok := mustKubectl(t, "-n", "mod-forms", "get", "secret", "can-module", "-o", "go-template={{index .data \"CAN_FORMS_API_READ_TOKEN\" | base64decode}}")
	api := portForward(t, "mod-forms", "forms-api", 8081)
	st2, leads := apiGet(t, api+"/v1/leads", readTok)
	if st2 != 200 || !strings.Contains(leads, "e2e@acme.example") {
		t.Fatalf("the lead is not in the forms store: %d %s", st2, leads)
	}

	// --- the recovery kit parses, and matches what the host was given -------------------------
	if res.Kit == nil {
		t.Fatal("no recovery kit")
	}
	kit, err := backup.ParseKit([]byte(res.Kit.Text()), nil)
	if err != nil {
		t.Fatalf("the printed kit does not parse: %v", err)
	}
	if kit.Restic.Password == "" || kit.Restic.Password != res.Kit.Restic.Password || kit.Bucket.Name != "e2e" {
		t.Fatalf("kit mismatch: %+v", kit)
	}
	if string(host.copied["/etc/can/backup.d/restic.password"]) != kit.Restic.Password {
		t.Fatal("the password on the host differs from the kit's")
	}
	if _, ok := host.copied["/etc/systemd/system/can-backup.timer"]; !ok {
		t.Fatal("the timer unit was not installed on the host")
	}

	if !strings.Contains(strings.Join(res.Warnings, "\n"), "BACKUPS DO NOT WORK YET") {
		t.Fatalf("a backup that cannot work yet must say so: %v", res.Warnings)
	}

	// --- nothing secret on disk or in the progress log -----------------------------------------
	raw, _ := os.ReadFile(provider.StatePath(dir))
	for _, secret := range []string{res.Admin.Password, res.Admin.TOTPSecret, kit.Restic.Password, kit.Bucket.SecretAccessKey} {
		if strings.Contains(string(raw), secret) || strings.Contains(progress.String(), secret) {
			t.Fatalf("a secret leaked into state.json or the progress output")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".can", "kit")); err == nil {
		t.Fatal("a kit file appeared in the company directory")
	}

	// --- re-running changes nothing destructive ------------------------------------------------
	podBefore := mustKubectl(t, "-n", "can-system", "get", "pod", "-l", "app.kubernetes.io/name=site", "-o", "jsonpath={.items[0].metadata.uid}")
	if err := launch.MarkAdminShown(store); err != nil {
		t.Fatal(err)
	}
	if err := launch.MarkKitShown(store); err != nil {
		t.Fatal(err)
	}
	res2, err := launch.Run(context.Background(), opts, deps)
	if err != nil {
		t.Fatalf("second launch.Run: %v", err)
	}
	if res2.Admin != nil || res2.Kit != nil {
		t.Fatal("a re-run must not issue new credentials or a new kit once they were handed over")
	}
	if loader.loads != 1 {
		t.Fatalf("images were loaded %d times, want 1", loader.loads)
	}
	if podAfter := mustKubectl(t, "-n", "can-system", "get", "pod", "-l", "app.kubernetes.io/name=site", "-o", "jsonpath={.items[0].metadata.uid}"); podAfter != podBefore {
		t.Fatal("an unchanged site must not roll the site pod")
	}
	// the original admin password still works: the account was not touched
	loginBody2, _ := json.Marshal(map[string]string{"username": res.Admin.Username, "password": res.Admin.Password, "totp": auth.Code(key, auth.SHA1, 6, 30, time.Now().Add(30*time.Second))})
	if st, body, _ := edge(t, "POST", "http://"+adminHost+"/api/v1/login", map[string]string{"Content-Type": "application/json"}, bytes.NewReader(loginBody2)); st != 200 {
		t.Fatalf("the admin password changed across a re-run: %d %s", st, body)
	}
}

func apiGet(t *testing.T, u, bearer string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func writeTimings(t *testing.T, res *launch.Result) {
	t.Helper()
	var b strings.Builder
	b.WriteString("| phase | seconds | note |\n|---|---|---|\n")
	for _, p := range res.Phases {
		fmt.Fprintf(&b, "| %s | %.1f | %s |\n", p.Name, p.Duration.Seconds(), p.Note)
	}
	fmt.Fprintf(&b, "| **total** | **%.1f** | |\n", res.Total.Seconds())
	t.Logf("phase timings:\n%s", b.String())
	if f := os.Getenv("CAN_IT_TIMINGS"); f != "" {
		_ = os.WriteFile(f, []byte(b.String()), 0o600)
	}
}
