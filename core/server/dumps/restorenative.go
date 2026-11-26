package dumps

// In-house modules (specs/043-module-backup-hooks, contracts/hooks.md): the restore hook is a
// subcommand of the module's own binary, named by hooks.command and hooks.restore. It is read
// from the manifest this can-core ships, so a module recorded before it had hooks is restored too.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

const nativeRestoreUID = 65532 // distroless nonroot: what the in-house charts run as

var (
	nativeCommandRe = regexp.MustCompile(`^/can-[a-z]+$`)
	nativeSubRe     = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)
)

type shippedHooks struct {
	Command, Restore string
	RAMLimitMB       int
}

// native returns the subcommand hooks of the shipped manifest, nil when it names none.
func (h *KubeHooks) native(module string) (*shippedHooks, error) {
	if !moduleRe.MatchString(module) {
		return nil, ErrBadName
	}
	b, err := os.ReadFile(path.Join(h.ModulesDir, module, "module.yaml")) //nolint:gosec // validated module id
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m struct {
		Hooks struct {
			Command string `yaml:"command"`
			Restore string `yaml:"restore"`
		} `yaml:"hooks"`
		Resources struct {
			RAMLimitMB int `yaml:"ram_limit_mb"`
		} `yaml:"resources"`
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("module.yaml of %s: %w", module, err)
	}
	if m.Hooks.Command == "" {
		return nil, nil
	}
	if !nativeCommandRe.MatchString(m.Hooks.Command) || !nativeSubRe.MatchString(m.Hooks.Restore) {
		return nil, fmt.Errorf("module %s: hooks.command and hooks.restore do not name a subcommand of a module binary", module)
	}
	return &shippedHooks{Command: m.Hooks.Command, Restore: m.Hooks.Restore, RAMLimitMB: m.Resources.RAMLimitMB}, nil
}

// HasSubcommandRestore reports whether the module is restored by a subcommand of its own binary.
func (h *KubeHooks) HasSubcommandRestore(module string) bool {
	s, err := h.native(module)
	return err == nil && s != nil
}

// runNativeRestore runs `<command> <restore> -replace` in a Job made like the backup Job: the
// image and user of the module's Deployment, its data volume at /data and its config.
func (h *KubeHooks) runNativeRestore(ctx context.Context, module string, s *shippedHooks) error {
	ns := "mod-" + module
	d, err := h.API.Get(ctx, "Deployment", ns, module)
	if err != nil {
		return fmt.Errorf("read the module's Deployment: %w", err)
	}
	spec, _ := d["spec"].(obj)
	tpl, _ := spec["template"].(obj)
	pod, _ := tpl["spec"].(obj)
	containers, _ := pod["containers"].([]any)
	image := ""
	for _, c := range containers {
		cm, _ := c.(obj)
		if n, _ := cm["name"].(string); image == "" || n == "app" {
			image, _ = cm["image"].(string)
		}
	}
	if image == "" {
		return errors.New("the module's Deployment names no image")
	}
	uid := nativeRestoreUID
	sc, _ := pod["securityContext"].(obj)
	if n := count(sc["runAsUser"]); n > 0 {
		uid = n
	}
	claim, err := h.dataClaim(ctx, ns, module)
	if err != nil {
		return err
	}
	if claim == "" {
		return errors.New("the module has no data volume")
	}
	limit := s.RAMLimitMB
	if limit <= 0 {
		limit = 64
	}
	name := fmt.Sprintf("can-hook-restore-%d", time.Now().UnixNano())
	labels := obj{"app.kubernetes.io/name": "can-hook", "app.kubernetes.io/part-of": "companyinacan", "can.dev/hook": "restore"}
	return h.runJob(ctx, ns, name, kube.Object{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": obj{"name": name, "namespace": ns, "labels": labels},
		"spec": obj{"backoffLimit": 0, "activeDeadlineSeconds": int64(900), "ttlSecondsAfterFinished": 3600,
			"template": obj{"metadata": obj{"labels": labels}, "spec": obj{
				"restartPolicy": "Never", "serviceAccountName": module, "automountServiceAccountToken": false, "enableServiceLinks": false,
				"securityContext": obj{"runAsNonRoot": true, "runAsUser": uid, "runAsGroup": uid, "fsGroup": uid,
					"seccompProfile": obj{"type": "RuntimeDefault"}},
				"containers": []any{obj{
					"name": "hook", "image": image, "imagePullPolicy": "IfNotPresent",
					"command": []any{s.Command, s.Restore, "-replace"},
					"env": []any{
						obj{"name": "CAN_API_URL", "value": h.PlatformURL},
						obj{"name": "CAN_MODULE", "value": module},
						obj{"name": "CAN_BACKUP_CONFIG", "value": "/etc/can-hook-config/" + module + ".yaml"},
						obj{"name": "GOMEMLIMIT", "value": fmt.Sprintf("%dMiB", limit*3/4)},
					},
					"resources": obj{"requests": obj{"cpu": "10m", "memory": "8Mi"}, "limits": obj{"memory": fmt.Sprintf("%dMi", limit)}},
					"securityContext": obj{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
						"capabilities": obj{"drop": []any{"ALL"}}},
					"terminationMessagePolicy": "FallbackToLogsOnError",
					"volumeMounts": []any{
						obj{"name": "data", "mountPath": "/data"},
						obj{"name": "config", "mountPath": "/etc/can-hook-config", "readOnly": true},
						obj{"name": "tmp", "mountPath": "/tmp"},
						obj{"name": "can-token", "mountPath": "/var/run/secrets/can", "readOnly": true},
					},
				}},
				"volumes": []any{
					obj{"name": "data", "persistentVolumeClaim": obj{"claimName": claim}},
					obj{"name": "config", "configMap": obj{"name": module + "-config", "optional": true}},
					obj{"name": "tmp", "emptyDir": obj{}},
					obj{"name": "can-token", "projected": obj{"sources": []any{obj{"serviceAccountToken": obj{"audience": "can", "expirationSeconds": 3600, "path": "token"}}}}},
				},
			}}},
	})
}
