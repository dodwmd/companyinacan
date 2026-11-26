package dumps

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

const (
	hookConfigMap = "can-hook-backup"
	hookJobPrefix = "can-hook-backup-"
	hookUID       = 1000 // the can-hooks image user and the module pods' fsGroup (contracts/hooks.md)
)

// HookDataPaths are where a hook Job mounts the module's volume: the paths the shipped hooks
// default to (analytics /data, wiki /storage, status /app/data).
var HookDataPaths = []string{"/data", "/storage", "/app/data"}

// MountsVolume reports whether a module with this requires.database gets its volume in the hook
// Job: not when the data is in a database server, which the hook reads over the network.
func MountsVolume(database string) bool { return database != "postgres" && database != "mariadb" }

// KubeHooks runs a module's backup hook as a Job in the module's namespace (contracts/hooks.md):
// the hook script from a ConfigMap, the module's data volume, a projected audience-`can` token,
// and a pod that satisfies the restricted Pod Security level.
type KubeHooks struct {
	API         kube.API
	ModulesDir  string // module directories baked into the can-core image
	PlatformURL string // module API URL as seen from module namespaces
	Manifest    func(ctx context.Context, module string) (*config.Module, error)
	// Installed, when set, gives the backup script recorded for a module whose installed release is
	// older than the one this can-core ships; ok is false to use the shipped file.
	Installed func(ctx context.Context, module, rel string) (script string, ok bool)
	Poll      time.Duration // default 2 s
	// PullTimeout bounds how long a hook pod may fail to start (image pull, configuration)
	// before the hook is given up (default 2 minutes).
	PullTimeout time.Duration
}

type obj = map[string]any

// RunBackup implements HookRunner. It returns once the Job succeeded; the Job is deleted in
// every case, so a timed-out hook does not keep running under a later run.
func (h *KubeHooks) RunBackup(ctx context.Context, module string) error {
	if !moduleRe.MatchString(module) {
		return ErrBadName
	}
	m, err := h.Manifest(ctx, module)
	if err != nil {
		return err
	}
	if m.Hooks.Builtin != nil {
		return h.builtinJob(ctx, module, m, false)
	}
	if m.Hooks.Command != "" {
		return h.runNative(ctx, module, m)
	}
	if m.Hooks.Image == "" {
		return errors.New("the module has a backup hook but no hooks.image to run it in")
	}
	var script string
	ok := false
	if h.Installed != nil {
		script, ok = h.Installed(ctx, module, m.Hooks.Backup)
	}
	if !ok {
		if script, err = h.script(module, m.Hooks.Backup); err != nil {
			return err
		}
	}
	ns := "mod-" + module
	database := !MountsVolume(m.Requires.Database) // the data is in a database server
	if ok, err := h.hasData(ctx, ns, database); err != nil {
		return err
	} else if !ok {
		return ErrNoData
	}
	labels := obj{"app.kubernetes.io/name": "can-hook", "app.kubernetes.io/part-of": "companyinacan", "can.dev/hook": "backup"}
	if err := h.API.Apply(ctx, kube.Object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": obj{"name": hookConfigMap, "namespace": ns, "labels": labels},
		"data":     obj{"backup.sh": script}}); err != nil {
		return fmt.Errorf("hook script ConfigMap: %w", err)
	}
	claim := ""
	if MountsVolume(m.Requires.Database) {
		if claim, err = h.dataClaim(ctx, ns, module); err != nil {
			return err
		}
	}
	name := fmt.Sprintf("%s%d", hookJobPrefix, time.Now().UnixNano())
	if _, err := h.API.Create(ctx, h.job(ctx, name, ns, module, m.Hooks.Image, claim, m.Requires.Database, labels)); err != nil {
		return fmt.Errorf("create the hook Job: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = h.API.Delete(cctx, "Job", ns, name)
	}()
	return h.wait(ctx, ns, name)
}

