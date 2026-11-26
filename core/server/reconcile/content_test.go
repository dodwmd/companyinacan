package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/server/store"
	"github.com/dodwmd/companyinacan/core/validate"
	"github.com/dodwmd/companyinacan/modules"
)

// copyModules copies the shipped modules named, so that a test can edit them.
func copyModules(t *testing.T, ids ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, id := range ids {
		if err := os.CopyFS(filepath.Join(dir, id), os.DirFS(filepath.Join("../../../modules", id))); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadModules(t *testing.T, dir string, ids ...string) Desired {
	t.Helper()
	company := "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nemail: {relay: {host: smtp.a.example, port: 587, from: a@a.example}}\nmodules:\n"
	for _, id := range ids {
		company += "  " + id + ": {}\n"
	}
	d, ps := LoadDesiredBytes("company.yaml", []byte(company), dir)
	if problem.HasErrors(ps) {
		t.Fatal(ps)
	}
	return d
}

func replaceIn(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s does not contain %q", path, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), old, new, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func apply(t *testing.T, g *rig, d Desired) Result {
	t.Helper()
	res, err := g.r.Apply(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// shippedFormsTag is the image tag the shipped forms chart names.
func shippedFormsTag(t *testing.T) string {
	t.Helper()
	c, err := ContentOf("../../../modules/forms/module.yaml", &config.Module{ID: "forms", Runtime: loadModules(t, "../../../modules", "forms").Manifests["forms"].Runtime})
	if err != nil || len(c.Images) != 1 {
		t.Fatalf("%v %v", c.Images, err)
	}
	_, tag, _ := strings.Cut(c.Images[0], ":")
	return tag
}

// The modules built into `can` hash as the directories can-core has: otherwise a company without a
// modules directory would plan an upgrade of everything, or miss one.
func TestBuiltInAndOnDiskModulesHaveTheSameContent(t *testing.T) {
	validate.UseShipped(modules.Shipped)
	paths, err := filepath.Glob("../../../modules/*/module.yaml")
	if err != nil || len(paths) < 10 {
		t.Fatal(paths, err)
	}
	for _, p := range paths {
		id := filepath.Base(filepath.Dir(p))
		doc, ps := config.Load(p)
		if doc == nil {
			t.Fatal(ps)
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			t.Fatal(err)
		}
		disk, err := ContentOf(p, m)
		if err != nil {
			t.Fatal(id, err)
		}
		builtinPath, ok := validate.ManifestPath(validate.BuiltinModules, strings.TrimPrefix(id, "_"))
		if !ok {
			if strings.HasPrefix(id, "_") {
				continue // go:embed leaves the example out
			}
			t.Fatalf("%s is not built in", id)
		}
		builtin, err := ContentOf(builtinPath, m)
		if err != nil {
			t.Fatal(id, err)
		}
		if !reflect.DeepEqual(disk, builtin) {
			t.Errorf("%s: on disk %+v, built in %+v", id, disk, builtin)
		}
		if m.Runtime.Chart != "" && builtin.Chart == "" {
			t.Errorf("%s: the chart was not hashed", id)
		}
	}
}

// Applying the shipped modules twice, each time read afresh from disk, leaves nothing to do.
func TestASecondApplyOfShippedModulesPlansNothing(t *testing.T) {
	ids := []string{"forms", "status", "jobs"}
	g := newRig(t)
	g.r.RequireApproval, g.r.Backoff = true, true
	apply(t, g, loadModules(t, "../../../modules", ids...))
	for range 3 {
		res := apply(t, g, loadModules(t, "../../../modules", ids...))
		if len(res.Actions)+len(res.Held) != 0 {
			t.Fatalf("a second apply plans %+v %+v", res.Actions, res.Held)
		}
	}
}

// Every change without a version bump is an upgrade; comments and documentation are not.
func TestContentChangesAreUpgradesWithoutAVersionBump(t *testing.T) {
	dir := copyModules(t, "forms")
	g := newRig(t)
	apply(t, g, loadModules(t, dir, "forms"))
	check := func(name, wantReason string) {
		t.Helper()
		res := apply(t, g, loadModules(t, dir, "forms"))
		if wantReason == "" {
			if len(res.Actions) != 0 {
				t.Fatalf("%s: planned %+v", name, res.Actions)
			}
			return
		}
		if kinds(res.Actions) != "upgrade:forms" || res.Actions[0].Reason != wantReason {
			t.Fatalf("%s: planned %+v, want %q", name, res.Actions, wantReason)
		}
		if again := apply(t, g, loadModules(t, dir, "forms")); len(again.Actions) != 0 {
			t.Fatalf("%s: planned again: %+v", name, again.Actions)
		}
	}
	manifest := filepath.Join(dir, "forms", "module.yaml")
	values := filepath.Join(dir, "forms", "chart", "values.yaml")
	replaceIn(t, manifest, "schema: 1\n", "# a comment\nschema: 1\n")
	check("a comment in the manifest", "")
	if err := os.WriteFile(filepath.Join(dir, "forms", "chart", "README.md"), []byte("docs"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("chart documentation", "")
	replaceIn(t, manifest, "category: sales", "category: marketing")
	check("the manifest", "manifest changed")
	replaceIn(t, filepath.Join(dir, "forms", "chart", "templates", "deployment.yaml"), "terminationGracePeriodSeconds: 30", "terminationGracePeriodSeconds: 31")
	check("a template", "chart changed")
	tag := "tag: " + shippedFormsTag(t)
	replaceIn(t, values, tag, tag+"1")
	check("the image", "image ghcr.io/dodwmd/can-forms:"+shippedFormsTag(t)+" -> ghcr.io/dodwmd/can-forms:"+shippedFormsTag(t)+"1")
	rec, err := g.db.GetModule(context.Background(), "forms")
	if err != nil || rec.Manifest.Category != "marketing" {
		t.Fatalf("the manifest was not recorded again: %q %v", rec.Manifest.Category, err)
	}
}

// A record from before content hashes is applied once more, and then left alone.
func TestAnExistingInstallIsRecordedOnce(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	g.r.RequireApproval = true
	d := loadModules(t, "../../../modules", "forms")
	apply(t, g, d)
	before, _ := g.db.GetModule(ctx, "forms")
	if _, err := g.db.ExecContext(ctx, `UPDATE modules SET content_json=''`); err != nil {
		t.Fatal(err)
	}
	res := apply(t, g, d)
	if kinds(res.Actions) != "upgrade:forms" || !strings.Contains(res.Actions[0].Reason, "(once)") || len(res.Held) != 0 {
		t.Fatalf("%+v %+v", res.Actions, res.Held)
	}
	after, _ := g.db.GetModule(ctx, "forms")
	if after.HMACSecret != before.HMACSecret || after.State != store.StateEnabled || after.Content == "" {
		t.Fatalf("the re-record changed more than the record: %+v", after)
	}
	if res := apply(t, g, d); len(res.Actions) != 0 {
		t.Fatalf("planned again: %+v", res.Actions)
	}
	if slices.Contains(g.fk.CallLog(), "disable:forms") {
		t.Fatal("the module was stopped")
	}
}

// A widening upgrade waits for approval of exactly what it widens; the running release stays.
func TestAWideningUpgradeWaitsForApproval(t *testing.T) {
	ctx := context.Background()
	dir := copyModules(t, "forms")
	g := newRig(t)
	g.r.RequireApproval = true
	apply(t, g, loadModules(t, dir, "forms"))
	manifest := filepath.Join(dir, "forms", "module.yaml")
	replaceIn(t, manifest, "leads.erase]\n", "leads.erase, leads.export]\n")
	replaceIn(t, manifest, "  viewer: [leads.read]", "  viewer: [leads.read, leads.export]")

	calls := len(g.fk.CallLog())
	res := apply(t, g, loadModules(t, dir, "forms"))
	if len(res.Actions) != 0 || len(res.Held) != 1 || res.Held[0].Held != HeldApproval || len(g.fk.CallLog()) != calls {
		t.Fatalf("not held: %+v", res)
	}
	want := []string{"new permission leads.export", "role viewer gains leads.export"}
	if !slices.Equal(res.Held[0].Widens, want) {
		t.Fatalf("widens %q", res.Held[0].Widens)
	}
	rec, _ := g.db.GetModule(ctx, "forms")
	if p, ok := ParsePending(rec.Pending); !ok || p.Approval != res.Held[0].Approval || slices.Contains(rec.Manifest.Permissions, "leads.export") {
		t.Fatalf("record: %q %v", rec.Pending, rec.Manifest.Permissions)
	}

	// An approval of something else does not count.
	if err := g.db.ApproveUpgrade(ctx, "forms", "0000000000000000"); err != nil {
		t.Fatal(err)
	}
	if res := apply(t, g, loadModules(t, dir, "forms")); len(res.Held) != 1 {
		t.Fatalf("a wrong approval was accepted: %+v", res)
	}
	if err := g.db.ApproveUpgrade(ctx, "forms", res.Held[0].Approval); err != nil {
		t.Fatal(err)
	}
	if got := apply(t, g, loadModules(t, dir, "forms")); kinds(got.Actions) != "upgrade:forms" || len(got.Held) != 0 {
		t.Fatalf("approved: %+v", got)
	}
	rec, _ = g.db.GetModule(ctx, "forms")
	if rec.Pending != "" || rec.ApprovedUpgrade != "" || !slices.Contains(rec.Manifest.Permissions, "leads.export") {
		t.Fatalf("after the upgrade: %+v", rec)
	}
}

func TestWidening(t *testing.T) {
	old := mf("m", "1", nil)
	old.Permissions = []string{"a"}
	next := mf("m", "1", []string{"email.send"}, "tickets")
	next.Permissions = []string{"a"}
	next.Requires.Database = "postgres"
	next.Runtime.SiteRoute = &config.SiteRoute{Path: "/_m"}
	want := []string{"gets a postgres database", "may use capability email.send", "public path on the site /_m", "serves capability tickets"}
	if got := Widening(old, next); !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	if got := Widening(next, old); len(got) != 0 {
		t.Fatalf("narrowing is not widening: %q", got)
	}
}

// An upgrade that fails is not retried at once for ever, but a different target is.
func TestAFailedUpgradeIsRetriedWithADelay(t *testing.T) {
	g := newRig(t)
	g.r.Backoff = true
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g.db.Now = func() time.Time { return now }
	m := mf("work", "1", nil)
	apply(t, g, desired(nil, nil, m))
	next := mf("work", "2", nil)
	g.fk.FailEnable["work"] = errors.New("image cannot be pulled")
	try := func(d Desired) Result {
		t.Helper()
		res, _ := g.r.Apply(context.Background(), d, Options{})
		return res
	}
	if res := try(desired(nil, nil, next)); len(res.Failed) != 1 {
		t.Fatalf("%+v", res)
	}
	if res := try(desired(nil, nil, next)); len(res.Actions) != 0 || len(res.Held) != 1 || res.Held[0].Held != HeldRetry {
		t.Fatalf("retried at once: %+v", res)
	}
	now = now.Add(time.Minute)
	if res := try(desired(nil, nil, next)); len(res.Failed) != 1 {
		t.Fatalf("not retried after the delay: %+v", res)
	}
	now = now.Add(time.Minute)
	if res := try(desired(nil, nil, next)); len(res.Held) != 1 {
		t.Fatalf("the delay does not grow: %+v", res)
	}
	delete(g.fk.FailEnable, "work")
	if res := try(desired(nil, nil, mf("work", "3", nil))); kinds(res.Actions) != "upgrade:work" || len(res.Failed) != 0 {
		t.Fatalf("a new version waits: %+v", res)
	}
}
