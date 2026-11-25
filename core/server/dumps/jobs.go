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
	Poll        time.Duration // default 2 s
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
	if m.Hooks.Image == "" {
		return errors.New("the module has a backup hook but no hooks.image to run it in")
	}
	script, err := h.script(module, m.Hooks.Backup)
	if err != nil {
		return err
	}
	ns := "mod-" + module
	if ok, err := h.hasData(ctx, ns); err != nil {
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
	if _, err := h.API.Create(ctx, h.job(ctx, name, ns, module, m.Hooks.Image, claim, m.Requires.Database != "sqlite", labels)); err != nil {
		return fmt.Errorf("create the hook Job: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = h.API.Delete(cctx, "Job", ns, name)
	}()
	return h.wait(ctx, ns, name)
}

// hasData reports whether the module has anything to dump: a volume or a database. A module
// that never installed, or whose namespace was removed, has neither.
func (h *KubeHooks) hasData(ctx context.Context, ns string) (bool, error) {
	if _, err := h.API.Get(ctx, "Namespace", "", ns); kube.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("read the namespace: %w", err)
	}
	claims, err := h.API.List(ctx, "PersistentVolumeClaim", ns, "")
	if err != nil {
		return false, fmt.Errorf("list volumes: %w", err)
	}
	if len(claims) > 0 {
		return true, nil
	}
	if _, err := h.API.Get(ctx, "Secret", ns, "can-database"); kube.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("read the database Secret: %w", err)
	}
	return true, nil
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

func (h *KubeHooks) job(ctx context.Context, name, ns, module, image, claim string, readOnly bool, labels obj) kube.Object {
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
						"envFrom":   []any{obj{"secretRef": obj{"name": "can-database", "optional": true}}},
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
	for {
		j, err := h.API.Get(ctx, "Job", ns, name)
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("read the hook Job: %w", err)
		}
		st, _ := j["status"].(obj)
		if count(st["succeeded"]) > 0 {
			return nil
		}
		if count(st["failed"]) > 0 {
			return fmt.Errorf("hook Job %s/%s failed (kubectl -n %s logs job/%s)", ns, name, ns, name)
		}
		switch reason := h.stuck(ctx, ns, name); {
		case reason == "":
			stuckSince = time.Time{}
		case stuckSince.IsZero():
			stuckSince = time.Now()
		case time.Since(stuckSince) >= pull:
			return fmt.Errorf("hook Job %s/%s cannot start: %s for %s", ns, name, reason, pull)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("hook Job %s/%s did not finish: %w", ns, name, ctx.Err())
		case <-t.C:
		}
	}
}

// stuck returns why the Job's pod cannot start, or "" when it is not in such a state.
func (h *KubeHooks) stuck(ctx context.Context, ns, job string) string {
	pods, err := h.API.List(ctx, "Pod", ns, "job-name="+job)
	if err != nil {
		return ""
	}
	for _, p := range pods {
		st, _ := p["status"].(obj)
		cs, _ := st["containerStatuses"].([]any)
		for _, c := range cs {
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