// hasData reports whether the module has anything to dump. A database module's data is in the
// database server, so a missing namespace or database Secret is an error, never "nothing to
// dump". The one exception: a Secret without DB_HOST belongs to a database that was never
// provisioned (the installer writes the password first and the connection settings only once the
// server accepted it, and never removes them), so such a module has never held data. Otherwise a module without a volume (never installed, or removed by hand)
// has nothing.
func (h *KubeHooks) hasData(ctx context.Context, ns string, database bool) (bool, error) {
	if database {
		sec, err := h.API.Get(ctx, "Secret", ns, reconcile.DatabaseSecret)
		if err != nil {
			return false, fmt.Errorf("the database module's Secret %s/%s cannot be read: %w", ns, reconcile.DatabaseSecret, err)
		}
		data, _ := sec["data"].(obj)
		host, _ := data["DB_HOST"].(string)
		return host != "", nil
	}
	if _, err := h.API.Get(ctx, "Namespace", "", ns); kube.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("read the namespace: %w", err)
	}
	claims, err := h.API.List(ctx, "PersistentVolumeClaim", ns, "")
	if err != nil {
		return false, fmt.Errorf("list volumes: %w", err)
	}
	return len(claims) > 0, nil
}

// script reads the manifest's hook file, refusing anything outside the module's directory.
func (h *KubeHooks) script(module, rel string) (string, error) {
	p := path.Clean(strings.TrimPrefix(rel, "./"))
	if !fs.ValidPath(p) || p == "." {
		return "", fmt.Errorf("hook path %q must be inside the module directory", rel)
	}
	b, err := fs.ReadFile(os.DirFS(path.Join(h.ModulesDir, module)), p)
	if err != nil {
		return "", fmt.Errorf("hook script: %w", err)
	}
	return string(b), nil
}

// dataClaim picks the volume the hook works on: <module>-data (what the module charts create),
// else the namespace's only claim, else none.
func (h *KubeHooks) dataClaim(ctx context.Context, ns, module string) (string, error) {
	l, err := h.API.List(ctx, "PersistentVolumeClaim", ns, "")
	if err != nil {
		return "", fmt.Errorf("list volumes: %w", err)
	}
	var names []string
	for _, o := range l {
		md, _ := o["metadata"].(obj)
		n, _ := md["name"].(string)
		if n == module+"-data" {
			return n, nil
		}
		names = append(names, n)
	}
	if len(names) == 1 {
		return names[0], nil
	}
	if len(names) > 1 {
		return "", fmt.Errorf("%s has %d volumes and none is called %s-data: cannot choose one", ns, len(names), module)
	}
	return "", nil
}

func (h *KubeHooks) job(ctx context.Context, name, ns, module, image, claim, database string, labels obj) kube.Object {
	readOnly := database != "sqlite"
	var envFrom []any
	if !MountsVolume(database) {
		envFrom = []any{obj{"secretRef": obj{"name": reconcile.DatabaseSecret}}}
	}
	deadline := int64(900)
	if d, ok := ctx.Deadline(); ok {
		if s := int64(time.Until(d).Seconds()); s > 0 && s < deadline {
			deadline = s
		}
	}
	mounts := []any{
		obj{"name": "script", "mountPath": "/hooks", "readOnly": true},
		obj{"name": "tmp", "mountPath": "/tmp"},
		obj{"name": "can-token", "mountPath": "/var/run/secrets/can", "readOnly": true},
	}
	volumes := []any{
		obj{"name": "script", "configMap": obj{"name": hookConfigMap}},
		obj{"name": "tmp", "emptyDir": obj{}},
		obj{"name": "can-token", "projected": obj{"sources": []any{obj{"serviceAccountToken": obj{"audience": "can", "expirationSeconds": 3600, "path": "token"}}}}},
	}
	if claim != "" {
		// Read-only unless the hook must take an SQLite online backup, which writes the WAL index.
		for _, p := range HookDataPaths {
			mounts = append(mounts, obj{"name": "data", "mountPath": p, "readOnly": readOnly})
		}
		volumes = append(volumes, obj{"name": "data", "persistentVolumeClaim": obj{"claimName": claim}})
	}
	return kube.Object{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": obj{"name": name, "namespace": ns, "labels": labels},
		"spec": obj{
			"backoffLimit": 0, "activeDeadlineSeconds": deadline, "ttlSecondsAfterFinished": 3600,
			"template": obj{
				"metadata": obj{"labels": labels},
				"spec": obj{
					"restartPolicy": "Never", "serviceAccountName": module, "automountServiceAccountToken": false,
					"enableServiceLinks": false,
					"securityContext": obj{"runAsNonRoot": true, "runAsUser": hookUID, "runAsGroup": hookUID, "fsGroup": hookUID,
						"seccompProfile": obj{"type": "RuntimeDefault"}},
					"containers": []any{obj{
						"name": "hook", "image": image, "imagePullPolicy": "IfNotPresent",
						"command": []any{"/bin/sh", "/hooks/backup.sh"},
						"env": []any{
							obj{"name": "CAN_API_URL", "value": h.PlatformURL},
							obj{"name": "CAN_MODULE", "value": module},
						},
						"envFrom":   envFrom,
						"resources": obj{"requests": obj{"cpu": "10m", "memory": "16Mi"}, "limits": obj{"memory": "64Mi"}},
						"securityContext": obj{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
							"capabilities": obj{"drop": []any{"ALL"}}},
						"volumeMounts": mounts,
					}},
					"volumes": volumes,
				},
			},
		},
	}
}

