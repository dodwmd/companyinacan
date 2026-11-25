package files

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDefaultsAreSafe(t *testing.T) {
	c, err := ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.ScannerRequired() {
		t.Error("scanner.required must default to true: no upload is accepted unscanned")
	}
	if c.Download.Inline {
		t.Error("inline display must default to off")
	}
	if c.Backend.Type != "local" || c.Limits.MaxFileBytes != 25<<20 || c.Share.DefaultTTLHours != 24 {
		t.Errorf("defaults: %+v", c)
	}
}

func TestConfigValidation(t *testing.T) {
	bad := map[string]string{
		"unknown key":          "bogus: 1",
		"huge file":            "limits: { max_file_bytes: 99999999999 }",
		"quota below file":     "limits: { max_file_bytes: 1000, quota_bytes_per_owner: 10 }",
		"unknown type":         "limits: { allowed_types: [exe] }",
		"s3 plain http":        "backend: { type: s3, s3: { endpoint: 'http://s3.example.com', bucket: my-bucket } }",
		"s3 bad bucket":        "backend: { type: s3, s3: { endpoint: 'https://s3.example.com', bucket: 'A_B' } }",
		"s3 creds in url":      "backend: { type: s3, s3: { endpoint: 'https://u:p@s3.example.com', bucket: my-bucket } }",
		"s3 prefix escape":     "backend: { type: s3, s3: { endpoint: 'https://s3.example.com', bucket: my-bucket, prefix: '../x/' } }",
		"s3 prefix no slash":   "backend: { type: s3, s3: { endpoint: 'https://s3.example.com', bucket: my-bucket, prefix: 'x' } }",
		"unknown backend":      "backend: { type: ftp }",
		"clamd without addr":   "scanner: { type: clamd }",
		"unknown scanner":      "scanner: { type: shell }",
		"inline pdf":           "download: { inline: true, inline_types: [pdf] }",
		"inline without types": "download: { inline: true }",
		"ttl order":            "share: { default_ttl_hours: 100, max_ttl_hours: 10 }",
		"base url":             "share: { base_url: 'javascript:alert(1)' }",
		"upload timeout":       "limits: { upload_timeout: 5d }",
	}
	for name, y := range bad {
		if _, err := ParseConfig([]byte(y)); err == nil {
			t.Errorf("%s: accepted %q", name, y)
		}
	}
	good := `
backend: { type: s3, s3: { endpoint: 'https://s3.example.com', bucket: my-bucket, region: eu-west-1, prefix: 'files/' } }
scanner: { type: clamd, address: 'clamav.svc:3310' }
download: { inline: true, inline_types: [png, jpg] }
limits: { allowed_types: [pdf, docx] }
`
	if _, err := ParseConfig([]byte(good)); err != nil {
		t.Errorf("a valid config: %v", err)
	}
	// development may use plain http to a local MinIO
	if _, err := ParseConfig([]byte("backend: { type: s3, s3: { endpoint: 'http://minio:9000', bucket: dev-bucket, allow_insecure_http: true } }")); err != nil {
		t.Errorf("dev s3: %v", err)
	}
}

func TestSecretsAreRequiredAndStrong(t *testing.T) {
	cfg := DefaultConfig()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := SecretsFromEnv(env(nil), cfg); err == nil {
		t.Error("no secrets accepted")
	}
	good := map[string]string{"CAN_FILES_API_TOKEN": "0123456789abcdefghijklmnopqrstuvwxyz", "CAN_FILES_LINK_SECRET": "zyxwvutsrqponmlkjihgfedcba9876543210"}
	if _, err := SecretsFromEnv(env(good), cfg); err != nil {
		t.Errorf("good secrets: %v", err)
	}
	weak := map[string]string{"CAN_FILES_API_TOKEN": strings.Repeat("a", 40), "CAN_FILES_LINK_SECRET": good["CAN_FILES_LINK_SECRET"]}
	if _, err := SecretsFromEnv(env(weak), cfg); err == nil {
		t.Error("a low-entropy token was accepted")
	}
	s3 := DefaultConfig()
	s3.Backend.Type = "s3"
	if _, err := SecretsFromEnv(env(good), s3); err == nil {
		t.Error("s3 without credentials was accepted")
	}
}

func TestMainCommandsAndWiring(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keyring"), []byte(testKeyring), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(dir, "files.yaml")
	y := "db_path: " + filepath.Join(dir, "f.db") + "\nkeyring_path: " + filepath.Join(dir, "keyring") + "\nstaging_dir: " + filepath.Join(dir, "stage") +
		"\nbackend: { local: { dir: " + filepath.Join(dir, "blobs") + " } }\n"
	if err := os.WriteFile(cfgFile, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(k string) string {
		return map[string]string{"CAN_FILES_API_TOKEN": "0123456789abcdefghijklmnopqrstuvwxyz", "CAN_FILES_LINK_SECRET": "zyxwvutsrqponmlkjihgfedcba9876543210", "CAN_FILES_CONFIG": cfgFile}[k]
	}
	run := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		c := Main(args, env, &o, &e)
		return c, o.String(), e.String()
	}
	if c, o, _ := run("version"); c != 0 || !strings.HasPrefix(o, "can-files ") {
		t.Errorf("version: %d %q", c, o)
	}
	if c, o, e := run("check-config"); c != 0 || !strings.Contains(o, "config ok") {
		t.Errorf("check-config: %d %q %q", c, o, e)
	}
	if c, _, _ := run("nonsense"); c != 2 {
		t.Errorf("unknown command: %d", c)
	}
	if c, _, e := run("verify-audit"); c != 0 {
		t.Errorf("verify-audit: %d %q", c, e)
	}
	if c, o, _ := run("rotate-keys"); c != 0 || !strings.Contains(o, "re-sealed 0") {
		t.Errorf("rotate-keys: %d %q", c, o)
	}
	if c, o, _ := run("sweep"); c != 0 || !strings.Contains(o, "purged 0") {
		t.Errorf("sweep: %d %q", c, o)
	}
	// a keyring readable by everyone is refused
	if err := os.Chmod(filepath.Join(dir, "keyring"), 0o666); err != nil {
		t.Fatal(err)
	}
	if c, _, e := run("verify-audit"); c != 2 || !strings.Contains(e, "keyring") {
		t.Errorf("a world-writable keyring: %d %q", c, e)
	}
	if c, _, _ := (func() (int, string, string) {
		var o, e bytes.Buffer
		return Main([]string{"serve"}, func(string) string { return "" }, &o, &e), "", ""
	})(); c != 2 {
		t.Errorf("serve without secrets: %d", c)
	}
}

func TestDownloadOfACorruptedBlobIsAbortedNotCompleted(t *testing.T) {
	a := newAPIEnv(t)
	sp := a.space(a.alice, "S")
	body := append(pdf(""), bytes.Repeat([]byte("0123456789"), 20000)...)
	f := a.upload(a.alice, sp.ID, "big.pdf", body)
	p := a.blobs()[0]
	raw, _ := os.ReadFile(p)
	raw[len(raw)-3] ^= 0xff
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", a.srv.URL+"/v1/files/"+f.ID+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+patAlice)
	resp, err := a.srv.Client().Do(req)
	if err != nil {
		return // the abort may also surface here
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err == nil || len(got) >= len(body) {
		t.Errorf("the client received a complete response (%d of %d bytes, err %v) for corrupted content", len(got), len(body), err)
	}
}
