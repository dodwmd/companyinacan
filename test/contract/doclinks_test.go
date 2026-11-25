package contract

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func script(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(root, "scripts", "check-doc-links.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// specs/032-e2e-docs: every relative Markdown link in the documentation must resolve, offline.
func TestDocumentationLinksResolve(t *testing.T) {
	cmd := exec.Command("sh", script(t))
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("broken documentation links:\n%s", out)
	}
}

// The checker must actually catch a broken link and accept a good one (and ignore code blocks, external
// links and anchors), or the test above proves nothing.
func TestDocLinkCheckerCatchesBrokenLinks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("exists.md", "# here\n")
	good := write("good.md", "[a](exists.md) [b](exists.md#top) [c](https://example.invalid/x) [d](#local) [e](mailto:a@b.c)\n```\n[skipped](missing.md)\n```\nand `[inline](missing.md)` code\n")
	bad := write("bad.md", "see [gone](nope/missing.md) and [ok](exists.md)\n")

	run := func(f string) (string, error) {
		cmd := exec.Command("sh", script(t), f)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// The script resolves relative links against each file's own directory, so absolute temp paths work.
	if out, err := run(good); err != nil {
		t.Fatalf("a file with only good links failed: %v\n%s", err, out)
	}
	out, err := run(bad)
	if err == nil {
		t.Fatalf("a broken link was not reported:\n%s", out)
	}
	if !strings.Contains(out, "nope/missing.md") || strings.Contains(out, "exists.md") {
		t.Fatalf("unexpected report:\n%s", out)
	}
}
