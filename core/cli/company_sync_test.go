package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/launch"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/server/modmgr"
)

// companyCluster is a cluster that holds ConfigMap can-system/company.
type companyCluster struct {
	// uid is the ConfigMap's; afterApply runs once a document was written.
	uid        string
	afterApply func()
	calls      []string
	t          *testing.T
	data       string
	serverEdit string
	applies    int
}

func (c *companyCluster) Run(_ context.Context, cmd cluster.Cmd) (string, error) {
	line := strings.Join(cmd.Args, " ")
	c.calls = append(c.calls, line)
	switch {
	case strings.Contains(line, "get configmap company") && strings.Contains(line, "-o json"):
		if c.data == "" {
			return "", nil
		}
		b, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"uid": cmpOr(c.uid, "uid-1"), "resourceVersion": "7", "annotations": map[string]string{modmgr.ServerEditAnnotation: c.serverEdit}},
			"data":     map[string]string{"company.yaml": c.data},
		})
		return string(b), nil
	case strings.Contains(line, "get configmap company"):
		return c.data, nil
	case strings.Contains(line, "/can-core modules plan"):
		return `{"actions":[]}`, nil
	case strings.Contains(line, "apply -f"):
		b, err := os.ReadFile(cmd.Args[slices.Index(cmd.Args, "-f")+1])
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
			if c.afterApply != nil {
				c.afterApply()
			}
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

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func useCluster(t *testing.T, c *companyCluster) {
	t.Helper()
	old := newExecutor
	t.Cleanup(func() { newExecutor = old })
	newExecutor = func(*cobra.Command) cluster.Executor { return c }
}

// An edit made in the admin UI right after the write is not taken for what apply wrote.
func TestApplyRecordsWhatItWrote(t *testing.T) {
	c := &companyCluster{t: t}
	useCluster(t, c)
	dir := writeCompany(t, applyCompany)
	args := []string{"apply", dir, "--modules-dir", "../../modules", "--skip-image-check"}
	c.data = applyCompany + "# before\n"
	ui := applyCompany + "# edited while apply was finishing\n"
	c.afterApply = func() { c.editInAdmin(ui) }
	if code, _, errs := run(t, args...); code != ExitOK {
		t.Fatal(errs)
	}
	c.afterApply = nil
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), []byte(applyCompany+"# next\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, args...); code != ExitFailure || c.data != ui {
		t.Fatalf("the next apply overwrote it: %d %q %q", code, errs, c.data)
	}
}

func pullDir(t *testing.T, content string, st provider.LaunchState) (dir, path string) {
	t.Helper()
	dir = writeCompany(t, content)
	if err := provider.NewStore(dir).Mutate(func(s *provider.State) error { s.Launch = &st; return nil }); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, "company.yaml")
}

func TestPullAfterUpAddedTheDefaultBundle(t *testing.T) {
	local := "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\n"
	bundled, added, err := launch.WithDefaultBundle([]byte(local))
	if err != nil || !added {
		t.Fatal(added, err)
	}
	c := &companyCluster{t: t}
	useCluster(t, c)
	c.editInAdmin(string(bundled) + "site: {tagline: from the UI}\n")
	dir, path := pullDir(t, local, provider.LaunchState{CompanySHA256: digest(bundled), CompanyBases: map[string]string{"uid-1": digest(bundled)}})
	if code, _, errs := run(t, "pull", dir); code != ExitOK {
		t.Fatalf("pull demanded --force for what `can up` added: %q", errs)
	}
	if b, _ := os.ReadFile(path); string(b) != c.data {
		t.Fatalf("%q", b)
	}
}

func TestPullDoesNotOverwriteFromAnotherCluster(t *testing.T) {
	c := &companyCluster{t: t, uid: "uid-other", data: applyCompany + "# another cluster\n"}
	useCluster(t, c)
	// Unedited since `can up`, so only the cluster's identity speaks against overwriting.
	dir, path := pullDir(t, applyCompany, provider.LaunchState{CompanySHA256: digest([]byte(applyCompany)), CompanyBases: map[string]string{"uid-1": digest([]byte(applyCompany))}})
	if code, _, errs := run(t, "pull", dir); code != ExitFailure || !strings.Contains(errs, "another cluster") {
		t.Fatalf("%d %q", code, errs)
	}
	if err := os.WriteFile(path+".bak", []byte("an earlier copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "pull", dir, "--force"); code != ExitOK {
		t.Fatal(errs)
	}
	for name, want := range map[string]string{path: c.data, path + ".bak": "an earlier copy", path + ".bak.1": applyCompany} {
		if b, _ := os.ReadFile(name); string(b) != want {
			t.Errorf("%s = %q, want %q", name, b, want)
		}
	}
}

func TestPullKeepsModeAndSymlink(t *testing.T) {
	c := &companyCluster{t: t}
	useCluster(t, c)
	c.editInAdmin(applyCompany + "# ui\n")
	dir, path := pullDir(t, applyCompany, provider.LaunchState{CompanySHA256: digest([]byte(applyCompany))})
	real := filepath.Join(t.TempDir(), "real.yaml")
	if err := os.Rename(path, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(real, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "pull", dir); code != ExitOK {
		t.Fatal(errs)
	}
	link, err := os.Lstat(path)
	if err != nil || link.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink is gone: %v %v", link.Mode(), err)
	}
	st, err := os.Stat(real)
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v %v", st.Mode(), err)
	}
	if b, _ := os.ReadFile(real); string(b) != c.data {
		t.Fatalf("%q", b)
	}
}

// With a server of `can up`, pull reads that one: its kubeconfig, through the tunnel, whatever
// kubectl's current context is.
func TestPullReadsTheServerOfCanUp(t *testing.T) {
	c := &companyCluster{t: t, data: applyCompany}
	useCluster(t, c)
	oldAccess := lifecycle.access
	t.Cleanup(func() { lifecycle.access = oldAccess })
	opened, closed := 0, 0
	lifecycle.access = func(context.Context, provider.Target) (func(), error) {
		opened++
		return func() { closed++ }, nil
	}
	dir, _ := pullDir(t, applyCompany, provider.LaunchState{})
	if err := provider.NewStore(dir).Mutate(func(s *provider.State) error {
		s.Server = &provider.ServerState{IP: "203.0.113.7"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kubeconfig := provider.KubeconfigPath(dir)
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "pull", dir); code != ExitOK {
		t.Fatal(errs)
	}
	if opened != 1 || closed != 1 || !strings.HasPrefix(c.calls[0], "--kubeconfig "+kubeconfig+" --context a ") {
		t.Fatalf("opened %d closed %d, ran %v", opened, closed, c.calls)
	}
	c.calls = nil
	if code, _, errs := run(t, "pull", dir, "--context", "k3d-x"); code != ExitOK || opened != 1 || !strings.HasPrefix(c.calls[0], "--context k3d-x ") {
		t.Fatalf("--context: %q %v", errs, c.calls)
	}
}
