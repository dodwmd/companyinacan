package diagnose

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
)

// Secrets and personal data seeded into every input. None of them may appear in a bundle.
// Built from pieces so the repository secret scanner does not flag these deliberately fake values.
const (
	awsKey    = "AKIA" + "IOSFODNN7EXAMPLE"
	ghToken   = "ghp_" + "16C7e42F292c6912E7710c838347Ae178B4a"
	stripeKey = "sk_live_" + "51H8exampleexampleexample"
	pemBegin  = "-----BEGIN " + "PRIVATE KEY-----"
	pemEnd    = "-----END " + "PRIVATE KEY-----"
)

var seeds = []string{
	"hunter2-PASSWORD-seed",
	awsKey,
	ghToken,
	"s3cr3tBearerValue123",
	"alice.smith@example.com",
	"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
	"MIIEvQIBADANBgkqhkiG9w0BAQEFAASC-private-key-body",
	stripeKey,
}

func readBundle(t *testing.T, b *Bundle) map[string]string {
	t.Helper()
	var buf bytes.Buffer
	if err := b.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(&buf)
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
		if h.Mode != 0o600 {
			t.Errorf("%s mode %o, want 0600", h.Name, h.Mode)
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
}

func TestSeededSecretsNeverAppear(t *testing.T) {
	b := New()
	b.AddRedactor(func(s string) string { return strings.ReplaceAll(s, "hunter2-PASSWORD-seed", "[redacted]") })
	company := "name: Acme\nowner: alice.smith@example.com\nsmtp:\n  password: hunter2-PASSWORD-seed\n  api_key: " + awsKey + "\nclient_secret: s3cr3tBearerValue123\n"
	log := strings.Join([]string{
		`{"level":"INFO","msg":"ok"}`,
		"Authorization: Bearer s3cr3tBearerValue123",
		"token " + ghToken + " leaked by alice.smith@example.com",
		"jwt eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"dial postgres://app:hunter2-PASSWORD-seed@db:5432/x",
		pemBegin,
		"MIIEvQIBADANBgkqhkiG9w0BAQEFAASC-private-key-body",
		pemEnd,
		"stripe " + stripeKey,
	}, "\n")
	for name, text := range map[string]string{"config.yaml": company, "logs.txt": log, "metrics.txt": "can_x{note=\"alice.smith@example.com\"} 1\n"} {
		if err := b.AddText(name, text); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.AddJSON("modules.json", map[string]any{"last_error": "dial password=hunter2-PASSWORD-seed refused for alice.smith@example.com"}); err != nil {
		t.Fatal(err)
	}
	files := readBundle(t, b)
	for name, body := range files {
		for _, seed := range seeds {
			if strings.Contains(body, seed) {
				t.Errorf("%s contains seeded secret %q:\n%s", name, seed, body)
			}
		}
	}
	if !strings.Contains(files["config.yaml"], "name: Acme") {
		t.Errorf("ordinary configuration was lost: %q", files["config.yaml"])
	}
	if !strings.Contains(files["logs.txt"], `"msg":"ok"`) {
		t.Errorf("ordinary log line was lost: %q", files["logs.txt"])
	}
}

func TestAllowlistRejectsOtherNames(t *testing.T) {
	b := New()
	for _, name := range []string{"../etc/passwd", "/abs", "state.db", "secrets.txt", "logs.txt/../x", "config.yaml.bak", ""} {
		if err := b.AddText(name, "x"); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
	if err := b.AddText("logs.txt", "x"); err != nil {
		t.Fatal(err)
	}
	if err := b.AddText("logs.txt", "again"); err == nil {
		t.Error("a name may be added once")
	}
}

func TestSizeCaps(t *testing.T) {
	b := New()
	b.MaxFile, b.MaxTotal = 1000, 2500
	big := strings.Repeat("line of log text\n", 1000)
	if err := b.AddText("logs.txt", big); err != nil {
		t.Fatal(err)
	}
	files := readBundle(t, b)
	if len(files["logs.txt"]) > 1100 || !strings.Contains(files["logs.txt"], "truncated") {
		t.Fatalf("file not capped: %d bytes", len(files["logs.txt"]))
	}
	b2 := New()
	b2.MaxFile, b2.MaxTotal = 1000, 1500
	_ = b2.AddText("logs.txt", strings.Repeat("a ", 400))
	_ = b2.AddText("metrics.txt", strings.Repeat("b ", 400))
	if err := b2.AddText("config.yaml", strings.Repeat("c ", 400)); err == nil {
		t.Fatal("total cap not enforced")
	}
}

func TestManifestListsFiles(t *testing.T) {
	b := New()
	_ = b.AddText("versions.json", `{"can":"v1"}`)
	files := readBundle(t, b)
	m := files["manifest.json"]
	if !strings.Contains(m, `"versions.json"`) || !strings.Contains(m, "generated_at") {
		t.Fatalf("manifest: %s", m)
	}
}

func TestTailLines(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString("line ")
		sb.WriteString(strings.Repeat("x", i%7))
		sb.WriteString("\n")
	}
	got := TailLines(strings.NewReader(sb.String()), 50, 20)
	if n := strings.Count(got, "\n"); n != 50 {
		t.Fatalf("lines = %d, want 50", n)
	}
	long := TailLines(strings.NewReader(strings.Repeat("y", 5000)+"\n"), 10, 100)
	if len(long) > 130 {
		t.Fatalf("long line not cut: %d", len(long))
	}
}

func TestRedactConfigByKey(t *testing.T) {
	out := RedactConfig("a: 1\nsmtp_password: hunter2\nnested:\n  Client_Secret: topsecretvalue\n  host: mail.example.com\n  token_ref: my-secret-name\n")
	if strings.Contains(out, "hunter2") || strings.Contains(out, "topsecretvalue") {
		t.Fatalf("secret values kept: %s", out)
	}
	if !strings.Contains(out, "host: mail.example.com") || !strings.Contains(out, "a: 1") {
		t.Fatalf("ordinary keys altered: %s", out)
	}
}

func TestScrubStripsTerminalEscapes(t *testing.T) {
	got := Scrub("\x1b[0m\x1b[1;31m[core]\x1b[0m started \x1b]0;title\x07ok")
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) || !strings.Contains(got, "[core] started") {
		t.Fatalf("%q", got)
	}
}

func TestRedactConfigHardening(t *testing.T) {
	in := `name: Acme
domain: acme.example
smtp:
  host: mail.example.com
  pass: p1-seed
  passwd: p2-seed
  db_dsn: postgres://u:p3-seed@db/x
  signing: s4-seed
  webhook: https://hooks.example.com/T000/B000/s5-seed
  endpoint_url: https://u:p6-seed@api.example.com
  access_key: k7-seed
contact:
  name: Alice Smith
  phone: "+1 555 0100"
  mobile: 555-0101
owner_name: Bob Jones
notes: |
  call Carol Diaz on 555 0102
  password is p8-seed
  more text
description: >-
  folded free text with a name Dave
people:
  - first_name: Erin
    last_name: Frost
after: 1
`
	out := RedactConfig(in)
	for _, seed := range []string{"p1-seed", "p2-seed", "p3-seed", "s4-seed", "s5-seed", "p6-seed", "k7-seed", "Alice", "555 0100", "555-0101", "Bob", "Carol", "p8-seed", "more text", "Dave", "Erin", "Frost"} {
		if strings.Contains(out, seed) {
			t.Errorf("%q survived:\n%s", seed, out)
		}
	}
	for _, keep := range []string{"name: Acme", "domain: acme.example", "host: mail.example.com", "after: 1"} {
		if !strings.Contains(out, keep) {
			t.Errorf("%q was lost:\n%s", keep, out)
		}
	}
}

func TestValidComposeProject(t *testing.T) {
	for _, ok := range []string{"companyinacan", "a", "my-proj_2"} {
		if !ValidComposeProject(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "-x", "A", "a b", "a;rm", "../x", "a/b", strings.Repeat("a", 65), "--file"} {
		if ValidComposeProject(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
