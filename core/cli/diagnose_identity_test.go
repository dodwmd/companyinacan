package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/provider"
)

func TestDiagnoseBundleDoesNotSayWhoseItIs(t *testing.T) {
	dir := writeCompany(t, `schema: 1
name: Wombat Widgets
domain: wombat-widgets.example
provider:
  type: ssh
  options:
    host: 203.0.113.7
    user: deployer
    key_file: /home/someone/.ssh/wombat_ed25519
    operator_ip: "198.51.100.9"
email:
  relay: {host: smtp.mailhouse.example, from: no-reply@wombat-widgets.example}
sso:
  enabled: true
  issuer: https://login.idp.example/9f1c2d3e-tenant/v2.0
  client_id: client-4711-abc
  client_secret_ref: sso-client
  allowed_email_domains: [wombat-corp.example]
  group_roles: {wombat-admins: [owner]}
  confirm_owner_mapping: true
`)
	if err := provider.NewStore(dir).Mutate(func(s *provider.State) error {
		s.Provider = "ssh"
		s.Server = &provider.ServerState{IP: "203.0.113.7", Name: "can-wombat-widgets", User: "deployer", Port: 22}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := &cliRunner{mu: map[string]string{"can-firewall check": "problem=pod 8.8.4.4 of Wombat Widgets on 203.0.113.7\nstatus=fail\n"}}
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return r }} }, nil)
	oldLook := lookPath
	defer func() { lookPath = oldLook }()
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }

	logf := filepath.Join(t.TempDir(), "can.log")
	log := "ssh deployer@203.0.113.7 -i /home/someone/.ssh/wombat_ed25519 failed for WOMBAT WIDGETS\n" +
		"GET https://admin.wombat-widgets.example/ from 198.51.100.9 via 203-0-113-7.sslip.io\n" +
		"oidc https://login.idp.example/9f1c2d3e-tenant/v2.0 client-4711-abc tenant 9f1c2d3e-tenant group wombat-admins wombat-corp.example\n" +
		"smtp.mailhouse.example refused; server can-wombat-widgets; deployment ready; pod 10.42.0.5\n"
	if err := os.WriteFile(logf, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	code, stdout, errs := run(t, "diagnose", "--server", "--company-dir", dir, "--modules-dir", "../../modules", "--log-file", logf, "--out", out)
	if code != ExitOK {
		t.Fatalf("code=%d out=%q err=%q", code, stdout, errs)
	}
	files := readTarball(t, out)
	for name, body := range files {
		low := strings.ToLower(body)
		for _, seed := range []string{"wombat", "203.0.113.7", "203-0-113-7", "198.51.100.9", "8.8.4.4", "deployer", "/home/someone", "9f1c2d3e",
			"client-4711", "login.idp.example", "mailhouse"} {
			if strings.Contains(low, seed) {
				t.Errorf("%s says %q:\n%s", name, seed, body)
			}
		}
	}
	// What a maintainer needs survives.
	for _, want := range []string{"deployment ready", "10.42.0.5", "<server>"} {
		if !strings.Contains(files["logs.txt"], want) {
			t.Errorf("logs.txt lost %q: %s", want, files["logs.txt"])
		}
	}
	if !strings.Contains(files["config.yaml"], "provider:") || !strings.Contains(files["doctor.json"], "node firewall") {
		t.Errorf("config or checks missing: %v", files)
	}
}

func TestDiagnoseLeavesOutAConfigItCannotRead(t *testing.T) {
	dir := writeCompany(t, "name: Wombat Widgets\n\tdomain: [broken\n")
	oldLook := lookPath
	defer func() { lookPath = oldLook }()
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	code, stdout, errs := run(t, "diagnose", "--company-dir", dir, "--out", out)
	if code != ExitOK || !strings.Contains(stdout, "left out") {
		t.Fatalf("code=%d out=%q err=%q", code, stdout, errs)
	}
	if body, ok := readTarball(t, out)["config.yaml"]; ok {
		t.Errorf("config.yaml: %s", body)
	}
}
