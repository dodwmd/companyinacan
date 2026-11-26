package dumps

// The export Jobs of `can export` (specs/070-company-export): the backup Jobs with another
// subcommand, which send their stream to PUT /v1/exports instead of the dumps. A Job gets no
// Secret, with one exception: when the operator asked for sealed fields, an in-house module's Job
// mounts the same keyring (or finance's seal key) its service runs with.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modbackup"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

// ErrNoExport is returned for a module that cannot be exported by this version; the error says why.
var ErrNoExport = errors.New("this module cannot be exported")

const (
	exportJobPrefix = "can-export-"
	// exportSeconds bounds one module's export Job: the stream goes at the speed of whoever
	// downloads it.
	exportSeconds = 3600
	// pgExportFile is the plain SQL dump on the Job's work volume.
	pgExportFile = pgWorkDir + "/database.sql"
	// moduleSecret is the Secret can-core generates a module's own secrets into.
	moduleSecret = "can-module"
)

// RunExport runs the export Job of one module and returns when it has finished. ErrNoData: there
// is nothing to export. ErrNoExport: the module has no export.
func (h *KubeHooks) RunExport(ctx context.Context, module string, includeSealed bool) error {
	if !moduleRe.MatchString(module) {
		return ErrBadName
	}
	m, err := h.Manifest(ctx, module)
	if err != nil {
		return err
	}
	var job kube.Object
	name := fmt.Sprintf("%s%d", exportJobPrefix, time.Now().UnixNano())
	switch {
	case m.Hooks.Builtin != nil:
		job, err = h.builtinExportJob(ctx, name, module, m)
	case m.Hooks.Command != "":
		job, err = h.nativeExportJob(ctx, name, module, m, includeSealed)
	case m.Hooks.Backup == "":
		return fmt.Errorf("%w: it declares no data hook", ErrNoExport)
	default:
		return fmt.Errorf("%w: its data hooks are shell scripts, which have no export yet (use the application's own export)", ErrNoExport)
	}
	if err != nil {
		return err
	}
	ns := "mod-" + module
	if _, err := h.API.Create(ctx, job); err != nil {
		return fmt.Errorf("create the export Job: %w", err)
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

// asExport relabels a hook Job and gives it the export's deadline.
func asExport(job kube.Object) (pod obj) {
	labels := obj{"app.kubernetes.io/name": "can-hook", "app.kubernetes.io/part-of": "companyinacan", "can.dev/hook": "export"}
	md, _ := job["metadata"].(obj)
	md["labels"] = labels
	spec, _ := job["spec"].(obj)
	spec["activeDeadlineSeconds"] = int64(exportSeconds)
	tpl, _ := spec["template"].(obj)
	tpl["metadata"] = obj{"labels": labels}
	pod, _ = tpl["spec"].(obj)
	return pod
}

func (h *KubeHooks) nativeExportJob(ctx context.Context, name, module string, m *config.Module, includeSealed bool) (kube.Object, error) {
	if !commandRe.MatchString(m.Hooks.Command) {
		return nil, errors.New("hooks.command does not name a module binary")
	}
	if m.Requires.Database != "sqlite" {
		return nil, fmt.Errorf("%w: it requires %q, not SQLite", ErrNoExport, m.Requires.Database)
	}
	ns := "mod-" + module
	if ok, err := h.hasData(ctx, ns, false); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNoData
	}
	claim, err := h.dataClaim(ctx, ns, module)
	if err != nil {
		return nil, err
	}
	if claim == "" {
		return nil, ErrNoData
	}
	image, uid, err := h.workload(ctx, ns, module)
	if err != nil {
		return nil, err
	}
	job := h.nativeJob(ctx, name, ns, module, image, claim, uid, m)
	pod := asExport(job)
	containers, _ := pod["containers"].([]any)
	c, _ := containers[0].(obj)
	command := []any{m.Hooks.Command, "export"}
	if includeSealed {
		command = append(command, "-include-sealed")
		if err := h.addKeys(ctx, ns, module, pod, c); err != nil {
			return nil, err
		}
	}
	c["command"] = command
	return job, nil
}

// addKeys gives the export container what its service decrypts with: the keyring volume of the
// module's Deployment, and for finance the seal key in its Secret. Nothing else of the
// Deployment's environment is copied.
func (h *KubeHooks) addKeys(ctx context.Context, ns, module string, pod, container obj) error {
	d, err := h.API.Get(ctx, "Deployment", ns, module)
	if kube.IsNotFound(err) {
		return errors.New("sealed fields need the module's Deployment, which names its keyring; enable the module once, or export without sealed fields")
	} else if err != nil {
		return fmt.Errorf("read the module's Deployment: %w", err)
	}
	spec, _ := d["spec"].(obj)
	tpl, _ := spec["template"].(obj)
	src, _ := tpl["spec"].(obj)
	named := func(list any) []any {
		var out []any
		all, _ := list.([]any)
		for _, v := range all {
			if o, _ := v.(obj); o["name"] == "keyring" {
				out = append(out, o)
			}
		}
		return out
	}
	containers, _ := src["containers"].([]any)
	for _, c := range containers {
		cm, _ := c.(obj)
		mounts := named(cm["volumeMounts"])
		volumes := named(src["volumes"])
		if len(mounts) > 0 && len(volumes) > 0 {
			have, _ := container["volumeMounts"].([]any)
			container["volumeMounts"] = append(have, mounts...)
			vols, _ := pod["volumes"].([]any)
			pod["volumes"] = append(vols, volumes...)
			break
		}
	}
	if module == "finance" {
		env, _ := container["env"].([]any)
		container["env"] = append(env, obj{"name": "CAN_FINANCE_SEAL_KEY",
			"valueFrom": obj{"secretKeyRef": obj{"name": moduleSecret, "key": "CAN_FINANCE_SEAL_KEY", "optional": true}}})
	}
	return nil
}

// pgExportScript writes a plain SQL snapshot of the database without the rows of the excluded
// tables.
func pgExportScript(excludes []string) string {
	var flags strings.Builder
	for _, e := range excludes {
		flags.WriteString(" --exclude-table-data='" + e + "'")
	}
	return "set -eu\numask 027\n" + pgWait + "pg_dump --format=plain --no-owner --no-privileges" + flags.String() + " >" + pgExportFile + "\n"
}

func (h *KubeHooks) builtinExportJob(ctx context.Context, name, module string, m *config.Module) (kube.Object, error) {
	b := m.Hooks.Builtin
	excludes, ok := modbackup.PGExportExcludes(module)
	if !ok {
		return nil, fmt.Errorf("%w: this version does not know which of its data is secret", ErrNoExport)
	}
	database := m.Requires.Database
	if database == "mariadb" {
		return nil, fmt.Errorf("%w: can-core cannot dump a MariaDB database", ErrNoExport)
	}
	postgres := database == "postgres"
	ns := "mod-" + module
	if ok, err := h.hasData(ctx, ns, postgres); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrNoData
	}
	claim := ""
	if b.SQLite != "" || b.Files {
		var err error
		if claim, err = h.dataClaim(ctx, ns, module); err != nil {
			return nil, err
		}
		if claim == "" {
			return nil, ErrNoData
		}
	}
	_, uid, err := h.workload(ctx, ns, module)
	if err != nil {
		return nil, err
	}
	self, err := h.selfImage(ctx)
	if err != nil {
		return nil, err
	}
	pg := ""
	if postgres {
		if pg, err = postgresImage(); err != nil {
			return nil, err
		}
	}
	job := h.builtinSpec(ctx, builtinParams{name: name, ns: ns, module: module, kind: "backup", self: self, pg: pg, claim: claim, uid: uid, builtin: *b})
	pod := asExport(job)
	containers, _ := pod["containers"].([]any)
	data, _ := containers[0].(obj)
	command := []any{"/can-core", "module-data", "export", "--work", pgWorkDir}
	if claim != "" {
		command = append(command, "--data", nativeDataPath)
	}
	if b.SQLite != "" {
		command = append(command, "--sqlite", b.SQLite)
	}
	if b.Files {
		command = append(command, "--files")
	}
	if pg != "" {
		command = append(command, "--pgdump", pgExportFile)
		init, _ := pod["initContainers"].([]any)
		client, _ := init[0].(obj)
		client["command"] = []any{"/bin/sh", "-c", pgExportScript(excludes)}
	}
	data["command"] = command
	return job, nil
}
