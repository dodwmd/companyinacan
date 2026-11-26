package contract

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/launch"
)

// release.yml produces deploy/release-images.json (docs/make-up.md): its own step is run here on
// the digest files publish-images uploads, and `can up` must accept the result.
func TestReleaseWorkflowWritesTheImageManifest(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	steps := workflowByFile(t, "release.yml").Jobs["publish-release"].Steps
	write := slices.IndexFunc(steps, func(s wfStep) bool { return strings.Contains(s.Run, "scripts/gen-release-images.sh") })
	sign := slices.IndexFunc(steps, func(s wfStep) bool { return strings.Contains(s.Run, "cosign sign-blob") })
	create := slices.IndexFunc(steps, func(s wfStep) bool { return strings.Contains(s.Run, "gh release create") })
	if write < 0 || write > sign || sign > create {
		t.Fatalf("publish-release: write the manifest (%d), then sign the checksums (%d), then release (%d)", write, sign, create)
	}

	dir := t.TempDir()
	for _, d := range []string{"digests", "out/dist", "scripts"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	put := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("scripts/gen-release-images.sh", readScript(t, "gen-release-images.sh"))
	sums := ""
	for _, a := range []string{"amd64", "arm64"} {
		sums += strings.Repeat("ab", 32) + "  can_v1.2.3_linux_" + a + ".tar.gz\n"
	}
	put("out/dist/SHA256SUMS", sums)
	digest := "sha256:" + strings.Repeat("0123456789abcdef", 4)
	for _, n := range []string{"can-core", "can-static", "can-forms"} {
		put("digests/digest-"+n+".txt", "ghcr.io/example/"+n+"@"+digest+"\n")
	}
	cmd := exec.Command("bash", "-c", steps[write].Run) //nolint:gosec // this repository's workflow
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "VERSION=v1.2.3", "GITHUB_REPOSITORY_OWNER=Example")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	set, err := launch.LoadRelease(filepath.Join(dir, "out", "dist", "release-images.json"))
	if err != nil {
		t.Fatal(err)
	}
	if set.Version != "v1.2.3" || len(set.Images) != 3 || set.Images["can-forms"].Ref != "ghcr.io/example/can-forms@"+digest {
		t.Fatalf("%+v", set)
	}
	for _, p := range []string{"linux/amd64", "linux/arm64"} {
		if a := set.CLI[p]; a.SHA256 != strings.Repeat("ab", 32) || !strings.HasPrefix(a.File, "can_v1.2.3_linux_") {
			t.Fatalf("%s: %+v", p, set.CLI)
		}
	}
	all, err := os.ReadFile(filepath.Join(dir, "out", "dist", "SHA256SUMS")) //nolint:gosec // test
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(all)), "release-images.json") {
		t.Fatalf("not in the checksums: %q %v", all, err)
	}
}
