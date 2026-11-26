package dumps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	for _, want := range []string{`"sizeLimit":"9Gi"`, `"runAsUser":82`, `"runAsUser":70`, `"fsGroup":82`, `"readOnlyRootFilesystem":true`, `"drop":["ALL"]`, `"memory":"128Mi"`, `"memory":"96Mi"`, `"claimName":"wiki-data"`} {
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

// QA: a restore downloads the module's whole set into the work volume, so every set the dump
// store accepts must fit in it.
func TestBuiltinWorkVolumeHoldsWhatTheStoreAccepts(t *testing.T) {
	gib, err := strconv.ParseInt(strings.TrimSuffix(workMaxSize, "Gi"), 10, 64)
	if err != nil {
		t.Fatal(workMaxSize, err)
	}
	var s Store
	if gib<<30 <= s.maxModuleBytes() || gib<<30 <= s.limit() {
		t.Errorf("work volume %s does not hold %d bytes", workMaxSize, s.maxModuleBytes())
	}
	h, api := builtinFixture(t, "postgres", config.BuiltinBackup{Files: true})
	manifest := "schema: 1\nid: wiki\nrequires: {database: postgres}\nhooks: {backup: backup, restore: restore, builtin: {files: true}}\n"
	if err := os.WriteFile(filepath.Join(h.ModulesDir, "wiki", "module.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := runBuiltinJob(t, api, func(ctx context.Context) error { return h.RunRestore(ctx, "wiki", false) })
	if !strings.Contains(raw, `"sizeLimit":"`+workMaxSize+`"`) {
		t.Error(raw)
	}
}

// runPGScript runs a database container's script with stand-ins for the Postgres tools, on a
// server of the given server_version_num. It returns the output and whether the data tool ran.
func runPGScript(t *testing.T, script, version string) (string, bool, error) {
	t.Helper()
	bash, err := exec.LookPath("bash") // the image's shell has pipefail
	if err != nil {
		t.Skip("no bash")
	}
	bin, work := t.TempDir(), t.TempDir()
	ran := filepath.Join(work, "ran")
	for name, body := range map[string]string{
		"pg_isready": "exit 0",
		"psql":       `case "$*" in *server_version_num*) echo ` + version + `;; *) cat >/dev/null;; esac`,
		"pg_dump":    ": >" + ran + "; echo PGDMP",
		"pg_restore": ": >" + ran,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!"+bash+"\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // test stubs
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bash, "-c", strings.ReplaceAll(script, pgWorkDir+"/", work+"/")) //nolint:gosec // our script
	cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "DB_HOST=h", "DB_NAME=d", "DB_USER=u", "DB_PASSWORD=p"}
	out, err := cmd.CombinedOutput()
	_, serr := os.Stat(ran)
	return string(out), serr == nil, err
}

// QA: only the server major of the pinned image is dumped and restored; anything else fails
// before a dump is taken or a database touched.
func TestBuiltinPostgresVersion(t *testing.T) {
	img, err := postgresImage()
	if err != nil || !strings.Contains(img, "postgres:"+pgMajor+".") {
		t.Fatal("pgMajor is not the pinned image's major:", img, err)
	}
	for _, script := range []string{pgDumpScript, pgRestoreScript} {
		if out, ran, err := runPGScript(t, script, "170006"); err != nil || !ran {
			t.Errorf("17 refused: %v %s", err, out)
		}
		for _, v := range []string{"150008", "160004", "180000", "90624"} {
			out, ran, err := runPGScript(t, script, v)
			if err == nil || ran || !strings.Contains(out, "unsupported Postgres server") {
				t.Errorf("%s accepted (ran %v): %v %s", v, ran, err, out)
			}
		}
	}
}

// QA: psql exits 3 after an SQL error. That is not "the module has no data": the restore fails
// with what the server said.
func TestBuiltinSQLErrorIsNotNoData(t *testing.T) {
	fail := func(t *testing.T, restore bool, key, container string, exit int) error {
		t.Helper()
		h, api := builtinFixture(t, "postgres", config.BuiltinBackup{Files: true})
		manifest := "schema: 1\nid: wiki\nrequires: {database: postgres}\nhooks: {backup: backup, restore: restore, builtin: {files: true}}\n"
		if err := os.WriteFile(filepath.Join(h.ModulesDir, "wiki", "module.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				jobs, _ := api.List(context.Background(), "Job", "mod-wiki", "")
				if len(jobs) == 0 {
					time.Sleep(time.Millisecond)
					continue
				}
				name := jobs[0]["metadata"].(map[string]any)["name"].(string)
				_ = api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "Pod",
					"metadata": obj{"name": name + "-x", "namespace": "mod-wiki", "labels": obj{"job-name": name}},
					"status": obj{key: []any{obj{"name": container, "state": obj{"terminated": obj{
						"exitCode": exit, "reason": "Error", "message": "ERROR:  permission denied for schema public"}}}}}})
				_ = api.MergePatch(context.Background(), "Job", "mod-wiki", name, kube.Object{"status": obj{"failed": 1}})
				return
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if restore {
			return h.RunRestore(ctx, "wiki", false)
		}
		return h.RunBackup(ctx, "wiki")
	}
	for _, c := range []struct {
		restore        bool
		key, container string
	}{{true, "containerStatuses", "database"}, {false, "initContainerStatuses", "database"}} {
		err := fail(t, c.restore, c.key, c.container, 3)
		if err == nil || errors.Is(err, ErrNoData) || !strings.Contains(err.Error(), "permission denied for schema public") {
			t.Errorf("restore=%v: %v", c.restore, err)
		}
	}
	if err := fail(t, false, "containerStatuses", "hook", 3); !errors.Is(err, ErrNoData) {
		t.Errorf("the hook container's exit 3 is no data: %v", err)
	}
}
