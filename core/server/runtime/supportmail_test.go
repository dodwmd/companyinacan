package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/installer"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

func TestAppIMAPBindsTheCredentialsToTheirHost(t *testing.T) {
	dir := t.TempDir()
	c := Config{Env: func(k string) string {
		if k == "CAN_SUPPORT_IMAP_SECRET_DIR" {
			return dir
		}
		return ""
	}}
	imap := map[string]any{"host": "imap.example.net", "username": "help@acme.example"}
	spec := reconcile.Spec{ID: "support", Domain: "acme.example", HMACSecret: "k", Manifest: &config.Module{ID: "support"},
		Settings: map[string]any{"support_address": "help@acme.example", "inbound": map[string]any{"imap": imap}}}
	get := func() *installer.IMAPAccount {
		t.Helper()
		a, err := c.appIMAP(spec)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	write := func(k, v string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if get() != nil {
		t.Fatal("no Secret mounted")
	}
	write("password", "first\n")
	if get() != nil {
		t.Fatal("a password without its host is not sent anywhere")
	}
	write("host", "imap.attacker.example")
	if get() != nil {
		t.Fatal("company.yaml names another host than the Secret")
	}
	write("host", "IMAP.example.net")
	want := installer.IMAPAccount{Host: "imap.example.net", Port: 993, Username: "help@acme.example", Password: "first", Encryption: "ssl", Folder: "INBOX", VerifyTLS: true}
	if a := get(); a == nil || *a != want {
		t.Fatalf("%+v", a)
	}
	write("username", "other")
	write("encryption", "starttls")
	write("tls_verify", "false")
	want.Username, want.Encryption, want.Port, want.VerifyTLS = "other", "starttls", 143, false
	if a := get(); a == nil || *a != want {
		t.Fatalf("%+v", a)
	}

	// The fingerprint covers the account without any relay opt-in, and follows the password.
	ra := reconcile.RelayAware(installer.New(installer.Config{IMAP: c.appIMAP}))
	fp := func() string {
		t.Helper()
		v, err := ra.RelayFingerprint(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := fp()
	write("password", "second")
	if got := fp(); first == "" || got == "" || got == first {
		t.Fatalf("a new password is not seen: %q then %q", first, got)
	}
	// Unreadable is not "no account": the module keeps what it has.
	if err := os.Remove(filepath.Join(dir, "password")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "password"), 0o700); err != nil {
		t.Fatal(err)
	}
	if a, err := c.appIMAP(spec); err == nil || a != nil {
		t.Fatalf("unreadable Secret: %+v %v", a, err)
	}
	// Nothing asked for in company.yaml: the Secret is not read at all.
	spec.Settings = map[string]any{"support_address": "help@acme.example"}
	if get() != nil || fp() != "" {
		t.Fatal("no inbound in company.yaml")
	}
}
