package contract

// E08d (specs/043-module-backup-hooks): every in-house module has a consistent dump and restore.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modbackup"
)

func allModules(t *testing.T) map[string]*config.Module {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "modules", "*", "module.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatal("no modules", err)
	}
	out := map[string]*config.Module{}
	for _, p := range paths {
		dir := filepath.Base(filepath.Dir(p))
		if strings.HasPrefix(dir, "_") {
			continue
		}
		doc, ps := config.Load(p)
		if doc == nil {
			t.Fatal(ps)
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			t.Fatal(err)
		}
		out[dir] = m
	}
	return out
}

func TestInHouseModulesHaveBackupAndRestoreSubcommands(t *testing.T) {
	n := 0
	for id, m := range allModules(t) {
		if m.Provider.Type != "inhouse" || m.Requires.Database != "sqlite" {
			continue
		}
		n++
		if m.Hooks.Backup != "backup" || m.Hooks.Restore != "restore" || m.Hooks.Command != "/"+m.Provider.Name || m.Hooks.Image != "" {
			t.Errorf("%s: hooks %+v, want command /%s with backup and restore and no image", id, m.Hooks, m.Provider.Name)
		}
		if why := m.BackupHookUnrunnable(); why != "" {
			t.Errorf("%s: %s", id, why)
		}
		// The binary at that path in the image dispatches to modbackup.
		main := readFile(t, "cmd/"+m.Provider.Name+"/main.go")
		if !strings.Contains(main, `modbackup.Main("`+m.Provider.Name+`"`) || !modbackup.Handles([]string{m.Hooks.Backup}) || !modbackup.Handles([]string{m.Hooks.Restore}) {
			t.Errorf("%s: cmd/%s does not run the subcommands", id, m.Provider.Name)
		}
		dockerfiles, err := filepath.Glob(filepath.Join(root, "deploy", "*", "Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, d := range dockerfiles {
			b, err := os.ReadFile(d) //nolint:gosec // repository file
			if err != nil {
				t.Fatal(err)
			}
			found = found || strings.Contains(string(b), `ENTRYPOINT ["`+m.Hooks.Command+`"]`)
		}
		if !found {
			t.Errorf("%s: no image has the binary %s", id, m.Hooks.Command)
		}
		// The job mounts the volume at /data, where the chart puts it.
		values := readFile(t, "modules/"+id+"/chart/values.yaml")
		if !regexp.MustCompile(`(?m)^\s+mountPath: /data\b`).MatchString(values) {
			t.Errorf("%s: the chart does not mount its volume at /data", id)
		}
	}
	if n < 10 {
		t.Fatalf("only %d in-house modules found", n)
	}
}

// Every reference to the hooks image is the same one, with a tag the release pipeline pushes.
func TestHooksImageReferencesAreConsistent(t *testing.T) {
	var ref string
	for id, m := range allModules(t) {
		if m.Hooks.Image == "" {
			continue
		}
		if !strings.HasPrefix(m.Hooks.Image, config.UnpublishedHooksImage) {
			t.Errorf("%s: unexpected hooks image %s", id, m.Hooks.Image)
		}
		if ref == "" {
			ref = m.Hooks.Image
		} else if m.Hooks.Image != ref {
			t.Errorf("%s: %s differs from %s", id, m.Hooks.Image, ref)
		}
	}
	tag := strings.TrimPrefix(ref, config.UnpublishedHooksImage)
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(tag) {
		t.Fatalf("hooks image tag %q", tag)
	}
	// A release vX.Y.Z pushes both vX.Y.Z and X.Y.Z, the form manifests and charts use.
	release := readFile(t, ".github/workflows/release.yml")
	if !strings.Contains(release, `-t $repo:${VERSION#v}`) {
		t.Error("release.yml does not push the unprefixed version tag the manifests and charts name")
	}
}
