package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/doctor"
)

func readTarball(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

func TestDiagnoseBundleHasNoSeededSecrets(t *testing.T) {
	oldLook, oldRun := lookPath, reachRunner
	defer func() { lookPath, reachRunner = oldLook, oldRun }()
	lookPath = func(n string) (string, error) { return "", os.ErrNotExist }
	reachRunner = func(context.Context, string, ...string) (string, error) { return "", errors.New("must not run") }

	const (
		pw    = "hunter2-PASSWORD-seed"
		tok   = "metrics-token-seed-ABCDEF"
		email = "alice.smith@example.com"
		bear  = "BearerSeedValue987"
	)
	dir := writeCompany(t, "schema: 1\nname: Acme\ndomain: acme.example\nowner: "+email+"\nprovider: {type: local}\nsmtp:\n  password: "+pw+"\n")
	logf := filepath.Join(t.TempDir(), "can.log")
	if err := os.WriteFile(logf, []byte(`{"msg":"started"}`+"\nAuthorization: Bearer "+bear+"\nlogin failed for "+email+" password="+pw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "# HELP can_up x\n# TYPE can_up gauge\ncan_up 1\ncan_leak{who=\""+email+"\"} 1\nsecret_internal "+pw+"\n")
	}))
	defer srv.Close()
	t.Setenv("CAN_METRICS_TOKEN", tok)
	t.Setenv("SOME_API_TOKEN", "env-secret-seed-123456")

	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	code, stdout, errs := run(t, "diagnose", "--company-dir", dir, "--log-file", logf, "--metrics-url", srv.URL+"/metrics", "--out", out)
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, stdout, errs)
	}
	if gotAuth != "Bearer "+tok {
		t.Fatalf("scrape did not use CAN_METRICS_TOKEN: %q", gotAuth)
	}
	files := readTarball(t, out)
	for _, want := range []string{"manifest.json", "versions.json", "config.yaml", "doctor.json", "logs.txt", "metrics.txt"} {
		if _, ok := files[want]; !ok {
			t.Errorf("bundle lacks %s: %v", want, files)
		}
	}
	for name, body := range files {
		for _, seed := range []string{pw, tok, email, bear, "env-secret-seed-123456"} {
			if strings.Contains(body, seed) {
				t.Errorf("%s leaks %q", name, seed)
			}
		}
	}
	if strings.Contains(files["metrics.txt"], "secret_internal") {
		t.Error("only can_, go_ and process_ series belong in the metrics snapshot")
	}
	if !strings.Contains(files["config.yaml"], "name: Acme") || !strings.Contains(files["metrics.txt"], "can_up 1") {
		t.Errorf("useful content lost: %q / %q", files["config.yaml"], files["metrics.txt"])
	}
	st, err := os.Stat(out)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode: %v %v", st, err)
	}
	if strings.Contains(stdout, pw) || strings.Contains(stdout, tok) {
		t.Fatal("secrets on stdout")
	}
	// never overwrite an existing file
	if code, _, _ := run(t, "diagnose", "--company-dir", dir, "--out", out); code == ExitOK {
		t.Fatal("overwrote an existing bundle")
	}
}

func TestDiagnoseWithoutAnythingStillWorks(t *testing.T) {
	oldLook := lookPath
	defer func() { lookPath = oldLook }()
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if code, _, errs := run(t, "diagnose", "--company-dir", t.TempDir(), "--out", out); code != ExitOK {
		t.Fatalf("code=%d %s", code, errs)
	}
	files := readTarball(t, out)
	if _, ok := files["versions.json"]; !ok {
		t.Fatalf("%v", files)
	}
	if _, ok := files["config.yaml"]; ok {
		t.Fatal("no company.yaml: the file must be absent, not empty")
	}
}

func TestDiagnoseRejectsMetricsURLWithCredentials(t *testing.T) {
	if code, _, _ := run(t, "diagnose", "--metrics-url", "http://user:pw@127.0.0.1:1/metrics", "--out", filepath.Join(t.TempDir(), "x.tgz")); code == ExitOK {
		t.Fatal("credentials in a URL accepted")
	}
}

func TestDoctorReportsReachability(t *testing.T) {
	oldLook, oldRun := lookPath, reachRunner
	defer func() { lookPath, reachRunner = oldLook, oldRun }()
	lookPath = func(n string) (string, error) {
		if n == "go" || n == "git" || n == "make" || n == "docker" {
			return "/bin/" + n, nil
		}
		return "", os.ErrNotExist
	}
	reachRunner = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "docker" && len(args) > 0 && args[0] == "info" {
			return "", errors.New("Cannot connect to the Docker daemon")
		}
		return "", nil
	}
	code, out, _ := run(t, "doctor")
	if code != ExitOK || !strings.Contains(out, doctor.StatusUnreachable) {
		t.Fatalf("an unreachable daemon is reported but does not fail doctor: code=%d\n%s", code, out)
	}
}

func TestDiagnoseRejectsUnsafeComposeProject(t *testing.T) {
	oldRun := reachRunner
	defer func() { reachRunner = oldRun }()
	called := false
	reachRunner = func(context.Context, string, ...string) (string, error) { called = true; return "", nil }
	for _, bad := range []string{"--file=/etc/passwd", "a b", "A", "x;y", "../x"} {
		if code, _, _ := run(t, "diagnose", "--compose-project", bad, "--out", filepath.Join(t.TempDir(), "x.tgz")); code != ExitUsage {
			t.Errorf("%q: code %d, want usage error", bad, code)
		}
	}
	if called {
		t.Fatal("docker was invoked with an unsafe project name")
	}
}
