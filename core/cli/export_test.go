package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/exportfmt"
)

func exportArchive(t *testing.T, complete bool) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	a := exportfmt.NewArchive(zw, time.Now())
	if err := a.Bytes("company/company.yaml", []byte("name: Acme\n")); err != nil {
		t.Fatal(err)
	}
	if complete {
		if err := a.JSON("manifest.json", map[string]any{"complete": true, "warnings": []string{"module wiki is NOT in this export"},
			"modules": []map[string]string{{"id": "forms", "status": "exported"}}}); err != nil {
			t.Fatal(err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fakeExport(t *testing.T, body []byte, fail error, remote string) *[]string {
	t.Helper()
	var asked []string
	prev := exportKubectl
	t.Cleanup(func() { exportKubectl = prev })
	exportKubectl = func(_ context.Context, stdout, stderr io.Writer, args ...string) error {
		asked = args
		_, _ = io.WriteString(stderr, remote)
		if _, err := stdout.Write(body); err != nil {
			return err
		}
		return fail
	}
	return &asked
}

func TestExportWritesACompleteArchive(t *testing.T) {
	dir := t.TempDir()
	body := exportArchive(t, true)
	asked := fakeExport(t, body, nil, "")
	out := filepath.Join(dir, "x.tar.gz")
	code, stdout, errs := run(t, "export", "--context", "k3d-x", "--out", out, "--skip", "wiki")
	if code != ExitOK || !strings.Contains(stdout, "not encrypted") || !strings.Contains(errs, "WARNING: module wiki is NOT in this export") {
		t.Fatalf("%d %s %s", code, stdout, errs)
	}
	if got := strings.Join(*asked, " "); got != "--context k3d-x -n can-system exec deploy/can-core -c can-core -- /can-core export --skip wiki" {
		t.Fatal(got)
	}
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal(err, "differs")
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
	// An existing file is never overwritten.
	if code, _, errs = run(t, "export", "--context", "k3d-x", "--out", out); code != ExitFailure || !strings.Contains(errs, "not overwritten") {
		t.Fatalf("%d %s", code, errs)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*")); len(m) != 1 {
		t.Fatal(m)
	}
}

func TestExportKeepsNothingIncomplete(t *testing.T) {
	complete := exportArchive(t, true)
	for name, c := range map[string]struct {
		body []byte
		err  error
	}{
		"no manifest":    {exportArchive(t, false), nil},
		"truncated":      {complete[:len(complete)-9], nil},
		"command failed": {complete, errors.New("exit status 1")},
		"nothing":        {nil, errors.New("exit status 1")},
		"not an archive": {[]byte("Error from server (NotFound)\n"), nil},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fakeExport(t, c.body, c.err, "error: can-core says no")
			for _, extra := range [][]string{nil, {"--passphrase-file", passFile(t, "a long enough passphrase")}} {
				args := append([]string{"export", "--context", "k3d-x", "--out", filepath.Join(dir, "x")}, extra...)
				code, _, errs := run(t, args...)
				if code != ExitFailure || !strings.Contains(errs, "can-core says no") {
					t.Fatalf("%d %s", code, errs)
				}
				if m, _ := filepath.Glob(filepath.Join(dir, "*")); len(m) != 0 {
					t.Fatalf("left behind: %v", m)
				}
			}
		})
	}
}

func passFile(t *testing.T, pass string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(p, []byte(pass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExportEncryptsAndDecrypts(t *testing.T) {
	dir := t.TempDir()
	body := exportArchive(t, true)
	asked := fakeExport(t, body, nil, "")
	pass := passFile(t, "a long enough passphrase")
	out := filepath.Join(dir, "x.tar.gz.canenc")
	code, stdout, errs := run(t, "export", "--context", "k3d-x", "--out", out, "--include-sealed", "--passphrase-file", pass)
	if code != ExitOK || !strings.Contains(stdout, "encrypted") || !strings.Contains(errs, "--include-sealed decrypts") || strings.Contains(errs, "NOT encrypted") {
		t.Fatalf("%d %s %s", code, stdout, errs)
	}
	if !strings.Contains(strings.Join(*asked, " "), "--include-sealed") {
		t.Fatal(*asked)
	}
	sealed, _ := os.ReadFile(out)
	if bytes.Contains(sealed, body[:40]) || !bytes.HasPrefix(sealed, []byte("CANEXPORT1\n")) {
		t.Fatal("not encrypted")
	}
	if code, _, errs = run(t, "export", "decrypt", out, "--passphrase-file", passFile(t, "the wrong passphrase")); code != ExitFailure {
		t.Fatalf("%d %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.tar.gz")); err == nil {
		t.Fatal("a failed decrypt left a file")
	}
	if code, _, errs = run(t, "export", "decrypt", out, "--passphrase-file", pass); code != ExitOK {
		t.Fatalf("%d %s", code, errs)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "x.tar.gz")); !bytes.Equal(got, body) {
		t.Fatal("differs")
	}

	// Without a passphrase the sealed fields are written in clear, and said so.
	code, _, errs = run(t, "export", "--context", "k3d-x", "--out", filepath.Join(dir, "plain"), "--include-sealed")
	if code != ExitOK || !strings.Contains(errs, "NOT encrypted") {
		t.Fatalf("%d %s", code, errs)
	}
}

func TestExportUsage(t *testing.T) {
	fakeExport(t, nil, errors.New("must not run"), "")
	for _, args := range [][]string{
		{"export", "--context", "x", "--module", "forms; rm"},
		{"export", "--context", "x", "--skip", "-x"},
		{"export", "--context", "x", "--passphrase-file", passFile(t, "short")},
	} {
		if code, _, errs := run(t, args...); code != ExitUsage {
			t.Errorf("%v: %d %s", args, code, errs)
		}
	}
}
