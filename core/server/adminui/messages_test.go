package adminui

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCatalogueMatchesUsage(t *testing.T) {
	used := map[string]bool{}
	tplKey := regexp.MustCompile(`\(?t "([a-z0-9_.]+)"`)
	err := fs.WalkDir(assets, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(assets, p)
		for _, m := range tplKey.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = true
			if _, ok := messagesEN[m[1]]; !ok {
				t.Errorf("%s uses unknown message key %q", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// keys referenced from Go code: msg("key") / flash keys / errors
	prefixes := map[string]bool{}
	for k := range messagesEN {
		prefixes[k[:strings.Index(k, ".")]] = true
	}
	goKey := regexp.MustCompile(`"([a-z_0-9]+)\.([a-z0-9_]+)"`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "messages.go" {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range goKey.FindAllStringSubmatch(string(b), -1) {
			if !prefixes[m[1]] || m[2] == "html" || m[2] == "css" {
				continue // not a message key (event names, file names, ...)
			}
			k := m[1] + "." + m[2]
			used[k] = true
			if _, ok := messagesEN[k]; !ok {
				t.Errorf("%s uses unknown message key %q", f, k)
			}
		}
	}
	for k := range messagesEN {
		if !used[k] {
			t.Errorf("message %q is never used", k)
		}
	}
}

func TestEmbeddedAssetsStaySmall(t *testing.T) {
	var total, css int64
	_ = fs.WalkDir(assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, _ := d.Info()
		total += fi.Size()
		if strings.HasSuffix(p, ".css") {
			css += fi.Size()
		}
		if strings.HasSuffix(p, ".js") {
			t.Errorf("%s: the admin UI ships no JavaScript", p)
		}
		return nil
	})
	if total > 100<<10 || css > 12<<10 {
		t.Fatalf("assets %d bytes, css %d bytes (budget 100 KiB / 12 KiB)", total, css)
	}
}
