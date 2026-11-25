package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestLoadReleaseAcceptsDigestPinnedRefs(t *testing.T) {
	p := writeFile(t, t.TempDir(), "release-images.json", `{"schema":1,"version":"v0.1.0","images":{
	  "can-core":{"ref":"ghcr.io/dodwmd/can-core@`+goodDigest+`"},
	  "can-static":{"ref":"ghcr.io/dodwmd/can-static@`+goodDigest+`"},
	  "can-forms":{"ref":"ghcr.io/dodwmd/can-forms@`+goodDigest+`"}}}`)
	set, err := LoadRelease(p)
	if err != nil {
		t.Fatal(err)
	}
	if set.Source != SourceRelease || set.Version != "v0.1.0" || !strings.HasSuffix(set.Ref(ImageCore), goodDigest) {
		t.Fatalf("%+v", set)
	}
	if len(set.Upload()) != 0 {
		t.Fatal("a release manifest pulls from the registry: nothing to upload")
	}
}

func TestLoadReleaseRefusesUnpinnedOrPlaceholders(t *testing.T) {
	for name, ref := range map[string]string{
		"tag only":    "ghcr.io/dodwmd/can-core:0.1.0",
		"placeholder": "ghcr.io/dodwmd/can-core@sha256:REPLACE_WITH_DIGEST",
		"short":       "ghcr.io/dodwmd/can-core@sha256:abcd",
		"shell":       "ghcr.io/x/can-core@" + goodDigest + ";rm -rf /",
	} {
		body := `{"schema":1,"images":{"can-core":{"ref":"` + ref + `"},"can-static":{"ref":"ghcr.io/x/s@` + goodDigest + `"},"can-forms":{"ref":"ghcr.io/x/f@` + goodDigest + `"}}}`
		p := writeFile(t, t.TempDir(), "r.json", body)
		if _, err := LoadRelease(p); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestLoadReleaseRequiresAllImages(t *testing.T) {
	p := writeFile(t, t.TempDir(), "r.json", `{"schema":1,"images":{"can-core":{"ref":"ghcr.io/x/c@`+goodDigest+`"}}}`)
	_, err := LoadRelease(p)
	if err == nil || !strings.Contains(err.Error(), "can-static") {
		t.Fatalf("want a missing can-static error, got %v", err)
	}
}

func imagesDir(t *testing.T, tamper bool) string {
	t.Helper()
	dir := t.TempDir()
	idx := `{"schema":1,"images":{`
	for i, n := range []string{ImageCore, ImageStatic, ImageForms} {
		body := "tarball-" + n
		writeFile(t, dir, n+".tar", body)
		s := sum(body)
		if tamper && n == ImageStatic {
			s = sum("something else")
		}
		if i > 0 {
			idx += ","
		}
		idx += `"` + n + `":{"ref":"` + n + `:dev","file":"` + n + `.tar","sha256":"` + s + `"}`
	}
	writeFile(t, dir, "images.json", idx+"}}")
	return dir
}

func TestLoadDirVerifiesTarballs(t *testing.T) {
	set, err := LoadDir(imagesDir(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if set.Source != SourceDir || set.Ref(ImageCore) != "can-core:dev" {
		t.Fatalf("%+v", set)
	}
	up := set.Upload()
	if len(up) != 3 {
		t.Fatalf("upload set %v", up)
	}
	if set.Fingerprint() == "" {
		t.Fatal("empty fingerprint")
	}
}

func TestLoadDirRefusesChecksumMismatch(t *testing.T) {
	_, err := LoadDir(imagesDir(t, true))
	if err == nil || !strings.Contains(err.Error(), "sha256") || !strings.Contains(err.Error(), ImageStatic) {
		t.Fatalf("want a sha256 mismatch for can-static, got %v", err)
	}
}

func TestLoadDirRefusesPathTraversalAndMissingIndex(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "make images") {
		t.Fatalf("a missing index must point at `make images`, got %v", err)
	}
	writeFile(t, dir, "images.json", `{"schema":1,"images":{"can-core":{"ref":"can-core:dev","file":"../evil.tar","sha256":"`+sum("x")+`"},"can-static":{"ref":"a:b","file":"a.tar","sha256":"`+sum("x")+`"},"can-forms":{"ref":"a:c","file":"b.tar","sha256":"`+sum("x")+`"}}}`)
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "file") {
		t.Fatalf("want a file name error, got %v", err)
	}
}

func TestResolveOrdersSourcesAndFailsClosed(t *testing.T) {
	if _, err := ResolveImages(ImageOptions{}); err == nil || !strings.Contains(err.Error(), "make images") || !strings.Contains(err.Error(), "--images-from") {
		t.Fatalf("no source must explain both ways out, got %v", err)
	}
	dir := imagesDir(t, false)
	set, err := ResolveImages(ImageOptions{FromDir: dir})
	if err != nil || set.Source != SourceDir {
		t.Fatalf("%v %+v", err, set)
	}
	rel := writeFile(t, t.TempDir(), "r.json", `{"schema":1,"images":{"can-core":{"ref":"a/c@`+goodDigest+`"},"can-static":{"ref":"a/s@`+goodDigest+`"},"can-forms":{"ref":"a/f@`+goodDigest+`"}}}`)
	if _, err := ResolveImages(ImageOptions{FromDir: dir, ReleaseManifest: rel}); err == nil {
		t.Fatal("both sources at once is ambiguous and must be refused")
	}
	if set, err = ResolveImages(ImageOptions{ReleaseManifest: rel}); err != nil || set.Source != SourceRelease {
		t.Fatalf("%v %+v", err, set)
	}
}
