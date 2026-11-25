package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const siteCompany = `schema: 1
name: Acme Co
domain: acme.example
provider: {type: local}
site:
  tagline: Anvils, delivered.
  hero: {headline: Hello, cta: {label: Go, href: "#x"}}
`

func TestSiteBuild(t *testing.T) {
	dir := writeCompany(t, siteCompany)
	out := filepath.Join(t.TempDir(), "public")
	code, stdout, stderr := run(t, "site", "build", dir, "--out", out)
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, stdout, stderr)
	}
	for _, f := range []string{"index.html", "404.html", "robots.txt", "sitemap.xml", "index.html.gz", ".can-site"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if !strings.Contains(stdout, "index.html") {
		t.Errorf("summary should list files: %q", stdout)
	}
}

func TestSiteBuildDefaultOut(t *testing.T) {
	dir := writeCompany(t, siteCompany)
	if code, _, e := run(t, "site", "build", dir); code != ExitOK {
		t.Fatalf("code=%d %s", code, e)
	}
	if _, err := os.Stat(filepath.Join(dir, "public", "index.html")); err != nil {
		t.Errorf("default output should be <dir>/public: %v", err)
	}
}

func TestSiteBuildNoCompressAndBaseURL(t *testing.T) {
	dir := writeCompany(t, siteCompany)
	out := t.TempDir()
	if code, _, e := run(t, "site", "build", dir, "--out", out, "--no-compress", "--base-url", "https://staging.acme.example"); code != ExitOK {
		t.Fatalf("code=%d %s", code, e)
	}
	if _, err := os.Stat(filepath.Join(out, "index.html.gz")); err == nil {
		t.Error("--no-compress still wrote .gz")
	}
	b, _ := os.ReadFile(filepath.Join(out, "index.html"))
	if !strings.Contains(string(b), `href="https://staging.acme.example/"`) {
		t.Error("--base-url not applied")
	}
}

