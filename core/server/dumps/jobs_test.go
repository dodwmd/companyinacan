package dumps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

func hookFixture(t *testing.T) (*KubeHooks, *kube.Fake, *config.Module) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wiki", "hooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wiki", "hooks", "backup.sh"), []byte("#!/bin/sh\necho hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &config.Module{ID: "wiki"}
	m.Hooks.Backup = "./hooks/backup.sh"
	m.Hooks.Image = "ghcr.io/dodwmd/can-hooks:0.1.0"
	api := kube.NewFake()
	if err := api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "wiki-data", "namespace": "mod-wiki"}}); err != nil {
		t.Fatal(err)
	}
	if err := api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "mod-wiki"}}); err != nil {
		t.Fatal(err)
	}
	h := &KubeHooks{API: api, ModulesDir: root, PlatformURL: "http://can-core:8090", Poll: time.Millisecond,
		Manifest: func(context.Context, string) (*config.Module, error) { return m, nil }}
	return h, api, m
}

// finishJob plays the Job controller: it marks the first hook Job it sees with the given status
// and returns a copy of the Job as it was created.
func finishJob(api *kube.Fake, status string) <-chan kube.Object {
	out := make(chan kube.Object, 1)
	go func() {
		for {
			jobs, _ := api.List(context.Background(), "Job", "mod-wiki", "")
			if len(jobs) > 0 {
				out <- jobs[0]
				name := jobs[0]["metadata"].(map[string]any)["name"].(string)
				_ = api.MergePatch(context.Background(), "Job", "mod-wiki", name, kube.Object{"status": map[string]any{status: 1}})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return out
}

func TestKubeHooksRunsARestrictedJobAndCleansUp(t *testing.T) {
	h, api, _ := hookFixture(t)
	created := finishJob(api, "succeeded")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.RunBackup(ctx, "wiki"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(<-created)
	job := string(raw)
	for _, want := range []string{
		`"runAsNonRoot":true`, `"allowPrivilegeEscalation":false`, `"drop":["ALL"]`, `"type":"RuntimeDefault"`, `"readOnlyRootFilesystem":true`,
		`"automountServiceAccountToken":false`, `"audience":"can"`, `"claimName":"wiki-data"`, `"mountPath":"/storage"`, `"mountPath":"/data"`,
		`"CAN_API_URL"`, `"http://can-core:8090"`, `"backoffLimit":0`, `"ghcr.io/dodwmd/can-hooks:0.1.0"`, `"serviceAccountName":"wiki"`,
	} {
		if !strings.Contains(job, want) {
			t.Errorf("job lacks %s:\n%s", want, job)
		}
	}
	if strings.Contains(job, "hostPath") {
		t.Error("no hostPath in hook jobs")
	}
	cm, err := api.Get(context.Background(), "ConfigMap", "mod-wiki", "can-hook-backup")
	if err != nil || !strings.Contains(cm["data"].(map[string]any)["backup.sh"].(string), "echo hi") {
		t.Fatalf("script ConfigMap: %v %v", cm, err)
	}
	left, _ := api.List(context.Background(), "Job", "mod-wiki", "")
	if len(left) != 0 {
		t.Fatalf("finished jobs must be deleted: %v", left)
	}
}

func TestKubeHooksFailedJobIsAnError(t *testing.T) {
	h, api, _ := hookFixture(t)
	finishJob(api, "failed")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.RunBackup(ctx, "wiki"); err == nil {
		t.Fatal("a failed job must fail the hook")
	}
}

func TestKubeHooksTimeoutIsAnErrorAndDeletesTheJob(t *testing.T) {
	h, api, _ := hookFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.RunBackup(ctx, "wiki"); err == nil {
		t.Fatal("expected a timeout")
	}
	left, _ := api.List(context.Background(), "Job", "mod-wiki", "")
	if len(left) != 0 {
		t.Fatalf("a timed-out job must not keep running: %v", left)
	}
}

func TestKubeHooksRefusesScriptOutsideTheModuleAndMissingImage(t *testing.T) {
	h, _, m := hookFixture(t)
	m.Hooks.Backup = "../../etc/passwd"
	if err := h.RunBackup(context.Background(), "wiki"); err == nil {
		t.Fatal("path traversal")
	}
	m.Hooks.Backup, m.Hooks.Image = "./hooks/backup.sh", ""
	if err := h.RunBackup(context.Background(), "wiki"); err == nil || !strings.Contains(err.Error(), "hooks.image") {
		t.Fatalf("got %v", err)
	}
}

// QA 2: a module record without any data (a failed first install, a namespace removed by hand)
// has nothing to dump; it must neither block backups nor be reported as dumped.
func TestKubeHooksReportsNoData(t *testing.T) {
	h, api, _ := hookFixture(t)
	if err := api.Delete(context.Background(), "PersistentVolumeClaim", "mod-wiki", "wiki-data"); err != nil {
		t.Fatal(err)
	}
	if err := h.RunBackup(context.Background(), "wiki"); !errors.Is(err, ErrNoData) {
		t.Fatalf("got %v, want ErrNoData", err)
	}
}
