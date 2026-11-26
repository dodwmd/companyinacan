package dumps

// Wrapped modules (specs/046-wrapped-backups): can-core dumps and restores them itself, with
// images that are already on the node. A Job in the module's namespace runs pg_dump or
// pg_restore in the pinned Postgres image of the shared server, and `can-core module-data` (the
// image this can-core runs) to move the dump and the volume's files through /v1/dumps.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
	"github.com/dodwmd/companyinacan/deploy"
)

const (
	pgUID       = 70 // the postgres user of the alpine image
	pgWorkDir   = "/work"
	pgDumpFile  = pgWorkDir + "/database.pgdump"
	dataLimit   = "96Mi"
	pgLimit     = "128Mi"
	workMaxSize = "5Gi" // the per-file cap of the dump store is 4 GiB
)

// pgWait waits for the server: a new pod's network rules may not be programmed yet.
const pgWait = `export PGHOST="$DB_HOST" PGPORT="${DB_PORT:-5432}" PGUSER="$DB_USER" PGPASSWORD="$DB_PASSWORD" PGDATABASE="$DB_NAME" PGCONNECT_TIMEOUT=10
n=0
until pg_isready -q; do n=$((n+1)); [ "$n" -lt 60 ] || { echo "the database server does not answer" >&2; exit 1; }; sleep 2; done
`

// pgDumpScript writes one consistent snapshot (a single transaction) of the module's database.
const pgDumpScript = "set -eu\numask 027\n" + pgWait + "pg_dump --format=custom --no-owner --no-privileges >" + pgDumpFile + "\n"

// pgRestoreScript replaces the database's content in one transaction: everything in the schema
// public goes first. COMMIT is only sent when pg_restore produced the whole script.
const pgRestoreScript = "set -euo pipefail\n" + pgWait + `{
  printf 'BEGIN;\nDROP SCHEMA IF EXISTS public CASCADE;\nCREATE SCHEMA public;\n'
  pg_restore --no-owner --no-privileges -f - ` + pgDumpFile + ` && printf 'COMMIT;\n'
} | psql -X -q -v ON_ERROR_STOP=1 >/dev/null
echo "restore ok: database"
`

// builtin returns the shipped manifest of a module can-core dumps itself, nil for any other.
func (h *KubeHooks) builtin(module string) (*config.Module, error) {
	if !moduleRe.MatchString(module) {
		return nil, ErrBadName
	}
	p := path.Join(h.ModulesDir, module, "module.yaml")
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	doc, ps := config.Load(p)
	if doc == nil {
		return nil, fmt.Errorf("module.yaml of %s: %v", module, ps)
	}
	m, err := config.DecodeModule(doc)
	if err != nil || m.Hooks.Builtin == nil {
		return nil, err
	}
	return m, nil
}

// RunBuiltinRestore replaces a wrapped module's data with its dump. The module must not be
// running, and can-core must be in restore mode (the dumps are downloaded).
func (h *KubeHooks) RunBuiltinRestore(ctx context.Context, module string) error {
	m, err := h.builtin(module)
	if err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("module %s has no built-in restore", module)
	}
	return h.builtinJob(ctx, module, m, true)
}

