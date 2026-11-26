package contract

// Epic E06d: one version and one list of in-house images (core/modimages) for the charts, the
// manifests, `make images`, the workflows and the docs.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/modimages"
)

func readRepo(t *testing.T, base, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(base, rel)) //nolint:gosec // repository file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func glob(t *testing.T, base string, patterns ...string) []string {
	t.Helper()
	var out []string
	for _, p := range patterns {
		m, err := filepath.Glob(filepath.Join(base, p))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range m {
			rel, err := filepath.Rel(base, f)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, rel)
		}
	}
	return out
}

func TestImageListMatchesTheDockerfiles(t *testing.T) {
	var listed []string
	for _, i := range modimages.All() {
		listed = append(listed, i.Dockerfile)
		if _, err := os.Stat(filepath.Join(root, i.Context)); err != nil {
			t.Error(err)
		}
	}
	have := glob(t, root, "deploy/*/Dockerfile")
	sort.Strings(listed)
	sort.Strings(have)
	if !slices.Equal(listed, have) {
		t.Fatalf("core/modimages/images.txt lists %v, deploy/ holds %v", listed, have)
	}
}

func TestReleaseWorkflowBuildsTheImageList(t *testing.T) {
	var w struct {
		Env  map[string]string `yaml:"env"`
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Image []string `yaml:"image"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	raw := readRepo(t, root, ".github/workflows/release.yml")
	if err := yaml.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, i := range modimages.All() {
		want = append(want, i.Name+":"+filepath.Dir(i.Dockerfile))
	}
	if got := strings.Fields(w.Env["IMAGES"]); !slices.Equal(got, want) {
		t.Errorf("release.yml IMAGES %v, want %v", got, want)
	}
	if got := w.Jobs["publish-images"].Strategy.Matrix.Image; !slices.Equal(got, modimages.Names()) {
		t.Errorf("release.yml publishes %v, want %v", got, modimages.Names())
	}
	if !strings.Contains(raw, `sh scripts/check-image-version.sh "$VERSION"`) {
		t.Error("release.yml must refuse to publish a tag the charts do not name")
	}
	// the images are pushed under the tag without the "v" too: what the charts name
	if !strings.Contains(raw, `-t $repo:${VERSION#v}`) {
		t.Error("release.yml no longer pushes the X.Y.Z tag the charts name")
	}
	doc := readRepo(t, root, "docs/releasing.md")
	for _, s := range []string{"core/modimages/version.txt", "scripts/set-image-version.sh"} {
		if !strings.Contains(doc, s) {
			t.Errorf("docs/releasing.md does not mention %s", s)
		}
	}
}

// versionProblems lists every place under base that names another version than want.
func versionProblems(t *testing.T, base, want string) []string {
	t.Helper()
	var bad []string
	names := modimages.Names()
	ref := regexp.MustCompile(`ghcr\.io/dodwmd/((?:` + strings.Join(names, "|") + `)|\$[a-z]+):([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.]+)?)`)
	for _, f := range glob(t, base, "modules/*/module.yaml", "modules/*/*.md", "modules/*/chart/*.md", "deploy/*/Dockerfile", "deploy/*/README.md", "docs/*.md", "*.md", ".github/workflows/*.yml") {
		for _, m := range ref.FindAllStringSubmatch(readRepo(t, base, f), -1) {
			if m[2] != want {
				bad = append(bad, f+": "+m[0])
			}
		}
	}
	for _, f := range glob(t, base, "modules/*/chart/values.yaml") {
		var v struct {
			Image struct{ Repository, Tag, Digest string }
		}
		if err := yaml.Unmarshal([]byte(readRepo(t, base, f)), &v); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		name, ours := strings.CutPrefix(v.Image.Repository, modimages.Registry+"/")
		if !ours {
			continue
		}
		if !slices.Contains(names, name) {
			bad = append(bad, f+": "+name+" is not built (core/modimages/images.txt)")
		}
		// A chart without templates installs nothing yet; once it has them it needs the tag.
		_, err := os.Stat(filepath.Join(base, filepath.Dir(f), "templates"))
		if (err == nil || v.Image.Tag != "") && v.Image.Tag != want {
			bad = append(bad, f+": tag "+v.Image.Tag)
		}
		chart := filepath.Join(filepath.Dir(f), "Chart.yaml")
		if _, err := os.Stat(filepath.Join(base, chart)); err != nil {
			continue
		}
		if c := readRepo(t, base, chart); !strings.Contains(c, `appVersion: "`+want+`"`) {
			bad = append(bad, chart+": appVersion")
		}
	}
	return bad
}

func TestEveryImageVersionIsTheSame(t *testing.T) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`).MatchString(modimages.Version()) {
		t.Fatalf("core/modimages/version.txt: %q", modimages.Version())
	}
	if bad := versionProblems(t, root, modimages.Version()); len(bad) != 0 {
		t.Fatalf("not version %s (scripts/set-image-version.sh %[1]s):\n%s", modimages.Version(), strings.Join(bad, "\n"))
	}
}

func TestSetImageVersionChangesEveryPlace(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	tmp := t.TempDir()
	for _, f := range glob(t, root, "core/modimages/*.txt", "modules/*/module.yaml", "modules/*/chart/*.yaml", "modules/*/chart/templates/_helpers.tpl", ".github/workflows/*.yml") {
		dst := filepath.Join(tmp, f)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(readRepo(t, root, f)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(root, "scripts", "set-image-version.sh")
	if out, err := exec.Command("sh", script, "9.8.7-rc.1", tmp).CombinedOutput(); err != nil { //nolint:gosec // repository script
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.TrimSpace(readRepo(t, tmp, "core/modimages/version.txt")); got != "9.8.7-rc.1" {
		t.Fatal(got)
	}
	if bad := versionProblems(t, tmp, "9.8.7-rc.1"); len(bad) != 0 {
		t.Fatal(strings.Join(bad, "\n"))
	}
	// upstream images keep their versions
	if got := readRepo(t, tmp, "modules/analytics/chart/values.yaml"); got != readRepo(t, root, "modules/analytics/chart/values.yaml") {
		t.Error("an upstream or adapter tag was changed")
	}
	check := filepath.Join(root, "scripts", "check-image-version.sh")
	if out, err := exec.Command("sh", check, "v9.8.7-rc.1", tmp).CombinedOutput(); err != nil { //nolint:gosec // repository script
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := exec.Command("sh", check, "v9.8.7", tmp).CombinedOutput(); err == nil { //nolint:gosec // repository script
		t.Fatalf("a different tag was accepted: %s", out)
	}
	if err := exec.Command("sh", script, "v1.0.0", tmp).Run(); err == nil { //nolint:gosec // repository script
		t.Error("a leading v was accepted")
	}
}

// `make images` (scripts/build-images.sh) with a stand-in container engine: every listed image is
// built from its Dockerfile and recorded under the reference the charts name.
func TestMakeImagesBuildsEveryImage(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum")
	}
	tmp := t.TempDir()
	engine := filepath.Join(tmp, "fake-engine")
	body := `#!/bin/sh
echo "$@" >>"` + tmp + `/calls"
case "$1" in
pull) ;;
image) echo "x@sha256:` + strings.Repeat("a", 64) + `" ;;
build) ;;
save) while [ $# -gt 1 ]; do [ "$1" = -o ] && echo tar >"$2"; shift; done ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(engine, []byte(body), 0o700); err != nil { //nolint:gosec // test stand-in
		t.Fatal(err)
	}
	run := func(only string) ([]byte, error) {
		cmd := exec.Command("sh", "scripts/build-images.sh", filepath.Join(tmp, "out")) //nolint:gosec // repository script
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "ENGINE="+engine, "ONLY="+only)
		return cmd.CombinedOutput()
	}
	if out, err := run(""); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	index := readRepo(t, tmp, "out/images.json")
	calls := readRepo(t, tmp, "calls")
	for _, i := range modimages.All() {
		if !strings.Contains(index, `"`+i.Name+`":{"ref":"`+modimages.Ref(i.Name)+`"`) {
			t.Errorf("images.json lacks %s as %s:\n%s", i.Name, modimages.Ref(i.Name), index)
		}
		if !strings.Contains(calls, "/"+i.Dockerfile+" ") {
			t.Errorf("%s was not built", i.Dockerfile)
		}
	}
	if out, err := run("can-core can-static can-work"); err != nil || strings.Contains(readRepo(t, tmp, "out/images.json"), "can-forms") {
		t.Fatalf("ONLY: %v: %s", err, out)
	}
	if out, err := run("can-work"); err == nil {
		t.Fatalf("without can-core: %s", out)
	}
	if out, err := run("can-core can-static can-nothing"); err == nil {
		t.Fatalf("unknown image: %s", out)
	}
}
