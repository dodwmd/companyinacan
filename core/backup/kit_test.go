package backup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKit(t *testing.T) *Kit {
	t.Helper()
	k, err := NewKit(KitInput{
		CompanyName: "Acme", Domain: "acme.example",
		Endpoint: "https://s3.example.invalid", Bucket: "acme-backups", Region: "auto",
		AccessKeyID: "AKIAFAKE", SecretAccessKey: "fakesecret",
		ProviderType:    "hetzner",
		ProviderOptions: map[string]any{"location": "fsn1", "server_type": "cx22", "token": "SHOULD-NOT-APPEAR", "api_token_ref": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestNewKit(t *testing.T) {
	k := testKit(t)
	if len(k.Restic.Password) < 32 {
		t.Errorf("password too short: %d", len(k.Restic.Password))
	}
	if k.Restic.Repository != "s3:https://s3.example.invalid/acme-backups/restic" {
		t.Errorf("repository = %q", k.Restic.Repository)
	}
	if _, ok := k.Provider.Options["token"]; ok {
		t.Error("secret-looking provider option copied into kit")
	}
	if k.Provider.Options["location"] != "fsn1" {
		t.Errorf("options = %v", k.Provider.Options)
	}
	k2 := testKit(t)
	if k.Restic.Password == k2.Restic.Password {
		t.Error("password not random")
	}
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestKitValidate(t *testing.T) {
	mut := []func(*Kit){
		func(k *Kit) { k.Restic.Password = "short" },
		func(k *Kit) { k.Restic.Repository = "/local/path" },
		func(k *Kit) { k.Bucket.AccessKeyID = "" },
		func(k *Kit) { k.Bucket.SecretAccessKey = "" },
		func(k *Kit) { k.Provider.Type = "" },
		func(k *Kit) { k.Version = 99 },
	}
	for i, f := range mut {
		k := testKit(t)
		f(k)
		if k.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestKitTextRoundTrip(t *testing.T) {
	k := testKit(t)
	txt := k.Text()
	if !strings.Contains(txt, k.Restic.Password) || !strings.Contains(txt, "acme.example") {
		t.Fatal("printed kit lacks the essentials")
	}
	got, err := ParseKit([]byte("noise before\n"+txt+"\nnoise after"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Restic.Password != k.Restic.Password || got.Bucket.SecretAccessKey != k.Bucket.SecretAccessKey {
		t.Fatal("round trip mismatch")
	}
	// Raw JSON is accepted too (stdin pipelines).
	if _, err := ParseKit([]byte(`{"version":1}`), nil); err == nil {
		t.Error("invalid kit accepted")
	}
}

func TestKitEncryption(t *testing.T) {
	k := testKit(t)
	enc, err := EncryptKit(k, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(enc, []byte(k.Restic.Password)) || bytes.Contains(enc, []byte("AKIAFAKE")) {
		t.Fatal("plaintext visible in encrypted kit")
	}
	ask := func(string) (string, error) { return "correct horse battery", nil }
	got, err := ParseKit(enc, ask)
	if err != nil || got.Restic.Password != k.Restic.Password {
		t.Fatalf("decrypt: %v", err)
	}
	wrong := func(string) (string, error) { return "wrong passphrase!!", nil }
	if _, err := ParseKit(enc, wrong); !errors.Is(err, ErrBadPassphrase) {
		t.Errorf("wrong passphrase: %v", err)
	}
	if _, err := ParseKit(enc, nil); err == nil {
		t.Error("encrypted kit without passphrase source accepted")
	}
	// Tampering is detected.
	tampered := bytes.Replace(enc, []byte("A"), []byte("B"), 3)
	if bytes.Equal(tampered, enc) {
		tampered = append(enc[:len(enc)-30:len(enc)-30], enc[len(enc)-29:]...)
	}
	if _, err := ParseKit(tampered, ask); err == nil {
		t.Error("tampered kit accepted")
	}
	if _, err := EncryptKit(k, "short"); err == nil {
		t.Error("weak passphrase accepted")
	}
	// Two encryptions differ (random salt and nonce).
	enc2, _ := EncryptKit(k, "correct horse battery")
	if bytes.Equal(enc, enc2) {
		t.Error("encryption is deterministic")
	}
}

func TestPrintOnce(t *testing.T) {
	k := testKit(t)
	var out bytes.Buffer
	if err := PrintOnce(strings.NewReader("show\nsaved\n"), &out, k); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), k.Restic.Password) {
		t.Error("kit not printed")
	}
	// Never printed without the first confirmation.
	out.Reset()
	if err := PrintOnce(strings.NewReader("no\n"), &out, k); !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(out.String(), k.Restic.Password) {
		t.Error("kit printed without confirmation")
	}
	// Not acknowledged after printing: error, so `make up` can warn loudly.
	out.Reset()
	if err := PrintOnce(strings.NewReader("show\n\n\n\n"), &out, k); !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("err = %v", err)
	}
}

func TestWriteKitFileRefusesRepo(t *testing.T) {
	k := testKit(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "deep")
	_ = os.Mkdir(sub, 0o755)
	if err := WriteKitFile(filepath.Join(sub, "kit.age"), k, "a good passphrase"); err == nil {
		t.Fatal("kit written inside a git working tree")
	}
	out := filepath.Join(t.TempDir(), "kit.enc")
	if err := WriteKitFile(out, k, "a good passphrase"); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(out)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode())
	}
	if err := WriteKitFile(out, k, "a good passphrase"); err == nil {
		t.Error("existing file overwritten")
	}
}

func TestKitRepoAndPasswordFile(t *testing.T) {
	k := testKit(t)
	path, cleanup, err := k.PasswordFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if string(b) != k.Restic.Password || st.Mode().Perm() != 0o600 {
		t.Error("password file wrong")
	}
	r := k.Repo(path)
	env := strings.Join(r.Env, "\n")
	if !strings.Contains(env, "AWS_ACCESS_KEY_ID=AKIAFAKE") || !strings.Contains(env, "AWS_SECRET_ACCESS_KEY=fakesecret") {
		t.Errorf("env = %v", r.Env)
	}
	cleanup()
	if _, err := os.Stat(path); err == nil {
		t.Error("password file not removed")
	}
}
