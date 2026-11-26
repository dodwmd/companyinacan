package dumps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

func deadline(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func asJSON(t *testing.T, o kube.Object) string {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The restore hook gets the module's volume writable, whatever the backup hook got, and runs the
// restore script.
func TestRunRestoreScript(t *testing.T) {
	h, api, m := hookFixture(t)
	m.Requires.Database = "none"
	m.Hooks.Restore = m.Hooks.Backup
	if err := api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "mod-wiki"}}); err != nil {
		t.Fatal(err)
	}
	created := finishJob(api, "succeeded")
	if err := h.RunRestore(deadline(t), "wiki"); err != nil {
		t.Fatal(err)
	}
	job := asJSON(t, <-created)
	for _, want := range []string{`"claimName":"wiki-data"`, `"readOnly":false`, `"name":"can-hook-restore"`, `/hooks/backup.sh`, `"can.dev/hook":"restore"`} {
		if !strings.Contains(job, want) {
			t.Errorf("missing %s in %s", want, job)
		}
	}
	if strings.Contains(job, `"mountPath":"/data","name":"data","readOnly":true`) {
		t.Error("the volume is read-only")
	}
}

func deployment(t *testing.T, api *kube.Fake) {
	t.Helper()
	if err := api.Apply(context.Background(), kube.Object{"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "wiki", "namespace": "mod-wiki"},
		"spec": map[string]any{"replicas": 0, "template": map[string]any{"spec": map[string]any{
			"securityContext": map[string]any{"runAsUser": 65532},
			"volumes":         []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": "wiki-data"}}},
			"containers": []any{map[string]any{"name": "app", "image": "img:1", "args": []any{"serve"}, "ports": []any{},
				"readinessProbe": map[string]any{}}},
		}}}}); err != nil {
		t.Fatal(err)
	}
}

// An in-house purge runs the module's binary with its own pod template, offline.
func TestRunPurgeCLI(t *testing.T) {
	h, api, _ := hookFixture(t)
	deployment(t, api)
	created := finishJob(api, "succeeded")
	if err := h.RunPurgeCLI(deadline(t), "wiki", "pers_1", "directory-basic"); err != nil {
		t.Fatal(err)
	}
	job := asJSON(t, <-created)
	for _, want := range []string{`"args":["purge","-subject","pers_1","-class","directory-basic"]`, `"image":"img:1"`, `"restartPolicy":"Never"`, `"claimName":"wiki-data"`} {
		if !strings.Contains(job, want) {
			t.Errorf("missing %s in %s", want, job)
		}
	}
	for _, not := range []string{"readinessProbe", `"ports"`, `"serve"`} {
		if strings.Contains(job, not) {
			t.Errorf("%s must not be in the purge Job", not)
		}
	}
}

// specs/043 contract: `<command> restore -replace` in the module's own image.
func TestRunSubcommandRestore(t *testing.T) {
	h, api, _ := hookFixture(t)
	manifest := filepath.Join(h.ModulesDir, "wiki", "module.yaml")
	if err := os.WriteFile(manifest, []byte("hooks:\n  command: /can-wiki\n  restore: restore\nresources:\n  ram_limit_mb: 64\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deployment(t, api)
	if err := api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "wiki-data", "namespace": "mod-wiki"}}); err != nil {
		t.Fatal(err)
	}
	if !h.HasSubcommandRestore("wiki") || h.HasSubcommandRestore("nothing") {
		t.Fatal("detection")
	}
	created := finishJob(api, "succeeded")
	if err := h.RunRestore(deadline(t), "wiki"); err != nil {
		t.Fatal(err)
	}
	job := asJSON(t, <-created)
	for _, want := range []string{`"command":["/can-wiki","restore","-replace"]`, `"image":"img:1"`, `"runAsUser":65532`, `"mountPath":"/data"`, `"wiki-config"`} {
		if !strings.Contains(job, want) {
			t.Errorf("missing %s in %s", want, job)
		}
	}
	if err := os.WriteFile(manifest, []byte("hooks:\n  command: /bin/sh\n  restore: restore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.RunRestore(deadline(t), "wiki"); err == nil {
		t.Fatal("only /can-* binaries")
	}
}
