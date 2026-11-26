//go:build integration

package integration

// Epic E06d on the make-up cluster: the images `make images` builds are the ones the charts pull,
// `can apply` refuses a module whose image the cluster cannot get, and a first install that fails
// for want of one does not stop backups. Called by TestMakeUpPhasesOnK3d.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/modimages"
)

// failedInstallOnK3d enables people, whose image was deliberately not imported. company is the
// document the cluster runs.
func failedInstallOnK3d(t *testing.T, company []byte) {
	t.Helper()
	dir := t.TempDir()
	var c map[string]any
	if err := yaml.Unmarshal(company, &c); err != nil {
		t.Fatal(err)
	}
	mods, ok := c["modules"].(map[string]any)
	if !ok {
		t.Fatalf("no modules in:\n%s", company)
	}
	mods["people"] = map[string]any{}
	doc, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), doc, 0o600); err != nil {
		t.Fatal(err)
	}
	apply := func(extra ...string) (string, error) {
		args := append([]string{"run", "./cmd/can", "apply", dir, "--modules-dir", "modules", "--context", kubeContext()}, extra...)
		cmd := exec.Command("go", args...) //nolint:gosec // test helper
		cmd.Dir = repoRoot(t)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := apply()
	if err == nil || !strings.Contains(out, "neither loaded nor pullable") || !strings.Contains(out, modimages.Ref("can-people")) {
		t.Fatalf("can apply must refuse people, whose image the cluster does not have: %v\n%s", err, out)
	}
	if strings.Contains(out, modimages.Ref("can-work")) || strings.Contains(out, modimages.Ref("can-forms")) {
		t.Fatalf("the imported images were not seen on the node:\n%s", out)
	}
	if out := mustKubectl(t, "-n", "can-system", "get", "configmap", "company", "-o", `jsonpath={.data.company\.yaml}`); strings.Contains(out, "people") {
		t.Fatalf("a refused apply changed company.yaml:\n%s", out)
	}
	// forced through, the install fails and leaves its volume
	if out, err := apply("--skip-image-check"); err != nil {
		t.Fatalf("can apply --skip-image-check: %v\n%s", err, out)
	}
	eventually(t, 10*time.Minute, "the people install to be recorded as failed", func() (bool, string) {
		out, err := kubectl("-n", "can-system", "exec", "deploy/can-core", "--", "/can-core", "modules", "problems")
		return err == nil && strings.Contains(out, "people"), out
	})
	// A second apply is refused too: the pod that could not pull the image does not hold it.
	if out, err := apply(); err == nil || !strings.Contains(out, modimages.Ref("can-people")) {
		t.Fatalf("can apply after the failed install: %v\n%s", err, out)
	}
	if got := mustKubectl(t, "-n", "mod-people", "get", "pvc", "-o", "name"); !strings.Contains(got, "people-data") {
		t.Fatalf("the failed install left no volume, so this proves nothing: %q", got)
	}
	run, err := kubectl("-n", "can-system", "exec", "deploy/can-core", "--", "/can-core", "dump-request", "--wait", "8m")
	if err != nil {
		logs, _ := kubectl("-n", "can-system", "logs", "deploy/can-core", "--tail=60")
		t.Fatalf("a failed first install stopped the backup: %v\n%s\n%s", err, run, logs)
	}
	if !strings.Contains(run, `"people" was NOT dumped`) || !strings.Contains(run, "first install never succeeded") {
		t.Fatalf("the skipped module is not reported:\n%s", run)
	}
	if got := onNode(t, "cat /var/lib/can/dumps/DUMPED"); !strings.Contains(got, "work sqlite") || strings.Contains(got, "people") {
		t.Errorf("DUMPED: %q", got)
	}
}