func (h *KubeHooks) wait(ctx context.Context, ns, name string) error {
	every := h.Poll
	if every <= 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	pull := h.PullTimeout
	if pull <= 0 {
		pull = 2 * time.Minute
	}
	var stuckSince time.Time
	// lost is why the pod was last seen evicted or not scheduled. It is remembered: a Job that
	// runs into its deadline has no pod left to ask.
	lost := ""
	for {
		j, err := h.API.Get(ctx, "Job", ns, name)
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("read the hook Job: %w", err)
		}
		pods, perr := h.API.List(ctx, "Pod", ns, "job-name="+name)
		if perr == nil && len(pods) > 0 {
			lost = ""
			if why := podLost(pods); why != "" {
				lost = ": " + why
			}
		}
		st, _ := j["status"].(obj)
		if count(st["succeeded"]) > 0 {
			return nil
		}
		if count(st["failed"]) > 0 {
			return fmt.Errorf("hook Job %s/%s failed%s (kubectl -n %s logs job/%s)", ns, name, lost, ns, name)
		}
		switch reason := stuck(pods); {
		case reason == "":
			stuckSince = time.Time{}
		case stuckSince.IsZero():
			stuckSince = time.Now()
		case time.Since(stuckSince) >= pull:
			return fmt.Errorf("hook Job %s/%s cannot start: %s for %s", ns, name, reason, pull)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("hook Job %s/%s did not finish%s: %w", ns, name, lost, ctx.Err())
		case <-t.C:
		}
	}
}

// stuck returns why the Job's pod cannot start, or "" when it is not in such a state.
func stuck(pods []kube.Object) string {
	for _, p := range pods {
		st, _ := p["status"].(obj)
		for _, c := range containerStatuses(st) {
			m, _ := c.(obj)
			state, _ := m["state"].(obj)
			w, _ := state["waiting"].(obj)
			switch r, _ := w["reason"].(string); r {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ErrImageNeverPull", "CreateContainerConfigError":
				return r
			}
		}
	}
	return ""
}

// podLost says why the node or the scheduler stopped a Job's pod from running: an eviction (disk
// pressure, a work volume over its limit), a deletion by the cluster, or no node to put it on.
// "" when none of these is the case.
func podLost(pods []kube.Object) string {
	for _, p := range pods {
		st, _ := p["status"].(obj)
		if phase, _ := st["phase"].(string); phase == "Failed" {
			if reason, _ := st["reason"].(string); reason != "" {
				msg, _ := st["message"].(string)
				return "the node stopped the pod (" + reason + "): " + oneLine(msg)
			}
		}
		conditions, _ := st["conditions"].([]any)
		for _, c := range conditions {
			m, _ := c.(obj)
			kind, _ := m["type"].(string)
			status, _ := m["status"].(string)
			reason, _ := m["reason"].(string)
			msg, _ := m["message"].(string)
			switch {
			case kind == "DisruptionTarget" && status == "True":
				return "the cluster removed the pod (" + reason + "): " + oneLine(msg)
			case kind == "PodScheduled" && status == "False" && reason == "Unschedulable":
				return "the pod could not be scheduled (Unschedulable): " + oneLine(msg)
			}
		}
	}
	return ""
}

// oneLine folds a cluster message onto one bounded line.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}

// containerStatuses are the statuses of a pod's init containers and containers.
func containerStatuses(status obj) []any {
	init, _ := status["initContainerStatuses"].([]any)
	cs, _ := status["containerStatuses"].([]any)
	return append(append([]any{}, init...), cs...)
}

func count(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}
