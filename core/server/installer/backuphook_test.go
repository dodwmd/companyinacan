package installer

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/dumps"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

// E08b QA3 1: a database module's backup hook reads the Secret the installer really writes, and
// the module counts as having data even without a volume.
func TestBackupHookReadsTheDatabaseSecretTheInstallerWrites(t *testing.T) {
	ctx := context.Background()
	in, api, _, spec := fixture(t, nil)
	spec.Manifest.Requires.Database = "postgres"
	if err := in.Enable(ctx, spec); err != nil {
		t.Fatal(err)
	}
	claims, err := api.List(ctx, "PersistentVolumeClaim", "mod-stub", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims {
		if err := api.Delete(ctx, "PersistentVolumeClaim", "mod-stub", nameOf(c)); err != nil {
			t.Fatal(err)
		}
	}

	// the variables every shipped database hook requires
	required := map[string]bool{}
	for _, m := range []string{"crm"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "modules", m, "hooks", "backup.sh"))
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range regexp.MustCompile(`\$\{(DB_[A-Z]+):\?\}`).FindAllStringSubmatch(string(b), -1) {
			required[v[1]] = true
		}
	}
	if len(required) < 4 {
		t.Fatalf("hooks require %v", required)
	}

	mods := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mods, "stub", "hooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mods, "stub", "hooks", "backup.sh"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	man := &config.Module{ID: "stub"}
	man.Requires.Database = "postgres"
	man.Hooks.Backup, man.Hooks.Image = "./hooks/backup.sh", "img:1"
	h := &dumps.KubeHooks{API: api, ModulesDir: mods, PlatformURL: "http://c", Poll: time.Millisecond,
		Manifest: func(context.Context, string) (*config.Module, error) { return man, nil }}

	refs := make(chan []string, 1)
	go func() {
		for {
			jobs, _ := api.List(ctx, "Job", "mod-stub", "")
			for _, j := range jobs {
				if !strings.HasPrefix(nameOf(j), "can-hook-backup-") {
					continue
				}
				c := dig(j, "spec", "template", "spec").(map[string]any)["containers"].([]any)[0].(map[string]any)
				var names []string
				for _, e := range asList(c["envFrom"]) {
					ref := asMap(asMap(e)["secretRef"])
					names = append(names, ref["name"].(string))
					if ref["optional"] == true {
						names = append(names, "OPTIONAL")
					}
				}
				refs <- names
				_ = api.MergePatch(ctx, "Job", "mod-stub", nameOf(j), kube.Object{"status": map[string]any{"succeeded": 1}})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.RunBackup(cctx, "stub"); err != nil {
		t.Fatalf("a database module without a volume still has data: %v", err)
	}
	names := <-refs
	if len(names) != 1 {
		t.Fatalf("envFrom = %v, want exactly the database Secret, not optional", names)
	}
	sec, err := api.Get(ctx, "Secret", "mod-stub", names[0])
	if err != nil {
		t.Fatalf("the hook references a Secret nobody writes: %v", err)
	}
	data := secretData(sec)
	for v := range required {
		if s, _ := data[v].(string); s == "" {
			t.Errorf("Secret %s lacks %s", names[0], v)
		}
	}
	// without the Secret the hook fails loudly instead of being skipped as "no data"
	if err := api.Delete(ctx, "Secret", "mod-stub", names[0]); err != nil {
		t.Fatal(err)
	}
	if err := h.RunBackup(cctx, "stub"); err == nil || strings.Contains(err.Error(), dumps.ErrNoData.Error()) {
		t.Fatalf("got %v", err)
	}
}
