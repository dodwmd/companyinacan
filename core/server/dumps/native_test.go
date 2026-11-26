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

func nativeFixture(t *testing.T) (*KubeHooks, *kube.Fake, *config.Module) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "forms", "chart"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "forms", "chart", "values.yaml"),
		[]byte("image:\n  repository: ghcr.io/dodwmd/can-forms\n  tag: 0.1.0\n  digest: sha256:REPLACE_WITH_DIGEST\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &config.Module{ID: "forms"}
	m.Requires.Database = "sqlite"
	m.Resources.RAMLimitMB = 32
	m.Hooks.Command, m.Hooks.Backup = "/can-forms", "backup"
	api := kube.NewFake()
	for _, o := range []kube.Object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "mod-forms"}},
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "forms-data", "namespace": "mod-forms"}},
	} {
		if err := api.Apply(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	h := &KubeHooks{API: api, ModulesDir: root, PlatformURL: "http://can-core:8090", Poll: time.Millisecond,
		Manifest: func(context.Context, string) (*config.Module, error) { return m, nil }}
	return h, api, m
}

func deployForms(t *testing.T, api *kube.Fake) {
	t.Helper()
	if err := api.Apply(context.Background(), kube.Object{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "forms", "namespace": "mod-forms"},
		"spec": map[string]any{"replicas": 0, "template": map[string]any{"spec": map[string]any{
			"securityContext": map[string]any{"runAsUser": 65532},
			"containers":      []any{map[string]any{"name": "app", "image": "ghcr.io/dodwmd/can-forms:0.1.0@sha256:abc"}},
		}}}}); err != nil {
		t.Fatal(err)
	}
}

// play marks the first hook Job with status and, when exit >= 0, adds its terminated pod.
func play(t *testing.T, api *kube.Fake, status string, exit int, message string) <-chan string {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		for {
			jobs, _ := api.List(context.Background(), "Job", "mod-forms", "")
			if len(jobs) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			raw, _ := json.Marshal(jobs[0])
			name := jobs[0]["metadata"].(map[string]any)["name"].(string)
			if exit >= 0 {
				_ = api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": name + "-x", "namespace": "mod-forms", "labels": map[string]any{"job-name": name}},
					"status": map[string]any{"containerStatuses": []any{map[string]any{"name": "hook", "state": map[string]any{
						"terminated": map[string]any{"exitCode": exit, "reason": "Error", "message": message}}}}}})
			}
			_ = api.MergePatch(context.Background(), "Job", "mod-forms", name, kube.Object{"status": map[string]any{status: 1}})
			out <- string(raw)
			return
		}
	}()
	return out
}

func runForms(h *KubeHooks) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.RunBackup(ctx, "forms")
}

// The in-house hook is the module's own image and binary, without a shell, a script or a Secret,
// within the module's memory limit and restricted pod security.
func TestNativeHookRunsTheModulesImage(t *testing.T) {
	h, api, _ := nativeFixture(t)
	deployForms(t, api) // scaled to zero: a disabled module is dumped the same way
	created := play(t, api, "succeeded", -1, "")
	if err := runForms(h); err != nil {
		t.Fatal(err)
	}
	job := <-created
	for _, want := range []string{
		`"image":"ghcr.io/dodwmd/can-forms:0.1.0@sha256:abc"`, `"command":["/can-forms","backup"]`, `"limits":{"memory":"32Mi"}`, `"GOMEMLIMIT"`, `"24MiB"`,
		`"runAsNonRoot":true`, `"runAsUser":65532`, `"allowPrivilegeEscalation":false`, `"drop":["ALL"]`, `"type":"RuntimeDefault"`, `"readOnlyRootFilesystem":true`,
		`"automountServiceAccountToken":false`, `"audience":"can"`, `"claimName":"forms-data"`, `"mountPath":"/data"`, `"serviceAccountName":"forms"`,
		`"name":"forms-config","optional":true`, `"/etc/can-hook-config/forms.yaml"`, `"FallbackToLogsOnError"`,
	} {
		if !strings.Contains(job, want) {
			t.Errorf("job lacks %s:\n%s", want, job)
		}
	}
	for _, never := range []string{"hostPath", "secretRef", "secretKeyRef", `"secret":`, "can-module", "/bin/sh"} {
		if strings.Contains(job, never) {
			t.Errorf("job contains %s:\n%s", never, job)
		}
	}
	if _, err := api.Get(context.Background(), "ConfigMap", "mod-forms", "can-hook-backup"); !kube.IsNotFound(err) {
		t.Errorf("no script ConfigMap is written: %v", err)
	}
}

func TestNativeHookWithoutADeploymentUsesTheChartImage(t *testing.T) {
	h, api, _ := nativeFixture(t)
	created := play(t, api, "succeeded", -1, "")
	if err := runForms(h); err != nil {
		t.Fatal(err)
	}
	if job := <-created; !strings.Contains(job, `"image":"ghcr.io/dodwmd/can-forms:0.1.0"`) {
		t.Fatal(job)
	}
}

func TestNativeHookExitCodes(t *testing.T) {
	h, api, _ := nativeFixture(t)
	deployForms(t, api)
	play(t, api, "failed", 3, "backup: there is no database")
	if err := runForms(h); !errors.Is(err, ErrNoData) {
		t.Fatalf("exit 3 must be ErrNoData: %v", err)
	}
	h, api, _ = nativeFixture(t)
	deployForms(t, api)
	play(t, api, "failed", 1, "backup: integrity check failed: page 3")
	if err := runForms(h); err == nil || errors.Is(err, ErrNoData) || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("the failure must say why: %v", err)
	}
}

func TestNativeHookRefusesOtherCommands(t *testing.T) {
	h, _, m := nativeFixture(t)
	for _, bad := range [][2]string{{"/bin/sh", "backup"}, {"/can-forms", "./hooks/x.sh"}} {
		m.Hooks.Command, m.Hooks.Backup = bad[0], bad[1]
		if err := runForms(h); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
