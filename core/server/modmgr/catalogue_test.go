package modmgr

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func TestLoadCatalogueListsEveryManifestAndSkipsExamples(t *testing.T) {
	cat, err := LoadCatalogue(writeModules(t))
	must(t, err)
	var ids []string
	for _, e := range cat.Entries {
		ids = append(ids, e.ID)
	}
	if got := strings.Join(ids, ","); got != "alt,crm,desk,evil,plan,solo,web" {
		t.Fatalf("ids = %s", got)
	}
	web := cat.Get("web")
	if web == nil || web.Schema == nil || web.Status() != "ready" || !web.Deployable() {
		t.Fatalf("web = %+v", web)
	}
	if e := cat.Get("crm"); e.Status() != "experimental" || !e.SourceAvailable() {
		t.Fatalf("crm = %+v", e)
	}
	if e := cat.Get("desk"); e.Deployable() {
		t.Fatal("not-deployable entry reported deployable")
	}
	if cat.Get("example") != nil || cat.Get("_example") != nil {
		t.Fatal("example directory listed")
	}
}

func TestLoadCatalogueIsBounded(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "big"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "big", "module.yaml"), []byte(strings.Repeat("# x\n", MaxManifestBytes/4+10)), 0o644))
	cat, err := LoadCatalogue(dir)
	must(t, err)
	if len(cat.Entries) != 1 || cat.Entries[0].Invalid == "" {
		t.Fatalf("oversized manifest must be listed as invalid, got %+v", cat.Entries)
	}
	for i := 0; i < MaxModules+5; i++ {
		d := filepath.Join(dir, "m"+strconv.Itoa(1000+i))
		must(t, os.MkdirAll(d, 0o755))
		must(t, os.WriteFile(filepath.Join(d, "module.yaml"), []byte("schema: 1\n"), 0o644))
	}
	cat, err = LoadCatalogue(dir)
	must(t, err)
	if len(cat.Entries) > MaxModules || !cat.Truncated {
		t.Fatalf("entries=%d truncated=%v", len(cat.Entries), cat.Truncated)
	}
}

func TestLoadCatalogueDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "module.yaml"), []byte(manifest("link", "Link", "")), 0o644))
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	cat, err := LoadCatalogue(dir)
	must(t, err)
	if len(cat.Entries) != 0 {
		t.Fatalf("symlinked module listed: %+v", cat.Entries)
	}
}

func TestFootprintVerdicts(t *testing.T) {
	cat, _ := LoadCatalogue(writeModules(t))
	mods := func(ids ...string) []*config.Module {
		var out []*config.Module
		for _, id := range ids {
			out = append(out, cat.Get(id).Manifest)
		}
		return out
	}
	f := Footprint(mods("web"), 2048)
	if f.PlatformMB != PlatformBaselineMB || f.RequestMB != 20 || f.LimitMB != 40 || f.DatabaseMB != 0 || f.TotalMB != PlatformBaselineMB+20 {
		t.Fatalf("%+v", f)
	}
	if f.Verdict != "fits" {
		t.Fatalf("verdict %s", f.Verdict)
	}
	f = Footprint(mods("web", "crm"), 2048) // postgres adds the database engine
	if f.DatabaseMB != DatabaseEngineMB || f.TotalMB != PlatformBaselineMB+20+300+DatabaseEngineMB {
		t.Fatalf("%+v", f)
	}
	if got := Footprint(mods("web", "crm"), 1024); got.Verdict != "over" || got.OverByMB <= 0 {
		t.Fatalf("%+v", got)
	}
	if got := Footprint(mods("web"), 0); got.Verdict != "unknown" {
		t.Fatalf("undeclared server size must not claim a fit: %+v", got)
	}
	// limit defaults to 1.5x the request when the manifest does not declare one
	if f := Footprint(mods("desk"), 4096); f.LimitMB != 375 {
		t.Fatalf("default limit = %d", f.LimitMB)
	}
	if tight := Footprint(mods("web"), PlatformBaselineMB+20+5); tight.Verdict != "tight" {
		t.Fatalf("%+v", tight)
	}
}

func TestAnalyseGraph(t *testing.T) {
	cat, _ := LoadCatalogue(writeModules(t))
	a := cat.Analyse(map[string]bool{"crm": true}, nil)
	n := a.Need("crm", "leads")
	if n == nil || n.State != NeedMissing || strings.Join(n.Available, ",") != "alt,web" {
		t.Fatalf("%+v", n)
	}
	if !a.Blocking() {
		t.Fatal("an unmet required capability blocks")
	}
	a = cat.Analyse(map[string]bool{"crm": true, "web": true}, nil)
	if n := a.Need("crm", "leads"); n.State != NeedMet || n.Providers[0] != "web" {
		t.Fatalf("%+v", n)
	}
	if n := a.Need("web", "email.send"); n == nil || n.State != NeedPlatform {
		t.Fatalf("platform capability: %+v", n)
	}
	// planned capability: shown as not yet served, never counted as met
	a = cat.Analyse(map[string]bool{"plan": true}, nil)
	if n := a.Need("plan", "invoices"); n.State != NeedPlanned {
		t.Fatalf("%+v", n)
	}
	if p := a.Provided("plan", "invoices"); p == nil || p.Served {
		t.Fatalf("planned provision must be flagged: %+v", p)
	}
	// exclusive capability served twice needs a primary
	a = cat.Analyse(map[string]bool{"web": true, "alt": true}, nil)
	if len(a.Conflicts) != 1 || a.Conflicts[0].Capability != "leads" || !a.Blocking() {
		t.Fatalf("%+v", a.Conflicts)
	}
	a = cat.Analyse(map[string]bool{"web": true, "alt": true}, map[string]string{"leads": "web"})
	if len(a.Conflicts) != 0 {
		t.Fatalf("primary designated: %+v", a.Conflicts)
	}
}

func TestSettingsSchemaPathCannotEscapeTheModuleDirectory(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "esc")
	must(t, os.MkdirAll(d, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "secret.json"), []byte(`{"type":"object"}`), 0o644))
	must(t, os.WriteFile(filepath.Join(d, "module.yaml"), []byte(manifest("esc", "Esc", "settings_schema: ../secret.json\n")), 0o644))
	cat, err := LoadCatalogue(dir)
	must(t, err)
	if e := cat.Get("esc"); e == nil || e.Schema != nil || e.SchemaProblem == "" {
		t.Fatalf("%+v", e)
	}
}
