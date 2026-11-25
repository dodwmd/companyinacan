package licence

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func entries() []Entry {
	acc := DefaultAccepted()
	var es []Entry
	for _, m := range []*config.Module{
		mod("zeta", "oss-wrapper", "MIT", "", ""),
		mod("alpha", "oss-wrapper", "AGPL-3.0-only", "", ""),
		mod("mid", "oss-wrapper", "Elastic-2.0", "", "source-available"),
	} {
		es = append(es, Entry{Module: m, Result: Evaluate(m, acc, "x")})
	}
	return es
}

func TestNoticeGolden(t *testing.T) {
	got := Notice(entries())
	if again := Notice(entries()); !bytes.Equal(got, again) {
		t.Fatal("notice is not deterministic")
	}
	golden := filepath.Join("..", "..", "testdata", "licence", "notice.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("missing golden file (run with UPDATE_GOLDEN=1): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("notice differs from golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if bytes.Contains(got, []byte("\r")) {
		t.Fatal("notice must use LF line endings")
	}
}

func TestNoticeEmpty(t *testing.T) {
	got := Notice(nil)
	if !bytes.Contains(got, []byte("None")) {
		t.Fatalf("empty notice should say none: %s", got)
	}
}

func TestCollectSkipsExamplesAndInhouse(t *testing.T) {
	dir := t.TempDir()
	write := func(sub, body string) {
		p := filepath.Join(dir, sub)
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "module.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("_example", "schema: 1\nid: example\nprovider: {type: oss-wrapper, name: X, licence: MIT, upstream: 'https://x.example'}\n")
	write("people", "schema: 1\nid: people\nprovider: {type: inhouse}\n")
	write("wrapped", "schema: 1\nid: wrapped\nprovider: {type: oss-wrapper, name: W, licence: MIT, upstream: 'https://w.example'}\n")
	es, ps := Collect(dir, DefaultAccepted())
	if len(ps) != 0 {
		t.Fatalf("problems: %v", ps)
	}
	if len(es) != 1 || es[0].Module.ID != "wrapped" {
		t.Fatalf("want only the wrapped module, got %+v", es)
	}
}

func TestCheckDetectsStaleNotice(t *testing.T) {
	file := filepath.Join(t.TempDir(), "THIRD_PARTY_LICENSES")
	if err := os.WriteFile(file, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := CheckNotice(file, Notice(entries()))
	if len(ps) != 1 || ps[0].Rule != "licence.notice-stale" {
		t.Fatalf("got %v", ps)
	}
	if err := os.WriteFile(file, Notice(entries()), 0o600); err != nil {
		t.Fatal(err)
	}
	if ps := CheckNotice(file, Notice(entries())); len(ps) != 0 {
		t.Fatalf("up-to-date notice flagged: %v", ps)
	}
}
