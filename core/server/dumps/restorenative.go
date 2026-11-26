package dumps

// In-house modules (specs/043-module-backup-hooks, contracts/hooks.md): the restore hook is
// `<command> <restore> -replace` in a Job shaped like the backup Job (native.go). The hooks are
// read from the manifest this can-core ships, so a module recorded before it had them is restored
// too.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

// native returns the shipped manifest of a module with subcommand hooks, nil for any other.
func (h *KubeHooks) native(module string) (*config.Module, error) {
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
	if err != nil {
		return nil, err
	}
	if m.Hooks.Command == "" {
		return nil, nil
	}
	return m, nil
}

// HasSubcommandRestore reports whether the module is restored by a subcommand of its own binary.
func (h *KubeHooks) HasSubcommandRestore(module string) bool {
	m, err := h.native(module)
	return err == nil && m != nil && m.Hooks.Restore != ""
}

// runNativeRestore runs the restore subcommand. missingBlobs adds -allow-missing-blobs, which is
// only right for files destroyed after the dump was taken.
func (h *KubeHooks) runNativeRestore(ctx context.Context, module string, m *config.Module, missingBlobs bool) error {
	if !commandRe.MatchString(m.Hooks.Command) || !subcommandRe.MatchString(m.Hooks.Restore) {
		return errors.New("hooks.command and hooks.restore do not name a subcommand of a module binary")
	}
	ns := "mod-" + module
	image, uid, err := h.workload(ctx, ns, module)
	if err != nil {
		return err
	}
	claim, err := h.dataClaim(ctx, ns, module)
	if err != nil {
		return err
	}
	if claim == "" {
		return errors.New("the module has no data volume")
	}
	name := fmt.Sprintf("can-hook-restore-%d", time.Now().UnixNano())
	job := h.nativeJob(ctx, name, ns, module, image, claim, uid, m)
	job["metadata"].(obj)["labels"].(obj)["can.dev/hook"] = "restore" // the pod template shares the map
	c := job["spec"].(obj)["template"].(obj)["spec"].(obj)["containers"].([]any)[0].(obj)
	command := []any{m.Hooks.Command, m.Hooks.Restore, "-replace"}
	if missingBlobs {
		command = append(command, "-allow-missing-blobs")
	}
	c["command"] = command
	return h.runNativeJob(ctx, ns, name, job)
}

// runNativeJob runs a subcommand Job and reports what its container said when it failed.
func (h *KubeHooks) runNativeJob(ctx context.Context, ns, name string, job kube.Object) error {
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
