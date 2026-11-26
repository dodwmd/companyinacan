package cli

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/provider"
)

func serverCompany(t *testing.T, checkOutput string) (string, *cliRunner) {
	t.Helper()
	dir := sshCompany(t)
	if err := provider.NewStore(dir).Mutate(func(s *provider.State) error {
		s.Provider = "ssh"
		s.Server = &provider.ServerState{IP: "203.0.113.7", User: "root", Port: 22}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := &cliRunner{mu: map[string]string{}}
	if checkOutput != "" {
		r.mu["can-firewall check"] = checkOutput
	}
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, nil)
	return dir, r
}

func TestDoctorServerFirewall(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		code      int
		want      string
	}{
		{"enforcing", "rules=ok\nstatus=ok\n", ExitOK, "pods cannot reach node services"},
		{"not enforcing", "problem=the isolation rules are not loaded\nstatus=fail\n", ExitFailure, "rules are not loaded"},
		{"never installed", "status=not-installed\n", ExitFailure, "not installed"},
		{"ssh fails", "", ExitFailure, "could not be checked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, r := serverCompany(t, tc.out)
			code, out, errs := run(t, "doctor", "--server", "--company-dir", dir, "--modules-dir", "../../modules")
			if code != tc.code || !strings.Contains(out, tc.want) || !strings.Contains(out, "node firewall") {
				t.Fatalf("code=%d out=%q err=%q", code, out, errs)
			}
			if len(r.ran) != 1 || len(r.copied) != 0 {
				t.Fatalf("want one read-only script, ran %d copied %d", len(r.ran), len(r.copied))
			}
		})
	}
}

func TestDoctorServerWithoutServerFails(t *testing.T) {
	dir := sshCompany(t)
	code, out, _ := run(t, "doctor", "--server", "--company-dir", dir, "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(out, "no server recorded") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestDiagnoseServerIncludesFirewall(t *testing.T) {
	dir, _ := serverCompany(t, "problem=the isolation rules are not loaded\nstatus=fail\n")
	bundle := filepath.Join(t.TempDir(), "d.tar.gz")
	code, _, errs := run(t, "diagnose", "--server", "--company-dir", dir, "--modules-dir", "../../modules", "--out", bundle)
	if code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	f, err := os.Open(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("no doctor.json with the firewall result: %v", err)
		}
		if strings.HasSuffix(h.Name, "doctor.json") {
			b, _ := io.ReadAll(tr)
			if !strings.Contains(string(b), "node firewall") || !strings.Contains(string(b), "rules are not loaded") {
				t.Fatalf("doctor.json: %s", b)
			}
			return
		}
	}
}

func TestUpAndStatusWarnWithoutNodeFirewall(t *testing.T) {
	for _, tc := range []struct{ name, check, want string }{
		{"never installed", "status=not-installed\n", "has no node firewall"},
		{"disabled", "rules=missing\nstatus=fail\n", "not enforcing"},
		{"dormant", "rules=dormant\nstatus=fail\n", "not enforcing"},
		{"enforcing", "rules=ok\nstatus=ok\n", ""},
		{"enforcing with a stopped service", "rules=ok\nstatus=fail\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := sshCompany(t)
			r := newRunner()
			r.mu["can-firewall check"] = tc.check
			useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
			for _, args := range [][]string{{"up", "--provision-only"}, {"status"}, {"up", "--provision-only", "--output", "json"}, {"status", "--output", "json"}} {
				code, out, errs := run(t, append(args, dir, "--modules-dir", "../../modules")...)
				if code != ExitOK {
					t.Fatalf("%v: a warning must not fail the command: code=%d out=%q err=%q", args, code, out, errs)
				}
				if tc.want == "" {
					if strings.Contains(out+errs, "node firewall") {
						t.Errorf("%v: unexpected warning: %q %q", args, out, errs)
					}
					continue
				}
				// In JSON mode progress goes to stderr and the warning is also in the document.
				if !strings.Contains(out, tc.want) {
					t.Errorf("%v: want %q in %q (stderr %q)", args, tc.want, out, errs)
				}
			}
		})
	}
}

// QA of #53, 13: a server whose bootstrap has not finished has no firewall yet, which is not news.
func TestStatusDoesNotWarnWhileProvisioning(t *testing.T) {
	dir, r := serverCompany(t, "status=not-installed\n")
	r.mu["k3s_active"] = "bootstrapped=\nk3s_active=inactive\nnodes_ready=0\n"
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, goodResolver())
	for _, args := range [][]string{{"status"}, {"status", "--output", "json"}} {
		code, out, errs := run(t, append(args, dir, "--modules-dir", "../../modules")...)
		if code != ExitFailure || !strings.Contains(out, string(provider.PhaseProvisioning)) || strings.Contains(out+errs, "node firewall") {
			t.Errorf("%v: code=%d out=%q err=%q", args, code, out, errs)
		}
	}
}
