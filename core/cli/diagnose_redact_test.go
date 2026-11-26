package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func redactorFor(t *testing.T, company string) (*identity, bool) {
	t.Helper()
	return identityFrom(t.TempDir(), company)
}

func TestRedactIPv6(t *testing.T) {
	id, _ := redactorFor(t, "")
	for _, in := range []string{
		"from 2a01:4f8:c17:1234::1 refused",
		"dial tcp [2A01:4F8:C17:1234::1]:443: i/o timeout",
		"client ip:2606:4700:4700::1111, retry",
		"addr=2001:db8:85a3:0:0:8a2e:370:7334.",
		"peer 2a01:4f8:c17:1234::1%eth0 gone",
		"src ::ffff:8.8.4.4 closed",
		"read 2001:db8::1: reset",
		"https://x/?a=2001:db8::",
	} {
		got := id.redact(in)
		for _, leak := range []string{"2a01", "2A01", "4700", "8a2e", "8.8.4.4", "db8"} {
			if strings.Contains(got, leak) {
				t.Errorf("%q -> %q", in, got)
				break
			}
		}
		if !strings.Contains(got, "<address>") {
			t.Errorf("%q -> %q", in, got)
		}
	}
	for _, keep := range []string{
		"at 12:34:56.789 done", "mac aa:bb:cc:dd:ee:ff", "listen [::]:8080 and [::1]:8081", "pod fd00:42::5 node fe80::1%cni0",
		"std::string and Foo::Bar", "image sha256:abcdef", "10.42.0.5:443", "ratio 1:2:3", "key: value",
	} {
		if got := id.redact(keep); got != keep {
			t.Errorf("changed %q -> %q", keep, got)
		}
	}
}

func TestRedactSslipLabels(t *testing.T) {
	id, _ := redactorFor(t, "")
	for in, want := range map[string]string{
		"https://admin.8-8-4-4.sslip.io/x": "https://admin.<address-dashed>.sslip.io/x",
		"app-8-8-4-4.nip.io":               "app-<address-dashed>.nip.io",
		"2a01-4f8-c17-1234--1.sslip.io":    "<address-dashed>.sslip.io",
		"web.2606-4700--1111.SSLIP.IO":     "web.<address-dashed>.SSLIP.IO",
		"08080404.nip.io":                  "<address-dashed>.nip.io",
		"10-42-0-5.sslip.io":               "10-42-0-5.sslip.io",
		"the sslip.io probe":               "the sslip.io probe",
		"admin.8.8.4.4.sslip.io":           "admin.<address>.sslip.io",
	} {
		if got := id.redact(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestRedactNameVariants(t *testing.T) {
	id, ok := redactorFor(t, "name: Wombat Widgets Pty Ltd\n")
	if !ok {
		t.Fatal("not parsed")
	}
	for _, v := range []string{
		"Wombat Widgets Pty Ltd", "wombat_widgets_pty_ltd", "WOMBAT-WIDGETS", "wombatwidgets", "WombatWidgetsService", "wombat.widgets",
		"wombat%20widgets", "db_wombat_widgets_prod", "wombat  widgets",
	} {
		if got := id.redact("x " + v + " y"); strings.Contains(strings.ToLower(got), "wombat") {
			t.Errorf("%q -> %q", v, got)
		}
	}
	short, _ := redactorFor(t, "name: Acme Co\n")
	if got := short.redact("acme_db ACME acmeco; acmeology"); got != "<company>_db <company> <company>; acmeology" {
		t.Errorf("short name: %q", got)
	}
	umlaut, _ := redactorFor(t, "name: Müller Büro\n")
	for _, v := range []string{"MÜLLER BÜRO", "muller-buro", "mueller_buero", "MuellerBuero"} {
		if got := umlaut.redact("host " + v + " down"); got != "host <company> down" {
			t.Errorf("%q -> %q", v, got)
		}
	}
	the, _ := redactorFor(t, "name: The Company\n")
	if got := the.redact("the server of The Company"); got != "the server of <company>" {
		t.Errorf("%q", got)
	}
}

// A company.yaml with a value of the wrong type, or one that does not parse, still scrubs the logs.
func TestDiagnoseScrubsLogsWhenCompanyYAMLIsWrong(t *testing.T) {
	oldLook := lookPath
	defer func() { lookPath = oldLook }()
	lookPath = func(string) (string, error) { return "", os.ErrNotExist }
	log := "Wombat Widgets: GET admin.wombat.example from 2a01:4f8::7 via smtp.mailhouse.example for jürgen@bücher.example\n" +
		"sso https://login.idp.example/tenant-9f1c user deployer wombat_widgets\n"
	for name, company := range map[string]string{
		"type errors": "schema: one\nname: Wombat Widgets\ndomain: [wombat.example]\nprovider: {type: ssh, options: {user: deployer, port: [1]}}\n" +
			"sso: https://login.idp.example/tenant-9f1c\nemail: {relay: smtp.mailhouse.example}\nmodules: 7\n",
		"duplicate key": "name: Wombat Widgets\ndomain: wombat.example\ndomain: wombat.example\nprovider: {options: {user: deployer}}\n" +
			"sso: {issuer: 'https://login.idp.example/tenant-9f1c'}\nemail: {relay: {host: smtp.mailhouse.example}}\n",
		"syntax error": "name: Wombat Widgets\ndomain: wombat.example\n\tbroken: [\nprovider:\n  options: {user: deployer}\n" +
			"sso:\n  issuer: https://login.idp.example/tenant-9f1c\nemail:\n  relay: {host: smtp.mailhouse.example}\n",
	} {
		dir := writeCompany(t, company)
		logf := filepath.Join(t.TempDir(), "can.log")
		if err := os.WriteFile(logf, []byte(log), 0o600); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "bundle.tar.gz")
		if code, stdout, errs := run(t, "diagnose", "--company-dir", dir, "--log-file", logf, "--out", out); code != ExitOK {
			t.Fatalf("%s: code=%d out=%q err=%q", name, code, stdout, errs)
		}
		files := readTarball(t, out)
		if files["logs.txt"] == "" {
			t.Fatalf("%s: no logs", name)
		}
		for file, body := range files {
			low := strings.ToLower(body)
			for _, seed := range []string{"wombat", "mailhouse", "login.idp.example", "tenant-9f1c", "deployer", "2a01:4f8", "jürgen", "bücher"} {
				if strings.Contains(low, seed) {
					t.Errorf("%s: %s says %q:\n%s", name, file, seed, body)
				}
			}
		}
	}
}
