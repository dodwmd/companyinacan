package provider

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRedactKnownSecretsAndPatterns(t *testing.T) {
	r := NewRedactor()
	r.Add("s3cr3t-token-value")
	r.Add("ab") // too short: ignored
	cases := map[string]string{
		"use s3cr3t-token-value now":                 "use " + Redacted + " now",
		"Authorization: Bearer abcdefghijklmnop1234": "Authorization: Bearer " + Redacted,
		"token=hunter2hunter2 next":                  "token=" + Redacted + " next",
		`{"password": "p4ssw0rd!!"}`:                 `{"password": "` + Redacted + `"}`,
		"-----BEGIN OPENSSH PRIVATE KEY-----\nAAA\nBBB\n-----END OPENSSH PRIVATE KEY-----": "-----BEGIN OPENSSH PRIVATE KEY----- " + Redacted + " -----END OPENSSH PRIVATE KEY-----", // scan-secrets:allow fake key text proving the redactor works
		"nothing secret here, ab": "nothing secret here, ab",
	}
	for in, want := range cases {
		if got := r.Redact(in); got != want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestRedactErrKeepsChain(t *testing.T) {
	r := NewRedactor()
	r.Add("topsecretvalue")
	base := errors.New("boom topsecretvalue")
	err := r.Err(base)
	if strings.Contains(err.Error(), "topsecretvalue") {
		t.Fatal("secret leaked in error")
	}
	if !errors.Is(err, base) {
		t.Fatal("error chain lost")
	}
	if r.Err(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestRedactWriterCatchesSecretSplitAcrossWrites(t *testing.T) {
	r := NewRedactor()
	r.Add("splitsecret123")
	var buf bytes.Buffer
	w := r.Writer(&buf)
	_, _ = w.Write([]byte("value is split"))
	_, _ = w.Write([]byte("secret123 end\nnext line"))
	if err := w.(Flusher).Flush(); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, "splitsecret123") || strings.Contains(got, "split") && strings.Contains(got, "secret123") {
		t.Fatalf("leak: %q", got)
	}
	if !strings.Contains(got, "next line") {
		t.Fatalf("flush lost data: %q", got)
	}
}

func TestResolveSecretRegistersWithRedactor(t *testing.T) {
	d := Deps{
		Getenv:   func(k string) string { return map[string]string{"HCLOUD_TOKEN": "tok-from-env-1234"}[k] },
		ReadFile: func(string) ([]byte, error) { return []byte("tok-from-file-5678\n"), nil },
	}
	d.defaults()
	for ref, want := range map[string]string{"env:HCLOUD_TOKEN": "tok-from-env-1234", "file:/x/y": "tok-from-file-5678"} {
		got, err := d.ResolveSecret(ref)
		if err != nil || got != want {
			t.Fatalf("%s: %q %v", ref, got, err)
		}
		if strings.Contains(d.Redactor.Redact("x "+want), want) {
			t.Fatalf("%s not registered with redactor", ref)
		}
	}
	for _, bad := range []string{"env:MISSING", "plain"} {
		_, err := d.ResolveSecret(bad)
		if err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}
