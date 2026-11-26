package contract

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/dumps"
)

func hookScripts(t *testing.T, pattern string) []string {
	t.Helper()
	var out []string
	for _, p := range []string{filepath.Join(root, "modules", "*", "hooks", pattern), filepath.Join(root, "test", "integration", "testdata", "modules", "*", "hooks", pattern)} {
		m, err := filepath.Glob(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m...)
	}
	if len(out) == 0 {
		t.Fatal("no hook scripts found")
	}
	return out
}

// QA 4: `curl --data-binary @file` reads the whole file into memory, which kills a hook Job
// limited to 64 MiB on any real dump. `-T file` streams it.
func TestBackupHooksStreamTheirUploads(t *testing.T) {
	for _, p := range hookScripts(t, "*.sh") {
		b, err := os.ReadFile(p) //nolint:gosec // repository file
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.Contains(s, "--data-binary @") || strings.Contains(s, "--data-binary=@") || strings.Contains(s, "-d @") {
			t.Errorf("%s loads a whole file into memory to upload it; use curl -T", p)
		}
		if filepath.Base(p) == "backup.sh" && strings.Contains(s, "/v1/dumps/") && !strings.Contains(s, "-T \"$tmp/") {
			t.Errorf("%s does not upload with curl -T", p)
		}
	}
}

// QA 5: every data path a shipped backup hook defaults to is where the hook Job mounts the
// module volume, and is the path the module's own chart uses.
func TestShippedHookDataPathsAreMounted(t *testing.T) {
	dataDefault := regexp.MustCompile(`(?m)^data="\$\{[A-Z_]+:-(/[^}]*)\}"`)
	chartPath := regexp.MustCompile(`(?m)^\s+mountPath:\s*(\S+)`)
	for _, p := range hookScripts(t, "backup.sh") {
		if strings.Contains(p, "testdata") {
			continue
		}
		dir := filepath.Dir(filepath.Dir(p))
		mod := filepath.Base(dir)
		doc, ps := config.Load(filepath.Join(dir, "module.yaml"))
		if doc == nil {
			t.Fatal(ps)
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			t.Fatal(err)
		}
		script, err := os.ReadFile(p) //nolint:gosec // repository file
		if err != nil {
			t.Fatal(err)
		}
		found := dataDefault.FindStringSubmatch(string(script))
		mounted := dumps.MountsVolume(m.Requires.Database)
		if found == nil {
			if mounted {
				t.Errorf("%s: database %q but the hook names no data directory", mod, m.Requires.Database)
			}
			continue
		}
		if !mounted {
			t.Errorf("%s: the hook reads %s but a %s module gets no volume", mod, found[1], m.Requires.Database)
		}
		if !slices.Contains(dumps.HookDataPaths, found[1]) {
			t.Errorf("%s: the hook reads %s, which the Job does not mount (%v)", mod, found[1], dumps.HookDataPaths)
		}
		values, err := os.ReadFile(filepath.Join(dir, "chart", "values.yaml")) //nolint:gosec // repository file
		if err != nil {
			t.Fatal(err)
		}
		if c := chartPath.FindStringSubmatch(string(values)); c == nil || c[1] != found[1] {
			t.Errorf("%s: the hook reads %s but the chart mounts the volume at %v", mod, found[1], c)
		}
	}
}

// QA 9: the commands the docs give operators must work on the server, where cluster access is
// `sudo k3s kubectl` (the kubeconfig is root's).
func TestDocumentedClusterCommandsRunOnTheServer(t *testing.T) {
	for _, rel := range []string{"docs/backup-restore.md", "docs/make-up.md"} {
		for i, line := range strings.Split(readFile(t, rel), "\n") {
			if strings.Contains(line, "dump-request") && strings.Contains(line, "kubectl") && !strings.Contains(line, "sudo k3s kubectl") {
				t.Errorf("%s:%d: %s", rel, i+1, line)
			}
		}
	}
}
