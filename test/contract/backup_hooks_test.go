package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
