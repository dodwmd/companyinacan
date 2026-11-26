package dumps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

func runExportJob(t *testing.T, h *KubeHooks, api *kube.Fake, module string, sealed bool) string {
	t.Helper()
	created := make(chan string, 1)
	go func() {
		for {
			jobs, _ := api.List(context.Background(), "Job", "mod-"+module, "")
			if len(jobs) > 0 {
				raw, _ := json.Marshal(jobs[0])
				name := jobs[0]["metadata"].(map[string]any)["name"].(string)
				_ = api.MergePatch(context.Background(), "Job", "mod-"+module, name, kube.Object{"status": map[string]any{"succeeded": 1}})
				created <- string(raw)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.RunExport(ctx, module, sealed); err != nil {
		t.Fatal(err)
	}
	return <-created
}

func peopleFixture(t *testing.T) (*KubeHooks, *kube.Fake) {
	t.Helper()
	h, api, m := nativeFixture(t)
	m.ID, m.Hooks.Command = "finance", "/can-finance"
	keyringVolume := map[string]any{"name": "keyring", "secret": map[string]any{"secretName": "can-module", "defaultMode": 288,
		"items": []any{map[string]any{"key": "keyring", "path": "keyring"}}}}
	for _, o := range []kube.Object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "mod-finance"}},
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "finance-data", "namespace": "mod-finance"}},
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "finance", "namespace": "mod-finance"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"volumes": []any{map[string]any{"name": "data"}, keyringVolume},
				"containers": []any{map[string]any{"name": "app", "image": "can-finance:1",
					"envFrom":      []any{map[string]any{"secretRef": map[string]any{"name": "can-module"}}},
					"volumeMounts": []any{map[string]any{"name": "keyring", "mountPath": "/etc/can-finance/keys/keyring", "subPath": "keyring", "readOnly": true}}}},
			}}}},
	} {
		if err := api.Apply(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	return h, api
}

// Without sealed fields an export Job holds no Secret at all; with them it gets the keyring and
// finance's seal key, never the service's whole environment. It is as restricted as a backup Job.
func TestNativeExportJob(t *testing.T) {
	h, api := peopleFixture(t)
	job := runExportJob(t, h, api, "finance", false)
	for _, want := range []string{
		`"command":["/can-finance","export"]`, `"limits":{"memory":"32Mi"}`, `"runAsNonRoot":true`, `"allowPrivilegeEscalation":false`, `"drop":["ALL"]`,
		`"readOnlyRootFilesystem":true`, `"automountServiceAccountToken":false`, `"type":"RuntimeDefault"`, `"can.dev/hook":"export"`, `"activeDeadlineSeconds":3600`,
		`"audience":"can"`, `"claimName":"finance-data"`,
	} {
		if !strings.Contains(job, want) {
			t.Errorf("job lacks %s:\n%s", want, job)
		}
	}
	for _, never := range []string{"hostPath", "secretRef", "secretKeyRef", "secretName", "can-module", "keyring", "/bin/sh"} {
		if strings.Contains(job, never) {
			t.Errorf("job without sealed fields contains %s:\n%s", never, job)
		}
	}
	h, api = peopleFixture(t)
	job = runExportJob(t, h, api, "finance", true)
	for _, want := range []string{`"command":["/can-finance","export","-include-sealed"]`, `"secretName":"can-module"`, `"subPath":"keyring"`,
		`"secretKeyRef":{"key":"CAN_FINANCE_SEAL_KEY","name":"can-module","optional":true}`} {
		if !strings.Contains(job, want) {
			t.Errorf("job lacks %s:\n%s", want, job)
		}
	}
	if strings.Contains(job, "envFrom") {
		t.Errorf("the service's whole environment was copied:\n%s", job)
	}
}

func TestWrappedExportJob(t *testing.T) {
	h, api := builtinFixture(t, "postgres", config.BuiltinBackup{Files: true})
	for _, o := range []kube.Object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": obj{"name": "mod-support"}},
		{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": obj{"name": "support-data", "namespace": "mod-support"}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": obj{"name": "can-db", "namespace": "mod-support"}, "data": obj{"DB_HOST": "aA=="}},
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": obj{"name": "support", "namespace": "mod-support"},
			"spec": obj{"template": obj{"spec": obj{"securityContext": obj{"runAsUser": 82}, "containers": []any{obj{"name": "app", "image": "app:1"}}}}}},
	} {
		if err := api.Apply(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	raw := runExportJob(t, h, api, "support", false)
	var j jobView
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		t.Fatal(err)
	}
	init, main := j.Spec.Template.Spec.InitContainers, j.Spec.Template.Spec.Containers
	if len(init) != 1 || len(main) != 1 {
		t.Fatal(raw)
	}
	script := init[0].Command[2]
	for _, want := range []string{"--format=plain", "--exclude-table-data='public.users'", "--exclude-table-data='public.mailboxes'", "--exclude-table-data='*.*token*'", "/work/database.sql"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %s:\n%s", want, script)
		}
	}
	if got := strings.Join(main[0].Command, " "); got != "/can-core module-data export --work /work --data /data --files --pgdump /work/database.sql" {
		t.Error(got)
	}
	// The token and the volume only for can-core's container, the database password only for pg_dump.
	if main[0].mounts() != "work tmp can-token:ro data:ro" || init[0].mounts() != "work tmp" || len(main[0].EnvFrom) != 0 || len(init[0].EnvFrom) != 1 {
		t.Errorf("mounts %q / %q", main[0].mounts(), init[0].mounts())
	}
}

func TestExportRefusals(t *testing.T) {
	h, _, m := hookFixture(t) // shell script hooks
	ctx := context.Background()
	if err := h.RunExport(ctx, "wiki", false); !errors.Is(err, ErrNoExport) {
		t.Fatal(err)
	}
	m.Hooks.Backup = ""
	if err := h.RunExport(ctx, "wiki", false); !errors.Is(err, ErrNoExport) {
		t.Fatal(err)
	}
	// A wrapped module this version has no secret rules for is not exported at all.
	m.Hooks.Backup, m.Hooks.Builtin = "backup", &config.BuiltinBackup{Files: true}
	if err := h.RunExport(ctx, "wiki", false); !errors.Is(err, ErrNoExport) {
		t.Fatal(err)
	}
	h, _, _ = nativeFixture(t)
	if err := h.RunExport(ctx, "nothing", false); err == nil {
		t.Fatal("a module without a namespace")
	}
}