func TestSiteBuildReportsProblemsAndWritesNothing(t *testing.T) {
	dir := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nsite:\n  hero: {headline: h, cta: {label: x, href: \"javascript:alert(1)\"}}\n  faq: [{question: q}]\n")
	out := filepath.Join(t.TempDir(), "public")
	code, stdout, _ := run(t, "site", "build", dir, "--out", out)
	if code != ExitFailure {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"/site/hero/cta/href", "/site/faq/0", "company.yaml:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("output directory must not be created when validation fails")
	}
}

func TestSiteBuildRendererErrorIsReported(t *testing.T) {
	dir := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nsite:\n  brand: {logo: missing.svg}\n")
	code, _, stderr := run(t, "site", "build", dir, "--out", filepath.Join(t.TempDir(), "o"))
	if code != ExitFailure || !strings.Contains(stderr, "site.brand.logo") {
		t.Fatalf("code=%d err=%q", code, stderr)
	}
}

func TestSiteBuildWithLogo(t *testing.T) {
	dir := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nsite:\n  brand: {logo: logo.svg}\n")
	if err := os.WriteFile(filepath.Join(dir, "logo.svg"), []byte("<svg/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if code, _, e := run(t, "site", "build", dir, "--out", out); code != ExitOK {
		t.Fatalf("code=%d %s", code, e)
	}
	if _, err := os.Stat(filepath.Join(out, "assets", "logo.svg")); err != nil {
		t.Error(err)
	}
}

func TestSiteBuildRefusesForeignOutDir(t *testing.T) {
	dir := writeCompany(t, siteCompany)
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "site", "build", dir, "--out", out)
	if code != ExitFailure || !strings.Contains(stderr, "not a site output") {
		t.Fatalf("code=%d err=%q", code, stderr)
	}
	if code, _, _ := run(t, "site", "build", dir, "--out", out, "--force"); code != ExitOK {
		t.Fatalf("--force: code=%d", code)
	}
}

func TestSiteBuildJSON(t *testing.T) {
	dir := writeCompany(t, siteCompany)
	out := t.TempDir()
	code, stdout, _ := run(t, "site", "build", dir, "--out", out, "--output", "json")
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	var res struct {
		Out   string   `json:"out"`
		Files []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || res.Out != out || len(res.Files) < 4 {
		t.Fatalf("bad json %v %q", err, stdout)
	}
}

func TestSiteBuildMissingCompanyYAML(t *testing.T) {
	code, stdout, _ := run(t, "site", "build", t.TempDir())
	if code != ExitFailure || !strings.Contains(stdout, "parse.read") {
		t.Fatalf("code=%d out=%q", code, stdout)
	}
}

func TestSiteBuildExample(t *testing.T) {
	out := t.TempDir()
	if code, _, e := run(t, "site", "build", "../../examples/acme-co", "--out", out); code != ExitOK {
		t.Fatalf("code=%d %s", code, e)
	}
	b, _ := os.ReadFile(filepath.Join(out, "index.html"))
	if !strings.Contains(string(b), "Acme Co") {
		t.Error("example did not render")
	}
}

func TestSiteBuildUsage(t *testing.T) {
	if code, _, _ := run(t, "site", "build", "a", "b"); code != ExitUsage {
		t.Errorf("two args: code=%d", code)
	}
	if code, _, _ := run(t, "site", "bogus"); code == ExitOK {
		t.Error("unknown subcommand must fail")
	}
}

func TestServeSite(t *testing.T) {
	dir := writeCompany(t, siteCompany)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() {
		done <- serveSite(ctx, serveOptions{Dir: dir, Addr: "127.0.0.1:0"}, &out, func(a string) { ready <- a })
	}()
	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	get := func(p string) (int, string, http.Header) {
		resp, err := http.Get("http://" + addr + p) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	code, body, hdr := get("/")
	if code != 200 || !strings.Contains(body, "Acme Co") || hdr.Get("Content-Security-Policy") == "" {
		t.Fatalf("code=%d body=%q", code, body)
	}
	if !strings.Contains(body, `href="http://`+addr+`/"`) {
		t.Error("preview must use the local address as base URL")
	}
	if code, body, _ := get("/missing"); code != 404 || !strings.Contains(body, "Page not found") {
		t.Errorf("404: %d %q", code, body)
	}

	// Editing company.yaml is picked up without restarting.
	time.Sleep(10 * time.Millisecond)
	edited := strings.Replace(siteCompany, "Acme Co", "Beta Co", 1)
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(filepath.Join(dir, "company.yaml"), future, future)
	if _, body, _ := get("/"); !strings.Contains(body, "Beta Co") {
		t.Error("edited company.yaml was not re-rendered")
	}

	// A broken edit keeps the last good site and reports the problem on stdout.
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), []byte("schema: 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future = future.Add(2 * time.Second)
	_ = os.Chtimes(filepath.Join(dir, "company.yaml"), future, future)
	if code, body, _ := get("/"); code != 200 || !strings.Contains(body, "Beta Co") {
		t.Errorf("broken edit must keep the last good build: %d", code)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServeSiteFailsOnInvalidCompany(t *testing.T) {
	dir := writeCompany(t, "schema: 9\n")
	err := serveSite(context.Background(), serveOptions{Dir: dir, Addr: "127.0.0.1:0"}, io.Discard, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestServePrebuiltDirectory(t *testing.T) {
	out := t.TempDir()
	if code, _, e := run(t, "site", "build", writeCompany(t, siteCompany), "--out", out); code != ExitOK {
		t.Fatalf("%s", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- serveSite(ctx, serveOptions{Public: out, Addr: "127.0.0.1:0"}, io.Discard, func(a string) { ready <- a })
	}()
	addr := <-ready
	resp, err := http.Get("http://" + addr + "/robots.txt") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status %d", resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
}
