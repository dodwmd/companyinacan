package dumps

// In-house modules (specs/043-module-backup-hooks): the backup hook is a subcommand of the
// module's own binary. The Job runs the image the module's Deployment names (so the tool and the
// schema it reads are the same version), as the same user, with no script, no shell and no Secret.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

const (
	nativeUID = 65532 // distroless nonroot: what the in-house charts run as
	// nativeNoData is the subcommand's exit code for "there is no database" (modbackup.ExitNoData).
	nativeNoData = 3
	// nativeDataPath is where every in-house chart mounts its volume.
	nativeDataPath = "/data"
	hookConfigDir  = "/etc/can-hook-config"
	defaultLimitMB = 64
)

var (
	commandRe    = regexp.MustCompile(`^/can-[a-z]+$`)
	subcommandRe = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)
)

func (h *KubeHooks) runNative(ctx context.Context, module string, m *config.Module) error {
	if !commandRe.MatchString(m.Hooks.Command) || !subcommandRe.MatchString(m.Hooks.Backup) {
		return errors.New("hooks.command and hooks.backup do not name a subcommand of a module binary")
	}
	if m.Requires.Database != "sqlite" {
		return fmt.Errorf("hooks.command backs up a SQLite database, the module requires %q", m.Requires.Database)
	}
	ns := "mod-" + module
	if ok, err := h.hasData(ctx, ns, false); err != nil {
		return err
	} else if !ok {
		return ErrNoData
	}
	claim, err := h.dataClaim(ctx, ns, module)
	if err != nil {
		return err
	}
	if claim == "" {
		return ErrNoData
	}
	image, uid, err := h.workload(ctx, ns, module)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s%d", hookJobPrefix, time.Now().UnixNano())
	if _, err := h.API.Create(ctx, h.nativeJob(ctx, name, ns, module, image, claim, uid, m)); err != nil {
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

// workload returns the image and user of the module's Deployment (kept, scaled to zero, while the
// module is disabled). Without one (a first install that failed and was rolled back) the chart's
// image in can-core's own module directory is used.
func (h *KubeHooks) workload(ctx context.Context, ns, module string) (string, int, error) {
	d, err := h.API.Get(ctx, "Deployment", ns, module)
	if kube.IsNotFound(err) {
		image, err := h.chartImage(module)
		return image, nativeUID, err
	}
	if err != nil {
		return "", 0, fmt.Errorf("read the module's Deployment: %w", err)
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
		return "", 0, errors.New("the module's Deployment names no image")
	}
	uid := nativeUID
	sc, _ := pod["securityContext"].(obj)
	if n := count(sc["runAsUser"]); n > 0 {
		uid = n
	}
	return image, uid, nil
}

func (h *KubeHooks) chartImage(module string) (string, error) {
	b, err := os.ReadFile(path.Join(h.ModulesDir, module, "chart", "values.yaml")) //nolint:gosec // validated module id
	if err != nil {
		return "", fmt.Errorf("the module has no Deployment and no chart values: %w", err)
	}
	var v struct {
		Image struct{ Repository, Tag, Digest string }
	}
	if err := yaml.Unmarshal(b, &v); err != nil || v.Image.Repository == "" {
		return "", errors.New("the module has no Deployment and its chart names no image")
	}
	ref := v.Image.Repository
	if v.Image.Tag != "" {
		ref += ":" + v.Image.Tag
	}
	if d := v.Image.Digest; d != "" && !strings.Contains(d, "REPLACE_WITH_") {
		ref += "@" + d
	}
	return ref, nil
}

// nativeJob: memory is bounded by the module's own limit; the volume is writable because a reader
// of a live SQLite database in WAL mode writes the shared index.
func (h *KubeHooks) nativeJob(ctx context.Context, name, ns, module, image, claim string, uid int, m *config.Module) kube.Object {
	limit := m.Resources.RAMLimitMB
	if limit <= 0 {
		limit = defaultLimitMB
	}
	labels := obj{"app.kubernetes.io/name": "can-hook", "app.kubernetes.io/part-of": "companyinacan", "can.dev/hook": "backup"}
	deadline := int64(900)
	if d, ok := ctx.Deadline(); ok {
		if s := int64(time.Until(d).Seconds()); s > 0 && s < deadline {
			deadline = s
		}
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
					"securityContext": obj{"runAsNonRoot": true, "runAsUser": uid, "runAsGroup": uid, "fsGroup": uid,
						"seccompProfile": obj{"type": "RuntimeDefault"}},
					"containers": []any{obj{
						"name": "hook", "image": image, "imagePullPolicy": "IfNotPresent",
						"command": []any{m.Hooks.Command, m.Hooks.Backup},
						"env": []any{
							obj{"name": "CAN_API_URL", "value": h.PlatformURL},
							obj{"name": "CAN_MODULE", "value": module},
							obj{"name": "CAN_BACKUP_CONFIG", "value": hookConfigDir + "/" + module + ".yaml"},
							obj{"name": "GOMEMLIMIT", "value": fmt.Sprintf("%dMiB", limit*3/4)},
						},
						"resources": obj{"requests": obj{"cpu": "10m", "memory": "8Mi"}, "limits": obj{"memory": fmt.Sprintf("%dMi", limit)}},
						"securityContext": obj{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
							"capabilities": obj{"drop": []any{"ALL"}}},
						// The last log lines reach the failure message can-core reports.
						"terminationMessagePolicy": "FallbackToLogsOnError",
						"volumeMounts": []any{
							obj{"name": "data", "mountPath": nativeDataPath},
							obj{"name": "config", "mountPath": hookConfigDir, "readOnly": true},
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
				},
			},
		},
	}
}

// explain turns a failed Job into what its container said: exit code 3 is "no data", anything
// else keeps the reason (for example OOMKilled) and the last lines of the log.
func (h *KubeHooks) explain(ctx context.Context, ns, job string, cause error) error {
	pods, err := h.API.List(ctx, "Pod", ns, "job-name="+job)
	if err != nil {
		return cause
	}
	for _, p := range pods {
		st, _ := p["status"].(obj)
		for _, c := range containerStatuses(st) {
			m, _ := c.(obj)
			state, _ := m["state"].(obj)
			t, ok := state["terminated"].(obj)
			if !ok || count(t["exitCode"]) == 0 { // a finished init container is not the cause
				continue
			}
			code := count(t["exitCode"])
			if code == nativeNoData {
				return ErrNoData
			}
			reason, _ := t["reason"].(string)
			msg, _ := t["message"].(string)
			msg = strings.Join(strings.Fields(msg), " ")
			if len(msg) > 400 {
				msg = msg[len(msg)-400:]
			}
			return fmt.Errorf("%w: exit %d %s %s", cause, code, reason, msg)
		}
	}
	return cause
}
