package modmgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

const baseCompany = `# the company
schema: 1
name: Acme
domain: acme.example
provider:
  type: local
  options: {server_ram_mb: 2048}
modules:
  web:
    enabled: true
    settings: {site_title: Acme}
`

func TestApplyPatchKeepsCommentsAndOtherKeys(t *testing.T) {
	out, err := ApplyPatch([]byte(baseCompany), Patch{Ops: []Op{
		SetEnabled("crm", true),
		SetSettings("web", map[string]any{"site_title": "New", "max_rows": int64(5), "verbose": true}),
		SetString("name", "Acme Two"),
	}})
	must(t, err)
	s := string(out)
	for _, want := range []string{"# the company", "name: Acme Two", "domain: acme.example", "crm:", "max_rows: 5", "verbose: true", "server_ram_mb: 2048"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, `"5"`) {
		t.Errorf("an integer was written as a string:\n%s", s)
	}
}

func TestApplyPatchFailsClosedOnPathsOutsideTheAllowList(t *testing.T) {
	for _, op := range []Op{
		{Path: []string{"provider", "type"}, Value: "ssh"},
		{Path: []string{"modules", "../x", "enabled"}, Value: true},
		{Path: []string{"modules", "web", "image"}, Value: "evil"},
		{Path: []string{"approvals", "x"}, Value: "y"},
		{Path: nil, Value: "y"},
	} {
		if _, err := ApplyPatch([]byte(baseCompany), Patch{Ops: []Op{op}}); !errors.Is(err, ErrPatchPath) {
			t.Errorf("%v: err = %v", op.Path, err)
		}
	}
}

func TestApplyPatchJSONRoundTripKeepsNumbers(t *testing.T) {
	p := Patch{Ops: []Op{SetSettings("web", map[string]any{"max_rows": int64(5), "ratio": 1.5, "on": true})}}
	b, err := p.Marshal()
	must(t, err)
	q, err := ParsePatch(b)
	must(t, err)
	out, err := ApplyPatch([]byte(baseCompany), q)
	must(t, err)
	if !strings.Contains(string(out), "max_rows: 5\n") || strings.Contains(string(out), `"5"`) {
		t.Fatalf("%s", out)
	}
}

func TestPatchIsBounded(t *testing.T) {
	big := map[string]any{}
	for i := 0; i < 5000; i++ {
		big[strings.Repeat("k", 10)+string(rune('a'+i%26))+strings.Repeat("z", i%50)] = strings.Repeat("v", 100)
	}
	b, _ := Patch{Ops: []Op{SetSettings("web", big)}}.Marshal()
	if _, err := ParsePatch(append(b, strings.Repeat(" ", MaxPatchBytes)...)); err == nil {
		t.Fatal("oversized patch accepted")
	}
}

func TestDiffShowsChangedLinesOnly(t *testing.T) {
	d := Diff("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n", "a\nb\nc\nd\nE\nf\ng\nh\ni\nj\n")
	var plus, minus int
	for _, l := range d {
		switch l.Op {
		case '+':
			plus++
		case '-':
			minus++
		}
	}
	if plus != 1 || minus != 1 {
		t.Fatalf("%+v", d)
	}
	if len(d) > 9 {
		t.Fatalf("unchanged runs must be collapsed: %d lines", len(d))
	}
	if !DiffTooLarge(strings.Repeat("x\n", MaxDiffLines+1), "") {
		t.Fatal("oversized diff not flagged")
	}
	if len(Diff("same\n", "same\n")) != 0 {
		t.Fatal("identical documents have no diff")
	}
}

func TestFileStoreOptimisticConcurrency(t *testing.T) {
	p := filepath.Join(t.TempDir(), "company.yaml")
	must(t, os.WriteFile(p, []byte(baseCompany), 0o644))
	fs := &FileStore{Path: p}
	ctx := context.Background()
	s1, err := fs.Get(ctx)
	must(t, err)
	v2, err := fs.Put(ctx, []byte(baseCompany+"# one\n"), s1.Version)
	must(t, err)
	if v2 == s1.Version {
		t.Fatal("version did not change")
	}
	if _, err := fs.Put(ctx, []byte(baseCompany+"# two\n"), s1.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write accepted: %v", err)
	}
	got, _ := fs.Get(ctx)
	if !strings.HasSuffix(string(got.Data), "# one\n") {
		t.Fatal("stale write overwrote")
	}
	if _, err := fs.Put(ctx, make([]byte, MaxCompanyBytes+1), got.Version); err == nil {
		t.Fatal("oversized document accepted")
	}
}

func TestFileStoreConcurrentWritersOnlyOneWins(t *testing.T) {
	p := filepath.Join(t.TempDir(), "company.yaml")
	must(t, os.WriteFile(p, []byte(baseCompany), 0o644))
	fs := &FileStore{Path: p}
	s, _ := fs.Get(context.Background())
	var wg sync.WaitGroup
	var ok, conflict int
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fs.Put(context.Background(), []byte(baseCompany+"# w\n"+strings.Repeat("#", i)+"\n"), s.Version)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if errors.Is(err, ErrConflict) {
				conflict++
			}
		}()
	}
	wg.Wait()
	if ok != 1 || conflict != 7 {
		t.Fatalf("ok=%d conflict=%d", ok, conflict)
	}
}

func TestKubeStoreRoundTripAndConflict(t *testing.T) {
	api := kube.NewFake()
	ctx := context.Background()
	_, err := api.Create(ctx, kube.Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "company", "namespace": "can-system"}, "data": map[string]any{"company.yaml": baseCompany}})
	must(t, err)
	ks := &KubeStore{API: api}
	s, err := ks.Get(ctx)
	must(t, err)
	if string(s.Data) != baseCompany || s.Version == "" {
		t.Fatalf("%+v", s)
	}
	v, err := ks.Put(ctx, []byte(baseCompany+"# x\n"), s.Version)
	must(t, err)
	if v == s.Version {
		t.Fatal("version unchanged")
	}
	if _, err := ks.Put(ctx, []byte("# y\n"), s.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write: %v", err)
	}
	if _, err := (&KubeStore{API: kube.NewFake()}).Get(ctx); !errors.Is(err, ErrNoCompany) {
		t.Fatalf("missing ConfigMap: %v", err)
	}
}
