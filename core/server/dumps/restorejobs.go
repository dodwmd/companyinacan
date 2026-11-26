package dumps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

// The restore side of the module hooks (specs/042-restore). Restore and purge Jobs run only
// while the module's workloads are at 0, so they have its volume to themselves.

// RunRestore runs the module's replace-all restore hook as a Job, with its volume writable.
func (h *KubeHooks) RunRestore(ctx context.Context, module string) error {
	return h.runScript(ctx, module, "restore", func(m *config.Module) string { return m.Hooks.Restore }, nil)
}

// RunPurgeScript runs the module's purge(subject, class) hook script.
func (h *KubeHooks) RunPurgeScript(ctx context.Context, module, subject, class string) error {
	return h.runScript(ctx, module, "purge", func(m *config.Module) string { return m.Hooks.Purge },
		[]any{obj{"name": "CAN_PURGE_SUBJECT", "value": subject}, obj{"name": "CAN_PURGE_CLASS", "value": class}})
}

func (h *KubeHooks) runScript(ctx context.Context, module, kind string, script func(*config.Module) string, env []any) error {
	if !moduleRe.MatchString(module) {
		return ErrBadName
	}
	m, err := h.Manifest(ctx, module)
	if err != nil {
		return err
	}
	rel := script(m)
	if rel == "" || m.Hooks.Image == "" {
		return fmt.Errorf("the module has no %s hook or no hooks.image to run it in", kind)
	}
	body, err := h.script(module, rel)
	if err != nil {
		return err
	}
	ns := "mod-" + module
	file := path.Base(path.Clean(rel))
	labels := obj{"app.kubernetes.io/name": "can-hook", "app.kubernetes.io/part-of": "companyinacan", "can.dev/hook": kind}
	cm := "can-hook-" + kind
	if err := h.API.Apply(ctx, kube.Object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": obj{"name": cm, "namespace": ns, "labels": labels},
		"data":     obj{file: body}}); err != nil {
		return fmt.Errorf("hook script ConfigMap: %w", err)
	}
	claim := ""
	if MountsVolume(m.Requires.Database) {
		if claim, err = h.dataClaim(ctx, ns, module); err != nil {
			return err
		}
	}
	name := fmt.Sprintf("can-hook-%s-%d", kind, time.Now().UnixNano())
	job := h.job(ctx, name, ns, module, m.Hooks.Image, claim, m.Requires.Database, labels)
	spec := job["spec"].(obj)["template"].(obj)["spec"].(obj)
	c := spec["containers"].([]any)[0].(obj)
	c["command"] = []any{"/bin/sh", "/hooks/" + file}
	c["env"] = append(c["env"].([]any), env...)
	for _, v := range spec["volumes"].([]any) {
		if v := v.(obj); v["name"] == "script" {
			v["configMap"] = obj{"name": cm}
		}
	}
	for _, mt := range c["volumeMounts"].([]any) {
		if mt := mt.(obj); mt["name"] == "data" {
			mt["readOnly"] = false
		}
	}
	return h.runJob(ctx, ns, name, job)
}

func (h *KubeHooks) runJob(ctx context.Context, ns, name string, job kube.Object) error {
	if _, err := h.API.Create(ctx, job); err != nil {
		return fmt.Errorf("create the hook Job: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = h.API.Delete(cctx, "Job", ns, name)
	}()
	return h.wait(ctx, ns, name)
}

// RunPurgeCLI runs an in-house module's own `purge -subject S -class C` command: a Job made from
// the pod template of the module's Deployment (same image, environment, volumes and security
// context) with the command replaced.
func (h *KubeHooks) RunPurgeCLI(ctx context.Context, module, subject, class string) error {
	if !moduleRe.MatchString(module) {
		return ErrBadName
	}
	ns := "mod-" + module
	d, err := h.API.Get(ctx, "Deployment", ns, module)
	if err != nil {
		return fmt.Errorf("read the module's Deployment: %w", err)
	}
	var copyOf struct {
		Spec struct {
			Template struct {
				Spec map[string]any `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &copyOf); err != nil {
		return err
	}
	spec := copyOf.Spec.Template.Spec
	cs, _ := spec["containers"].([]any)
	if len(cs) == 0 {
		return errors.New("the module's Deployment has no container")
	}
	c, _ := cs[0].(obj)
	c["args"] = []any{"purge", "-subject", subject, "-class", class}
	for _, k := range []string{"readinessProbe", "livenessProbe", "startupProbe", "ports", "lifecycle"} {
		delete(c, k)
	}
	spec["containers"] = []any{c}
	delete(spec, "initContainers")
	spec["restartPolicy"] = "Never"
	name := fmt.Sprintf("can-hook-purge-%d", time.Now().UnixNano())
	// Not the module's labels: the pod must not become an endpoint of its Service.
	labels := obj{"app.kubernetes.io/name": "can-hook", "app.kubernetes.io/part-of": "companyinacan", "can.dev/hook": "purge"}
	return h.runJob(ctx, ns, name, kube.Object{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": obj{"name": name, "namespace": ns, "labels": labels},
		"spec": obj{"backoffLimit": 0, "activeDeadlineSeconds": int64(600), "ttlSecondsAfterFinished": 3600,
			"template": obj{"metadata": obj{"labels": labels}, "spec": spec}},
	})
}
