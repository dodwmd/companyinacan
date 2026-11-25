package contract

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// wikiArchive builds a small storage.tar.gz.
func wikiArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"data/pages/start.txt": "restored page", ".htaccess-like": "dotfile"} {
		if strings.Contains(name, "/") {
			_ = tw.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755})
			_ = tw.WriteHeader(&tar.Header{Name: "data/pages/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// runWikiRestore runs modules/wiki/hooks/restore.sh against a fake curl (serves the archive), a
// fake mountpoint and, when failTar is set, a tar whose extraction fails half way.
func runWikiRestore(t *testing.T, failTar bool) (data string, out string, err error) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	data = filepath.Join(dir, "storage")
	for _, d := range []string{bin, data} {
		if e := os.MkdirAll(d, 0o755); e != nil {
			t.Fatal(e)
		}
	}
	arc := filepath.Join(dir, "served.tar.gz")
	if e := os.WriteFile(arc, wikiArchive(t), 0o600); e != nil {
		t.Fatal(e)
	}
	write := func(name, body string) {
		if e := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); e != nil {
			t.Fatal(e)
		}
	}
	write("curl", `while [ $# -gt 0 ]; do [ "$1" = "-o" ] && out="$2"; shift; done
cp "`+arc+`" "$out"
`)
	write("mountpoint", "exit 0\n")
	if failTar {
		realTar, e := exec.LookPath("tar")
		if e != nil {
			t.Skip("no tar")
		}
		// listing works; extraction writes one file then dies, like a full disk
		write("tar", `case "$*" in
*-xzf*) while [ $# -gt 0 ]; do [ "$1" = "-C" ] && d="$2"; shift; done
  echo partial > "$d/partial.txt"; echo "tar: write error" >&2; exit 2 ;;
esac
exec `+realTar+` "$@"
`)
	}
	for name, body := range map[string]string{"old-page.txt": "OLD", ".old-hidden": "OLDHIDDEN"} {
		if e := os.WriteFile(filepath.Join(data, name), []byte(body), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	tok := filepath.Join(dir, "token")
	_ = os.WriteFile(tok, []byte("t0k"), 0o600)
	cmd := exec.Command("sh", filepath.Join(root, "modules/wiki/hooks/restore.sh"))
	cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "CAN_API_URL=http://can-core", "CAN_MODULE=wiki", "WIKI_DATA=" + data, "CAN_TOKEN_FILE=" + tok, "TMPDIR=" + dir}
	b, err := cmd.CombinedOutput()
	return data, string(b), err
}

func names(t *testing.T, dir string) string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, e := range es {
		n = append(n, e.Name())
	}
	return strings.Join(n, ",")
}

// A failed extraction must leave the live volume exactly as it was.
func TestWikiRestoreFailedExtractionKeepsTheVolume(t *testing.T) {
	data, out, err := runWikiRestore(t, true)
	if err == nil {
		t.Fatalf("restore reported success after a failed extraction: %s", out)
	}
	if got := names(t, data); got != ".old-hidden,old-page.txt" {
		t.Fatalf("volume changed after a failed extraction: %s\n%s", got, out)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "old-page.txt")); string(b) != "OLD" {
		t.Error("existing file content changed")
	}
}

// A good archive replaces everything, dotfiles included, and leaves no staging directories.
func TestWikiRestoreSwapsInTheArchive(t *testing.T) {
	data, out, err := runWikiRestore(t, false)
	if err != nil {
		t.Fatalf("restore failed: %v\n%s", err, out)
	}
	if got := names(t, data); got != ".htaccess-like,data" {
		t.Fatalf("volume after restore: %s\n%s", got, out)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "data/pages/start.txt")); string(b) != "restored page" {
		t.Error("restored content missing")
	}
}
