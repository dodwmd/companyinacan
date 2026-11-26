package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/email"
	"github.com/dodwmd/companyinacan/core/server/installer"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// The fingerprint follows the mounted relay Secret: empty while it is not there, then one value per
// content, and nothing for a module the company did not opt in.
func TestRelayFingerprintFollowsTheMountedSecret(t *testing.T) {
	dir := t.TempDir()
	c := Config{CompanyFile: filepath.Join(dir, "company.yaml"), Env: func(k string) string {
		if k == "CAN_SMTP_SECRET_DIR" {
			return dir
		}
		return ""
	}}
	in := installer.New(installer.Config{Relay: c.appRelay})
	man := &config.Module{ID: "support"}
	man.Requires.Capabilities = []string{"email.send"}
	on := true
	spec := reconcile.Spec{ID: "support", Manifest: man, Domain: "acme.example", HMACSecret: "k",
		Email: &config.Email{Modules: map[string]config.EmailModule{"support": {Enabled: &on}}}}
	var ra reconcile.RelayAware = in
	fp := func() string {
		t.Helper()
		v, err := ra.RelayFingerprint(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := fp(); got != "" {
		t.Fatalf("no Secret mounted: %q", got)
	}
	write := func(k, v string) {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("host", "smtp.example.net")
	write("username", "u")
	write("password", "first")
	first := fp()
	if first == "" || first != fp() {
		t.Fatalf("not stable: %q", first)
	}
	write("password", "second")
	if got := fp(); got == "" || got == first {
		t.Fatalf("a new password is not seen: %q", got)
	}
	// Unreadable (here: a directory in a key's place) is not "no relay": neither the reconciler nor the
	// installer may take it for a removed Secret and empty can-smtp.
	if err := os.Remove(filepath.Join(dir, "password")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "password"), 0o700); err != nil {
		t.Fatal(err)
	}
	if v, err := ra.RelayFingerprint(context.Background(), spec); !errors.Is(err, email.ErrSecretUnreadable) {
		t.Fatalf("fingerprint of an unreadable Secret: %q %v", v, err)
	}
	if r, err := c.appRelay(spec); r != nil || !errors.Is(err, email.ErrSecretUnreadable) {
		t.Fatalf("relay of an unreadable Secret: %v %v", r != nil, err)
	}
	spec.Email = nil
	if got := fp(); got != "" {
		t.Fatalf("not opted in: %q", got)
	}
}