func (h *KubeHooks) builtinJob(ctx context.Context, module string, m *config.Module, restore bool) error {
	if !moduleRe.MatchString(module) {
		return ErrBadName
	}
	b := m.Hooks.Builtin
	database := m.Requires.Database
	if database == "mariadb" {
		return errors.New("can-core cannot dump a MariaDB database")
	}
	postgres := database == "postgres"
	if !postgres && b.SQLite == "" && !b.Files {
		return errors.New("hooks.builtin names no data")
	}
	ns := "mod-" + module
	if ok, err := h.hasData(ctx, ns, postgres); err != nil {
		return err
	} else if !ok {
		return ErrNoData
	}
	claim := ""
	if b.SQLite != "" || b.Files {
		var err error
		if claim, err = h.dataClaim(ctx, ns, module); err != nil {
			return err
		}
		switch {
		case claim == "" && (postgres || restore):
			return errors.New("the module has no data volume")
		case claim == "":
			return ErrNoData
		}
	}
	_, uid, err := h.workload(ctx, ns, module)
	if err != nil {
		return err
	}
	self, err := h.selfImage(ctx)
	if err != nil {
		return err
	}
	pg := ""
	if postgres {
		if pg, err = postgresImage(); err != nil {
			return err
		}
	}
	kind := "backup"
	if restore {
		kind = "restore"
	}
	name := fmt.Sprintf("can-hook-%s-%d", kind, time.Now().UnixNano())
	job := h.builtinSpec(ctx, builtinParams{name: name, ns: ns, module: module, kind: kind, self: self, pg: pg, claim: claim, uid: uid, builtin: *b})
	if _, err := h.API.Create(ctx, job); err != nil {
		return fmt.Errorf("create the hook Job: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = h.API.Delete(cctx, "Job", ns, name)
	}()
	if err := h.wait(ctx, ns, name); err != nil {
		return h.explain(ctx, ns, name, err)
	}
	return nil
}

// selfImage is the image of the running can-core: it is on the node by definition.
func (h *KubeHooks) selfImage(ctx context.Context) (string, error) {
	d, err := h.API.Get(ctx, "Deployment", "can-system", "can-core")
	if err != nil {
		return "", fmt.Errorf("read can-core's own Deployment: %w", err)
	}
	spec, _ := d["spec"].(obj)
	tpl, _ := spec["template"].(obj)
	pod, _ := tpl["spec"].(obj)
	containers, _ := pod["containers"].([]any)
	for _, c := range containers {
		cm, _ := c.(obj)
		if cm["name"] == "can-core" {
			if image, _ := cm["image"].(string); image != "" {
				return image, nil
			}
		}
	}
	return "", errors.New("can-core's Deployment names no image")
}

// postgresImage is the pinned image of the shared Postgres (deploy/templates/postgres.yaml), so
// that the client is always the server's version.
func postgresImage() (string, error) {
	b, err := deploy.Templates.ReadFile("templates/postgres.yaml")
	if err != nil {
		return "", err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var o struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name  string `yaml:"name"`
							Image string `yaml:"image"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&o); err != nil {
			return "", fmt.Errorf("the postgres template names no image: %w", err)
		}
		if o.Kind != "Deployment" {
			continue
		}
		for _, c := range o.Spec.Template.Spec.Containers {
			if c.Name == "postgres" && c.Image != "" {
				return c.Image, nil
			}
		}
	}
}

type builtinParams struct {
	name, ns, module, kind string
	self, pg, claim        string
	uid                    int
	builtin                config.BuiltinBackup
}

// builtinSpec builds the Job. Only the can-core container gets the token and the volume; only
// the Postgres container gets the database password. They share the work directory (disk, not
// memory), where the dump is readable through the pod's group.
func (h *KubeHooks) builtinSpec(ctx context.Context, p builtinParams) kube.Object {
	restore := p.kind == "restore"
	labels := obj{"app.kubernetes.io/name": "can-hook", "app.kubernetes.io/part-of": "companyinacan", "can.dev/hook": p.kind}
	deadline := int64(900)
	if d, ok := ctx.Deadline(); ok {
		if s := int64(time.Until(d).Seconds()); s > 0 && s < deadline {
			deadline = s
		}
	}
	container := func(name, image, limit string, uid int, command, env, envFrom, mounts []any) obj {
		return obj{
			"name": name, "image": image, "imagePullPolicy": "IfNotPresent", "command": command, "env": env, "envFrom": envFrom,
			"resources": obj{"requests": obj{"cpu": "10m", "memory": "16Mi"}, "limits": obj{"memory": limit}},
			"securityContext": obj{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
				"runAsUser": uid, "capabilities": obj{"drop": []any{"ALL"}}},
			"terminationMessagePolicy": "FallbackToLogsOnError",
			"volumeMounts":             mounts,
		}
	}
	work := obj{"name": "work", "mountPath": pgWorkDir}
	tmp := obj{"name": "tmp", "mountPath": "/tmp"}

	command := []any{"/can-core", "module-data", p.kind, "--work", pgWorkDir}
	mounts := []any{work, tmp, obj{"name": "can-token", "mountPath": "/var/run/secrets/can", "readOnly": true}}
	if p.claim != "" {
		command = append(command, "--data", nativeDataPath)
		// Writable for a restore, and for SQLite: a reader of a WAL database writes its index.
		mounts = append(mounts, obj{"name": "data", "mountPath": nativeDataPath, "readOnly": !restore && p.builtin.SQLite == ""})
	}
	if p.builtin.SQLite != "" {
		command = append(command, "--sqlite", p.builtin.SQLite)
	}
	if p.builtin.Files {
		command = append(command, "--files")
	}
	if p.pg != "" {
		command = append(command, "--pgdump", pgDumpFile)
	}
	data := container("hook", p.self, dataLimit, p.uid, command, []any{
		obj{"name": "CAN_API_URL", "value": h.PlatformURL},
		obj{"name": "CAN_MODULE", "value": p.module},
		obj{"name": "GOMEMLIMIT", "value": "72MiB"},
	}, nil, mounts)

	var init []any
	main := data
	if p.pg != "" {
		script := pgDumpScript
		if restore {
			script = pgRestoreScript
		}
		client := container("database", p.pg, pgLimit, pgUID, []any{"/bin/sh", "-c", script},
			[]any{obj{"name": "HOME", "value": "/tmp"}},
			[]any{obj{"secretRef": obj{"name": reconcile.DatabaseSecret}}}, []any{work, tmp})
		if restore {
			init, main = []any{data}, client // download and verify, then load
		} else {
			init = []any{client} // dump first, then upload it and archive the files
		}
	}
	volumes := []any{
		obj{"name": "work", "emptyDir": obj{"sizeLimit": workMaxSize}},
		obj{"name": "tmp", "emptyDir": obj{}},
		obj{"name": "can-token", "projected": obj{"sources": []any{obj{"serviceAccountToken": obj{"audience": "can", "expirationSeconds": 3600, "path": "token"}}}}},
	}
	if p.claim != "" {
		volumes = append(volumes, obj{"name": "data", "persistentVolumeClaim": obj{"claimName": p.claim}})
	}
	spec := obj{
		"restartPolicy": "Never", "serviceAccountName": p.module, "automountServiceAccountToken": false,
		"enableServiceLinks": false,
		"securityContext": obj{"runAsNonRoot": true, "runAsUser": p.uid, "runAsGroup": p.uid, "fsGroup": p.uid,
			"seccompProfile": obj{"type": "RuntimeDefault"}},
		"containers": []any{main},
		"volumes":    volumes,
	}
	if init != nil {
		spec["initContainers"] = init
	}
	return kube.Object{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": obj{"name": p.name, "namespace": p.ns, "labels": labels},
		"spec": obj{"backoffLimit": 0, "activeDeadlineSeconds": deadline, "ttlSecondsAfterFinished": 3600,
			"template": obj{"metadata": obj{"labels": labels}, "spec": spec}},
	}
}
