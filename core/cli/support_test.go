package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/launch"
)

func supportDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	y := `schema: 1
name: Acme
domain: acme.example
provider: {type: local}
modules:
  support:
    settings:
      support_address: help@acme.example
      inbound: {imap: {host: imap.example.net}}
`
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSupportCommands(t *testing.T) {
	dir := supportDir(t)
	report := `{"config":{"state":"ok"},"fetch":{"state":"ok"},"send":{"state":"ok"}}`
	var asked []string
	var fail error
	prev := supportKubectl
	t.Cleanup(func() { supportKubectl = prev })
	supportKubectl = func(_ context.Context, args ...string) (string, error) {
		asked = args
		if args[len(args)-1] == "status" {
			return "\n" + report + "\n", fail
		}
		return base64.StdEncoding.EncodeToString([]byte("generated-pw")), fail
	}

	code, out, errs := run(t, "support", "credentials", dir, "--context", "k3d-x")
	if code != ExitUsage || strings.Contains(out+errs, "generated-pw") {
		t.Fatalf("a password must not be printed to a pipe unasked: %d %s %s", code, out, errs)
	}
	code, out, errs = run(t, "support", "credentials", dir, "--context", "k3d-x", "--print-secrets")
	if code != ExitOK || !strings.Contains(out, "generated-pw") || !strings.Contains(out, "admin@acme.example") || !strings.Contains(out, "https://support.acme.example") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	if strings.Join(asked[:4], " ") != "--context k3d-x -n mod-support" {
		t.Errorf("asked %v", asked)
	}

	if code, out, errs = run(t, "support", "status", dir, "--context", "k3d-x"); code != ExitOK || !strings.Contains(out, "fetch:  ok") {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	report = `{"config":{"state":"ok"},"fetch":{"state":"failing","detail":"IMAP: cannot connect to the host and port"},"send":{"state":"ok"}}`
	fail = errors.New("exit status 1") // the in-container command's exit status when something fails
	if code, out, _ = run(t, "support", "status", dir, "--context", "k3d-x"); code != ExitFailure || !strings.Contains(out, "failing (IMAP: cannot connect") {
		t.Fatalf("%d %s", code, out)
	}
	report = "no such pod"
	if code, _, errs = run(t, "support", "status", dir, "--context", "k3d-x"); code != ExitFailure || !strings.Contains(errs, "did not report") {
		t.Fatalf("%d %s", code, errs)
	}
}

func TestSupportIMAPCredentialsFile(t *testing.T) {
	c := &config.Company{Domain: "acme.example", Modules: map[string]config.ModuleEntry{"support": {Settings: map[string]any{
		"support_address": "help@acme.example", "inbound": map[string]any{"imap": map[string]any{"host": "imap.example.net"}}}}}}
	if _, err := launch.SupportIMAPFrom(c, "", "pw"); err == nil || !strings.Contains(err.Error(), "IMAP_USERNAME") {
		t.Errorf("no user name anywhere: %v", err)
	}
	a, err := launch.SupportIMAPFrom(c, "help@acme.example", "pw")
	if err != nil || a.Host != "imap.example.net" {
		t.Fatalf("%v %+v", err, a)
	}
	if _, err := launch.SupportIMAPFrom(&config.Company{}, "u", "pw"); err == nil {
		t.Error("credentials without anything to use them for")
	}
}
