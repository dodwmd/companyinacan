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

func builtinFixture(t *testing.T, database string, b config.BuiltinBackup) (*KubeHooks, *kube.Fake) {
	t.Helper()
	h, api, m := hookFixture(t)
	m.Hooks.Image = ""
	m.Hooks.Backup, m.Hooks.Restore, m.Hooks.Builtin = "backup", "restore", &b
	m.Requires.Database = database
	for _, o := range []kube.Object{
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": obj{"name": "can-core", "namespace": "can-system"},
			"spec": obj{"template": obj{"spec": obj{"containers": []any{obj{"name": "can-core", "image": "can-core:v1"}}}}}},
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": obj{"name": "wiki", "namespace": "mod-wiki"},
			"spec": obj{"template": obj{"spec": obj{"securityContext": obj{"runAsUser": 82}, "containers": []any{obj{"name": "app", "image": "app:1"}}}}}},
		{"apiVersion": "v1", "kind": "Secret", "metadata": obj{"name": "can-db", "namespace": "mod-wiki"}, "data": obj{"DB_HOST": "aA=="}},
	} {
		if err := api.Apply(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	return h, api
}

type jobView struct {
	Spec struct {
		Template struct {
			Spec struct {
				InitContainers []containerView `json:"initContainers"`
				Containers     []containerView `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

type containerView struct {
	Name, Image  string
	Command      []string
	EnvFrom      []any `json:"envFrom"`
	VolumeMounts []struct {
		Name     string
		ReadOnly bool `json:"readOnly"`
	} `json:"volumeMounts"`
}

func (c containerView) mounts() string {
	var out []string
	for _, m := range c.VolumeMounts {
		s := m.Name
		if m.ReadOnly {
			s += ":ro"
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func runBuiltinJob(t *testing.T, api *kube.Fake, run func(context.Context) error) (string, jobView) {
	t.Helper()
	created := finishJob(api, "succeeded")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(<-created)
	if err != nil {
		t.Fatal(err)
	}
	var j jobView
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	return string(raw), j
}

// The database password and the token never meet in one container, the dump runs before the
// files are archived, and everything is restricted and bounded.
func TestBuiltinPostgresJob(t *testing.T) {
	h, api := builtinFixture(t, "postgres", config.BuiltinBackup{Files: true})
	raw, j := runBuiltinJob(t, api, func(ctx context.Context) error { return h.RunBackup(ctx, "wiki") })
	spec := j.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || len(spec.Containers) != 1 {
		t.Fatal(raw)
	}
	db, up := spec.InitContainers[0], spec.Containers[0]
	pg, err := postgresImage()
	if err != nil || !strings.Contains(pg, "postgres:") || !strings.Contains(pg, "@sha256:") {
		t.Fatal(pg, err)
	}
	if db.Image != pg || len(db.EnvFrom) != 1 || db.mounts() != "work tmp" || !strings.Contains(db.Command[2], "pg_dump") {
		t.Errorf("database container %+v", db)
	}
	if up.Image != "can-core:v1" || len(up.EnvFrom) != 0 || up.mounts() != "work tmp can-token:ro data:ro" ||
		strings.Join(up.Command, " ") != "/can-core module-data backup --work /work --data /data --files --pgdump /work/database.pgdump" {
		t.Errorf("upload container %+v", up)
	}
	for _, want := range []string{`"runAsUser":82`, `"runAsUser":70`, `"fsGroup":82`, `"readOnlyRootFilesystem":true`, `"drop":["ALL"]`, `"memory":"128Mi"`, `"memory":"96Mi"`, `"claimName":"wiki-data"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("job lacks %s", want)
		}
	}
}

// The restore downloads and verifies before pg_restore touches the database; the shipped
// manifest decides, whatever is recorded.
func TestBuiltinRestoreJob(t *testing.T) {
	h, api := builtinFixture(t, "postgres", config.BuiltinBackup{Files: true})
	manifest := "schema: 1\nid: wiki\nrequires: {database: postgres}\nhooks: {backup: backup, restore: restore, builtin: {files: true}}\n"
	if err := os.WriteFile(filepath.Join(h.ModulesDir, "wiki", "module.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if !h.HasSubcommandRestore("wiki") {
		t.Error("a restore must see the built-in hook")
	}
	raw, j := runBuiltinJob(t, api, func(ctx context.Context) error { return h.RunRestore(ctx, "wiki", false) })
	spec := j.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Command[2] != "restore" || spec.InitContainers[0].mounts() != "work tmp can-token:ro data" ||
		len(spec.Containers) != 1 || !strings.Contains(spec.Containers[0].Command[2], "pg_restore") {
		t.Error(raw)
	}
}

func TestBuiltinNeverPassesAsNothingToDo(t *testing.T) {
	// A database module without its Secret is an error.
	h, api := builtinFixture(t, "postgres", config.BuiltinBackup{Files: true})
	if err := api.Delete(context.Background(), "Secret", "mod-wiki", "can-db"); err != nil {
		t.Fatal(err)
	}
	if err := h.RunBackup(context.Background(), "wiki"); err == nil || errors.Is(err, ErrNoData) {
		t.Error("missing database Secret:", err)
	}
	// MariaDB cannot be dumped.
	h, _ = builtinFixture(t, "mariadb", config.BuiltinBackup{Files: true})
	if err := h.RunBackup(context.Background(), "wiki"); err == nil || errors.Is(err, ErrNoData) {
		t.Error("mariadb:", err)
	}
}
