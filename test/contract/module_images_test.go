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
	// each image is pushed under the version its chart pulls, and a published one is never moved
	for _, want := range []string{`core/modimages/images.txt`, `tags="$tags -t $repo:$own"`, `sh scripts/check-image-versions.sh "ghcr.io/$owner"`, "dev.companyinacan.source=$source", `[ "$state" != published ]`} {
		if !strings.Contains(raw, want) {
			t.Errorf("release.yml lacks %q", want)
		}
	}
	doc := readRepo(t, root, "docs/releasing.md")
	for _, s := range []string{"core/modimages/images.txt", "scripts/set-image-version.sh"} {
		if !strings.Contains(doc, s) {
			t.Errorf("docs/releasing.md does not mention %s", s)
		}
	}
}

// listedVersions reads the version column of the list under base.
func listedVersions(t *testing.T, base string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range strings.Split(readRepo(t, base, "core/modimages/images.txt"), "\n") {
		if f := strings.Fields(l); len(f) == 4 && !strings.HasPrefix(l, "#") {
			out[f[0]] = f[3]
		}
	}
	return out
}

// versionProblems lists every place under base that names another version of an image than the list.
func versionProblems(t *testing.T, base string) []string {
	t.Helper()
	var bad []string
	want := listedVersions(t, base)
	ref := regexp.MustCompile(`ghcr\.io/dodwmd/(can-[a-z]+):([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.]+)?)`)
	for _, f := range glob(t, base, "modules/*/module.yaml", "modules/*/*.md", "modules/*/chart/*.md", "deploy/*/Dockerfile", "deploy/*/README.md", "docs/*.md", "*.md", ".github/workflows/*.yml") {
		for _, m := range ref.FindAllStringSubmatch(readRepo(t, base, f), -1) {
			if v, ours := want[m[1]]; ours && m[2] != v {
				bad = append(bad, f+": "+m[0]+", the list says "+v)
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
		version, listed := want[name]
		if !listed || version == "-" {
			bad = append(bad, f+": "+name+" has no version in core/modimages/images.txt")
			continue
		}
		// A chart without templates installs nothing yet; once it has them it needs the tag.
		_, err := os.Stat(filepath.Join(base, filepath.Dir(f), "templates"))
		if (err == nil || v.Image.Tag != "") && v.Image.Tag != version {
			bad = append(bad, f+": tag "+v.Image.Tag+", the list says "+version)
		}
		chart := filepath.Join(filepath.Dir(f), "Chart.yaml")
		if _, err := os.Stat(filepath.Join(base, chart)); err != nil {
			continue
		}
		if c := readRepo(t, base, chart); !strings.Contains(c, `appVersion: "`+version+`"`) {
			bad = append(bad, chart+": appVersion is not "+version)
		}
	}
	return bad
}

func TestEveryImageVersionAgreesWithTheList(t *testing.T) {
	for name, v := range listedVersions(t, root) {
		local := name == "can-core" || name == "can-static"
		if local != (v == "-") || (!local && !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`).MatchString(v)) {
			t.Errorf("core/modimages/images.txt: %s has version %q", name, v)
		}
	}
	if bad := versionProblems(t, root); len(bad) != 0 {
		t.Fatalf("(scripts/set-image-version.sh):\n%s", strings.Join(bad, "\n"))
	}
}

func TestSetImageVersionChangesEveryPlace(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	tmp := t.TempDir()
	for _, f := range glob(t, root, "core/modimages/images.txt", "modules/*/module.yaml", "modules/*/chart/*.yaml", "modules/*/chart/templates/_helpers.tpl", ".github/workflows/*.yml") {
		dst := filepath.Join(tmp, f)
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(readRepo(t, root, f)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(root, "scripts", "set-image-version.sh")
	for _, name := range []string{"can-forms", "can-work", "can-hooks", "can-board"} {
		if out, err := exec.Command("sh", script, name, "9.8.7-rc.1", tmp).CombinedOutput(); err != nil { //nolint:gosec // repository script
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		if got := listedVersions(t, tmp)[name]; got != "9.8.7-rc.1" {
			t.Fatalf("%s: the list says %s", name, got)
		}
	}
	if bad := versionProblems(t, tmp); len(bad) != 0 {
		t.Fatal(strings.Join(bad, "\n"))
	}
	if got := listedVersions(t, tmp)["can-people"]; got != listedVersions(t, root)["can-people"] {
		t.Errorf("another image changed: %s", got)
	}
	if got := readRepo(t, tmp, "modules/analytics/chart/values.yaml"); got != readRepo(t, root, "modules/analytics/chart/values.yaml") {
		t.Error("an upstream or adapter tag was changed")
	}
	for _, args := range [][]string{{"can-work", "v1.0.0"}, {"can-core", "1.0.0"}, {"can-nothing", "1.0.0"}} {
		if err := exec.Command("sh", script, args[0], args[1], tmp).Run(); err == nil { //nolint:gosec // repository script
			t.Errorf("%v was accepted", args)
		}
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

// A release fails when an image changed since its version was published: the registry is a stand-in
// that answers from files.
func TestReleaseRefusesAChangedImageUnderAPublishedVersion(t *testing.T) {
	for _, tool := range []string{"sh", "go", "jq", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	digest := func(name string) string {
		out, err := exec.Command("sh", filepath.Join(root, "scripts", "image-source-digest.sh"), name, root).Output() //nolint:gosec // repository script
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return strings.TrimSpace(string(out))
	}
	work, hooks := digest("can-work"), digest("can-hooks")
	if len(work) != 64 || work != digest("can-work") || work == hooks || work == digest("can-forms") {
		t.Fatalf("digests: %s %s", work, hooks)
	}

	tmp := t.TempDir()
	// $1 is the reference; a file named after the image answers with its label.
	inspect := filepath.Join(tmp, "inspect")
	body := "#!/bin/sh\nn=${1##*/}; n=${n%%:*}\n[ -f \"" + tmp + "/$n\" ] || { echo \"ERROR: $1: ${NOPE:-not found}\" >&2; exit 1; }\ncat \"" + tmp + "/$n\"\n"
	if err := os.WriteFile(inspect, []byte(body), 0o700); err != nil { //nolint:gosec // test stand-in
		t.Fatal(err)
	}
	publish := func(name, label string) {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(`{"config":{"Labels":{"dev.companyinacan.source":"`+label+`"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(env ...string) (string, error) {
		cmd := exec.Command("sh", filepath.Join(root, "scripts", "check-image-versions.sh"), "ghcr.io/o", root) //nolint:gosec // repository script
		cmd.Env = append(append(os.Environ(), "IMAGE_INSPECT=sh "+inspect), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	publish("can-work", work)
	out, err := check()
	if err != nil || !strings.Contains(out, "can-work published "+work) || !strings.Contains(out, "can-hooks absent "+hooks) || !strings.Contains(out, "can-core release -") {
		t.Fatalf("%v:\n%s", err, out)
	}
	for _, label := range []string{strings.Repeat("0", 64), ""} {
		publish("can-work", label)
		if out, err := check(); err == nil || !strings.Contains(out, "set-image-version.sh can-work") {
			t.Fatalf("label %q: %v:\n%s", label, err, out)
		}
	}
	publish("can-work", work)
	if out, err := check("NOPE=denied"); err == nil || !strings.Contains(out, "cannot tell") {
		t.Fatalf("an unreadable registry must fail: %v:\n%s", err, out)
	}
}

// What image-source-digest.sh assumes: each Go image is one command, and nothing but its binary
// reaches the final image; can-hooks copies nothing.
func TestImageDockerfilesHaveTheShapeTheDigestAssumes(t *testing.T) {
	for _, i := range modimages.All() {
		var lines []string
		for _, l := range strings.Split(readRepo(t, root, i.Dockerfile), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "#") {
				lines = append(lines, l)
			}
		}
		body := strings.Join(lines, "\n")
		cmds := map[string]bool{}
		for _, c := range regexp.MustCompile(`\./cmd/[a-z-]+`).FindAllString(body, -1) {
			cmds[c] = true
		}
		final := body[strings.LastIndex(body, "\nFROM "):]
		if i.Name == "can-hooks" {
			if len(cmds) != 0 || regexp.MustCompile(`(?m)^(COPY|ADD) `).MatchString("\n"+body) {
				t.Errorf("%s now builds or copies files: teach scripts/image-source-digest.sh", i.Dockerfile)
			}
			continue
		}
		if len(cmds) != 1 {
			t.Errorf("%s builds %v, want one command", i.Dockerfile, cmds)
		}
		for _, l := range regexp.MustCompile(`(?m)^(COPY|ADD) .*$`).FindAllString(final, -1) {
			// can-core also ships the module directories; it has no version of its own, so no digest decides
			shipped := i.Name == "can-core" && l == "COPY modules /modules"
			if !shipped && !strings.HasPrefix(l, "COPY --from=build /out/") {
				t.Errorf("%s: %q reaches the image but not its source digest", i.Dockerfile, l)
			}
		}
	}
}
