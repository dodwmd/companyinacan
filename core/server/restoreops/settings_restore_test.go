package restoreops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/server/audit"
	"github.com/dodwmd/companyinacan/core/server/installer"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/core/server/registry"
)

// A shorter retention that was waiting for approval when the backup was made does not reach the
// restored data: neither the restore nor the apply that follows it installs it, and it waits again
// (specs/064-module-settings).
func TestARestoreDoesNotApplyAWideningSettingNobodyApproved(t *testing.T) {
	ctx := context.Background()
	o, _, api := ops(t)
	o.ModulesDir = "../../../modules"
	company := func(settings string) []byte {
		return []byte("schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: local}\nmodules:\n  work: {settings: " + settings + "}\n")
	}
	desired := func(settings string) reconcile.Desired {
		d, ps := reconcile.LoadDesiredBytes("company.yaml", company(settings), o.ModulesDir)
		if problem.HasErrors(ps) {
			t.Fatal(ps)
		}
		return d
	}
	fake := reconcile.NewFakeInstaller()
	r := reconcile.New(o.DB, registry.New(o.DB, ""), audit.New(o.DB), fake)
	r.RequireApproval = true
	apply := func(d reconcile.Desired) reconcile.Result {
		res, err := r.Apply(ctx, d, reconcile.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	apply(desired("{retention_days: 3650}"))
	// Asked for and held; with it a change that widens nothing.
	const pending = "{retention_days: 30, max_body_bytes: 2097152}"
	if res := apply(desired(pending)); len(res.Held) != 1 {
		t.Fatalf("%+v", res)
	}

	export, err := json.Marshal(backup.SecretsExport{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	for file, content := range map[string][]byte{backup.SecretsDump: export, backup.CompanyDump: company(pending)} {
		p := filepath.Join(o.DumpsDir, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o.Installer = func(seed func(context.Context, reconcile.Spec) error, _ string) reconcile.ModuleInstaller {
		return installer.New(installer.Config{API: api, ModulesDir: o.ModulesDir, PlatformURL: "http://can-core.can-system.svc:8090",
			NodeIP: "172.18.0.2", Domain: "acme.example", Quiesce: true, Seed: seed, ChildAddressSpaceMB: -1, Poll: time.Millisecond})
	}
	if rep, err := o.Apply(ctx, false); err != nil || len(rep.Modules) != 1 {
		t.Fatalf("the module must be recreated: %v %+v", err, rep)
	}
	cm, err := api.Get(ctx, "ConfigMap", "mod-work", "work-config")
	if err != nil {
		t.Fatal(err)
	}
	var conf map[string]any
	raw, _ := cm["data"].(map[string]any)["work.yaml"].(string)
	if err := yaml.Unmarshal([]byte(raw), &conf); err != nil {
		t.Fatal(err)
	}
	if conf["retention_days"] != 3650 || conf["max_body_bytes"] != 2097152 {
		t.Fatalf("restored with retention_days %v, max_body_bytes %v\n%s", conf["retention_days"], conf["max_body_bytes"], raw)
	}

	if _, err := o.Complete(ctx); err != nil {
		t.Fatal(err)
	}
	d := desired(pending)
	if res := apply(d); len(res.Actions) != 1 || len(res.Held) != 0 {
		t.Fatalf("the apply after a restore starts the module without waiting: %+v", res)
	}
	spec, ok := fake.EnabledSpec("work")
	if !ok || spec.Within == nil {
		t.Fatalf("%+v", spec)
	}
	rec, err := o.DB.GetModule(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	was := reconcile.ParseApplied(rec.Applied)
	if was.Values["config.retention_days"] != int64(3650) || was.Values["config.max_body_bytes"] != int64(2097152) || rec.ApprovedUpgrade != "" {
		t.Fatalf("recorded %v, approval %q", was.Values, rec.ApprovedUpgrade)
	}
	res := apply(d)
	if len(res.Held) != 1 || res.Held[0].Held != reconcile.HeldApproval || !strings.Contains(strings.Join(res.Held[0].Widens, "\n"), "setting retention_days becomes 30") {
		t.Fatalf("it must wait for approval again: %+v", res)
	}
	if err := o.DB.ApproveUpgrade(ctx, "work", res.Held[0].Approval); err != nil {
		t.Fatal(err)
	}
	if res := apply(d); len(res.Actions) != 1 || len(res.Held) != 0 {
		t.Fatalf("%+v", res)
	}
	if spec, _ := fake.EnabledSpec("work"); spec.Within != nil {
		t.Fatal("an approved upgrade installs what company.yaml says")
	}
	if rec, _ := o.DB.GetModule(ctx, "work"); reconcile.ParseApplied(rec.Applied).Values["config.retention_days"] != int64(30) {
		t.Fatalf("%s", rec.Applied)
	}
}
