package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
)

// companyCluster is a cluster that holds ConfigMap can-system/company.
type companyCluster struct {
	t          *testing.T
	data       string
	serverEdit string
	applies    int
}

func (c *companyCluster) Run(_ context.Context, cmd cluster.Cmd) (string, error) {
	line := strings.Join(cmd.Args, " ")
	switch {
	case strings.Contains(line, "get configmap company") && strings.Contains(line, "-o json"):
		if c.data == "" {
			return "", nil
		}
		b, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"uid": "uid-1", "resourceVersion": "7", "annotations": map[string]string{modmgr.ServerEditAnnotation: c.serverEdit}},
			"data":     map[string]string{"company.yaml": c.data},
		})
		return string(b), nil
	case strings.Contains(line, "get configmap company"):
		return c.data, nil
	case strings.Contains(line, "/can-core modules plan"):
		return `{"actions":[]}`, nil
	case strings.HasPrefix(line, "apply -f"):
		b, err := os.ReadFile(cmd.Args[2])
		if err != nil {
			c.t.Fatal(err)
		}
		var cm struct{ Data map[string]string }
		if err := yaml.Unmarshal(b, &cm); err != nil {
			c.t.Fatal(err)
		}
		if v, ok := cm.Data["company.yaml"]; ok {
			c.data = v
			c.applies++
		}
	}
	return "", nil
}

// editInAdmin is what can-core's KubeStore does: new content and the annotation naming it.
func (c *companyCluster) editInAdmin(data string) {
	c.data, c.serverEdit = data, digest([]byte(data))
}

func TestApplyAndPullNeverOverwriteSilently(t *testing.T) {
	c := &companyCluster{t: t}
	old := newExecutor
	t.Cleanup(func() { newExecutor = old })
	newExecutor = func(*cobra.Command) cluster.Executor { return c }

	dir := writeCompany(t, applyCompany)
	path := filepath.Join(dir, "company.yaml")
	apply := func(extra ...string) (int, string) {
		code, _, errs := run(t, append([]string{"apply", dir, "--modules-dir", "../../modules", "--skip-image-check"}, extra...)...)
		return code, errs
	}
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if code, errs := apply(); code != ExitOK || c.data != applyCompany {
		t.Fatalf("first apply: %d %q", code, errs)
	}
	// The admin UI changes the site; the directory's next apply is refused and changes nothing.
	ui := applyCompany + "site: {tagline: from the admin UI}\n"
	c.editInAdmin(ui)
	local := applyCompany + "# my own edit\n"
	write(local)
	if code, errs := apply(); code != ExitFailure || !strings.Contains(errs, "changed in the admin UI") || !strings.Contains(errs, "can pull") || c.data != ui {
		t.Fatalf("apply over a UI edit: %d %q %q", code, errs, c.data)
	}
	// Pulling refuses to lose the local edit; --out writes it aside; --force keeps a copy.
	if code, _, errs := run(t, "pull", dir); code != ExitFailure || !strings.Contains(errs, "never applied") || read() != local {
		t.Fatalf("pull over a local edit: %d %q", code, errs)
	}
	other := filepath.Join(dir, "cluster.yaml")
	if code, _, errs := run(t, "pull", dir, "--out", other); code != ExitOK {
		t.Fatalf("pull --out: %q", errs)
	}
	if b, _ := os.ReadFile(other); string(b) != ui || read() != local {
		t.Fatalf("pull --out wrote %q", b)
	}
	if code, errs := apply(); code != ExitFailure {
		t.Fatalf("--out is not a pull: %q", errs)
	}
	if code, _, errs := run(t, "pull", dir, "--force"); code != ExitOK || read() != ui {
		t.Fatalf("pull --force: %d %q", code, errs)
	}
	if b, _ := os.ReadFile(path + ".bak"); string(b) != local {
		t.Fatalf("no copy of the local edit: %q", b)
	}
	// Pulled: editing and applying works again.
	next := ui + "# after the pull\n"
	write(next)
	if code, errs := apply(); code != ExitOK || c.data != next {
		t.Fatalf("apply after pull: %d %q", code, errs)
	}
	// A pull over an unchanged file needs no --force.
	c.editInAdmin(ui + "# again\n")
	if code, _, errs := run(t, "pull", dir); code != ExitOK || read() != ui+"# again\n" {
		t.Fatalf("pull over an unedited file: %q", errs)
	}
	// --force-company discards on purpose.
	c.editInAdmin(ui + "# third\n")
	if code, errs := apply("--force-company"); code != ExitOK || c.data != ui+"# again\n" {
		t.Fatalf("--force-company: %d %q", code, errs)
	}
}

// A directory that never saw the cluster still refuses to overwrite an edit made in the admin UI, and
// still applies over what the CLI itself wrote.
func TestApplyFromAFreshDirectory(t *testing.T) {
	c := &companyCluster{t: t}
	old := newExecutor
	t.Cleanup(func() { newExecutor = old })
	newExecutor = func(*cobra.Command) cluster.Executor { return c }
	dir := writeCompany(t, applyCompany)
	args := []string{"apply", dir, "--modules-dir", "../../modules", "--skip-image-check"}

	c.editInAdmin(applyCompany + "# ui\n")
	if code, _, errs := run(t, args...); code != ExitFailure || c.applies != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	c.data, c.serverEdit = applyCompany+"# another machine\n", ""
	if code, _, errs := run(t, args...); code != ExitOK || c.data != applyCompany {
		t.Fatalf("%d %q", code, errs)
	}
}
