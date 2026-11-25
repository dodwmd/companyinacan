package contract

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Repository hygiene for the CI and release surface (epic E20, specs/024-release-security).
// These tests read .github/ as data; they never contact GitHub.

type wfStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	If   string            `yaml:"if"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

type wfJob struct {
	Name        string         `yaml:"name"`
	If          string         `yaml:"if"`
	Uses        string         `yaml:"uses"`
	Permissions map[string]any `yaml:"permissions"`
	Steps       []wfStep       `yaml:"steps"`
}

type workflow struct {
	Name        string           `yaml:"name"`
	On          any              `yaml:"on"`
	Permissions any              `yaml:"permissions"`
	Jobs        map[string]wfJob `yaml:"jobs"`
	file        string
	raw         string
}

func loadWorkflows(t *testing.T) []workflow {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows found: %v", err)
	}
	var out []workflow
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var w workflow
		if err := yaml.Unmarshal(b, &w); err != nil {
			t.Fatalf("%s: invalid YAML: %v", f, err)
		}
		w.file = filepath.Base(f)
		w.raw = string(b)
		out = append(out, w)
	}
	return out
}

func workflowByFile(t *testing.T, name string) workflow {
	t.Helper()
	for _, w := range loadWorkflows(t) {
		if w.file == name {
			return w
		}
	}
	t.Fatalf("workflow %s missing", name)
	return workflow{}
}

var (
	pinnedUse  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}$`)
	useLine    = regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*(\S+)(.*)$`)
	tagComment = regexp.MustCompile(`#\s*v?\d+(\.\d+)*`)
)

// Every `uses:` must be pinned by a 40-hex commit SHA with the tag recorded in a trailing comment.
func TestWorkflowActionsPinnedBySHA(t *testing.T) {
	for _, w := range loadWorkflows(t) {
		for _, m := range useLine.FindAllStringSubmatch(w.raw, -1) {
			ref, rest := m[1], m[2]
			if strings.HasPrefix(ref, "./") {
				continue
			}
			if !pinnedUse.MatchString(ref) {
				t.Errorf("%s: action %q is not pinned by a 40-hex commit SHA", w.file, ref)
			}
			if !tagComment.MatchString(rest) {
				t.Errorf("%s: action %q has no `# vX.Y.Z` comment recording the tag", w.file, ref)
			}
		}
	}
}

func TestWorkflowsDeclareLeastPrivilegeAndNoPullRequestTarget(t *testing.T) {
	for _, w := range loadWorkflows(t) {
		perms, ok := w.Permissions.(map[string]any)
		if !ok {
			t.Errorf("%s: no top-level `permissions:` map", w.file)
		} else {
			for k, v := range perms {
				if v == "write" {
					t.Errorf("%s: top-level permission %s: write (grant writes per job)", w.file, k)
				}
			}
		}
		if on, ok := w.On.(map[string]any); ok {
			if _, bad := on["pull_request_target"]; bad {
				t.Errorf("%s: uses pull_request_target", w.file)
			}
		}
		// Only the automatic token: no repository or organisation secrets in any workflow.
		for _, m := range regexp.MustCompile(`\$\{\{\s*secrets\.([A-Za-z0-9_]+)`).FindAllStringSubmatch(w.raw, -1) {
			if m[1] != "GITHUB_TOKEN" {
				t.Errorf("%s: uses secret %s (workflows may only use GITHUB_TOKEN)", w.file, m[1])
			}
		}
	}
}

var unsafeExpr = regexp.MustCompile(`\$\{\{[^}]*(github\.event\.|github\.head_ref|github\.ref_name|inputs\.)[^}]*\}\}`)

// lintRun reports attacker-influenced expressions interpolated into a shell script.
func lintRun(script string) []string {
	return unsafeExpr.FindAllString(script, -1)
}

func TestWorkflowNoScriptInjection(t *testing.T) {
	for _, w := range loadWorkflows(t) {
		for jn, j := range w.Jobs {
			for i, s := range j.Steps {
				for _, bad := range lintRun(s.Run) {
					t.Errorf("%s: job %s step %d interpolates %s into a run: script; pass it through env: and quote it", w.file, jn, i+1, bad)
				}
			}
		}
	}
}

func TestScriptInjectionLintCatchesTheClassicCases(t *testing.T) {
	for _, bad := range []string{
		`echo "${{ github.event.pull_request.title }}"`,
		`git checkout ${{ github.head_ref }}`,
		`./x ${{ inputs.version }}`,
		`echo ${{ github.event.issue.body }}`,
	} {
		if len(lintRun(bad)) == 0 {
			t.Errorf("not flagged: %s", bad)
		}
	}
	for _, ok := range []string{`echo "$TITLE"`, `echo ${{ matrix.variant }}`, `echo ${{ github.sha }}`} {
		if len(lintRun(ok)) != 0 {
			t.Errorf("wrongly flagged: %s", ok)
		}
	}
}

// specs/001-project-foundation: branch protection refers to these names; renaming one silently
// un-requires a check.
func TestCIRequiredJobNamesUnchanged(t *testing.T) {
	want := []string{"contract", "dco", "licences", "lint", "unit"}
	w := workflowByFile(t, "ci.yml")
	var got []string
	for key, j := range w.Jobs {
		if j.Name != key {
			t.Errorf("ci.yml job %q has name %q: keep them identical", key, j.Name)
		}
		got = append(got, j.Name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ci.yml jobs = %v, want exactly %v", got, want)
	}
}

func TestProtectScriptRequiresExactlyTheCIJobs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "scripts", "gh-protect-main.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`REQUIRED_CHECKS="([^"]*)"`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("REQUIRED_CHECKS not found in scripts/gh-protect-main.sh")
	}
	got := strings.Fields(m[1])
	sort.Strings(got)
	want := []string{"contract", "dco", "licences", "lint", "unit"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("gh-protect-main.sh requires %v, want %v", got, want)
	}
}

func TestSecurityWorkflowIsNotRequiredAndReadOnlyExceptCodeQL(t *testing.T) {
	w := workflowByFile(t, "security.yml")
	ci := workflowByFile(t, "ci.yml")
	for _, j := range w.Jobs {
		for _, cj := range ci.Jobs {
			if j.Name == cj.Name {
				t.Errorf("security.yml job name %q collides with a required ci.yml check", j.Name)
			}
		}
	}
	for jn, j := range w.Jobs {
		for k, v := range j.Permissions {
			if v == "write" && (jn != "codeql" || k != "security-events") {
				t.Errorf("security.yml job %s has %s: write", jn, k)
			}
		}
	}
	// A deny list that names a permissive licence would reject most of the module graph.
	m := regexp.MustCompile(`deny-licenses:\s*(.+)`).FindStringSubmatch(w.raw)
	if m == nil {
		t.Fatal("dependency review has no deny-licenses policy")
	}
	for _, id := range strings.Split(m[1], ",") {
		id = strings.TrimSpace(id)
		for _, permissive := range []string{"Apache-2.0", "MIT", "BSD-2-Clause", "BSD-3-Clause", "ISC", "0BSD", "Unlicense", "MPL-2.0"} {
			if id == permissive {
				t.Errorf("deny-licenses lists permissive licence %s", id)
			}
		}
		if !strings.Contains(id, "GPL") && id != "EUPL-1.2" {
			t.Errorf("deny-licenses entry %q is not a copyleft licence", id)
		}
	}
}

func TestReleaseWorkflowShape(t *testing.T) {
	w := workflowByFile(t, "release.yml")
	on, ok := w.On.(map[string]any)
	if !ok {
		t.Fatal("release.yml: `on` is not a map")
	}
	for k := range on {
		if k != "push" && k != "workflow_dispatch" {
			t.Errorf("release.yml triggers on %s; only tag push and workflow_dispatch are allowed", k)
		}
	}
	push, _ := on["push"].(map[string]any)
	if _, branches := push["branches"]; branches || push["tags"] == nil {
		t.Errorf("release.yml push trigger must be tags only: %v", push)
	}
	tags, _ := push["tags"].([]any)
	if len(tags) != 1 || tags[0] != "v*.*.*" {
		t.Errorf("release.yml tag filter = %v, want [v*.*.*]", tags)
	}
	disp, _ := on["workflow_dispatch"].(map[string]any)
	inputs, _ := disp["inputs"].(map[string]any)
	dry, _ := inputs["dry_run"].(map[string]any)
	if dry["default"] != true {
		t.Errorf("workflow_dispatch dry_run must default to true, got %v", dry["default"])
	}

	publishJobs := map[string]bool{"publish-images": true, "publish-release": true}
	for jn, j := range w.Jobs {
		writes := false
		for _, v := range j.Permissions {
			if v == "write" {
				writes = true
			}
		}
		if writes && !publishJobs[jn] {
			t.Errorf("release.yml: job %s has write permissions but is not a publish job", jn)
		}
		if publishJobs[jn] {
			if !strings.Contains(j.If, "needs.verify.outputs.publish == 'true'") {
				t.Errorf("release.yml: publish job %s is not gated on the verify job's publish output", jn)
			}
			if !writes {
				t.Errorf("release.yml: publish job %s declares no write permission", jn)
			}
		}
		// Publishing verbs must live only in the gated jobs.
		for i, s := range j.Steps {
			verbs := []string{"--push", "cosign sign", "gh release create", "docker/login-action", "attest-build-provenance"}
			for _, verb := range verbs {
				if (strings.Contains(s.Run, verb) || strings.Contains(s.Uses, verb)) && !publishJobs[jn] {
					t.Errorf("release.yml: job %s step %d publishes (%s) outside a gated publish job", jn, i+1, verb)
				}
			}
		}
	}
	for _, need := range []string{"verify", "build", "images-check", "publish-images", "publish-release"} {
		if _, ok := w.Jobs[need]; !ok {
			t.Errorf("release.yml: job %s missing", need)
		}
	}
	for _, snippet := range []string{"CGO_ENABLED=0", "check-reproducible.sh", "gen-sbom.sh", "licences", "resolve-base-digests.sh", "release-notes.sh", "THIRD_PARTY_LICENSES", "cosign", "attest-build-provenance"} {
		if !strings.Contains(w.raw+readScript(t, "build-release.sh"), snippet) {
			t.Errorf("release pipeline never mentions %s", snippet)
		}
	}
}

func readScript(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "scripts", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The release binaries are the nine cmd/ mains, linux amd64 and arm64 only.
func TestBuildReleaseCoversEveryCommand(t *testing.T) {
	src := readScript(t, "build-release.sh")
	dirs, err := filepath.Glob(filepath.Join(root, "cmd", "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatal("no cmd/ directories")
	}
	m := regexp.MustCompile(`BINARIES="\$\{BINARIES:-([^}]*)\}"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("BINARIES default not found")
	}
	have := map[string]bool{}
	for _, n := range strings.Fields(m[1]) {
		have[n] = true
	}
	for _, d := range dirs {
		if !have[filepath.Base(d)] {
			t.Errorf("cmd/%s is not built by scripts/build-release.sh", filepath.Base(d))
		}
	}
	if !strings.Contains(src, `ARCHES="${ARCHES:-amd64 arm64}"`) || !strings.Contains(src, "GOOS=linux") {
		t.Error("release binaries must be linux/amd64 and linux/arm64 only")
	}
	for _, flag := range []string{"CGO_ENABLED=0", "-trimpath", "core/cli.version", "core/cli.commit", "core/cli.date"} {
		if !strings.Contains(src, flag) {
			t.Errorf("build-release.sh lacks %s", flag)
		}
	}
}

// Every Dockerfile that release.yml builds exists, and every deploy Dockerfile is built by it.
func TestReleaseImagesMatchDeployDockerfiles(t *testing.T) {
	w := workflowByFile(t, "release.yml")
	m := regexp.MustCompile(`IMAGES: "([^"]*)"`).FindStringSubmatch(w.raw)
	if m == nil {
		t.Fatal("IMAGES not found in release.yml")
	}
	built := map[string]bool{}
	for _, e := range strings.Fields(m[1]) {
		dir := e[strings.Index(e, ":")+1:]
		built[dir] = true
		if _, err := os.Stat(filepath.Join(root, dir, "Dockerfile")); err != nil {
			t.Errorf("release.yml builds %s but it has no Dockerfile", dir)
		}
	}
	files, _ := filepath.Glob(filepath.Join(root, "deploy", "*", "Dockerfile"))
	for _, f := range files {
		dir := "deploy/" + filepath.Base(filepath.Dir(f))
		if !built[dir] {
			t.Errorf("%s is not built by release.yml", dir)
		}
	}
	// The matrix of publish-images must list the same image names.
	for _, e := range strings.Fields(m[1]) {
		name := e[:strings.Index(e, ":")]
		if !strings.Contains(w.raw, name+",") && !strings.Contains(w.raw, ", "+name+"]") {
			t.Errorf("image %s is missing from the publish-images matrix", name)
		}
	}
}

func TestDependabotCoversEcosystems(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, ".github", "dependabot.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Version int `yaml:"version"`
		Updates []struct {
			Ecosystem   string   `yaml:"package-ecosystem"`
			Directory   string   `yaml:"directory"`
			Directories []string `yaml:"directories"`
			Schedule    struct {
				Interval string `yaml:"interval"`
			} `yaml:"schedule"`
			Groups map[string]any `yaml:"groups"`
		} `yaml:"updates"`
	}
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var dockerDirs []string
	for _, u := range d.Updates {
		seen[u.Ecosystem] = true
		if u.Schedule.Interval != "weekly" || len(u.Groups) == 0 {
			t.Errorf("dependabot %s must be weekly and grouped", u.Ecosystem)
		}
		if u.Ecosystem == "docker" {
			dockerDirs = append(dockerDirs, u.Directories...)
		}
	}
	for _, e := range []string{"gomod", "github-actions", "docker"} {
		if !seen[e] {
			t.Errorf("dependabot lacks %s", e)
		}
	}
	files, _ := filepath.Glob(filepath.Join(root, "deploy", "*", "Dockerfile"))
	var want []string
	for _, f := range files {
		want = append(want, "/deploy/"+filepath.Base(filepath.Dir(f)))
	}
	sort.Strings(want)
	sort.Strings(dockerDirs)
	if strings.Join(want, ",") != strings.Join(dockerDirs, ",") {
		t.Errorf("dependabot docker directories = %v, want %v", dockerDirs, want)
	}
}

func TestCodeownersAndSecurityPolicyPresent(t *testing.T) {
	co, err := os.ReadFile(filepath.Join(root, ".github", "CODEOWNERS"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(co), "/.github/") || !strings.Contains(string(co), "/scripts/") {
		t.Error("CODEOWNERS must cover /.github/ and /scripts/")
	}
	sec, err := os.ReadFile(filepath.Join(root, "SECURITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"cosign verify", "gh attestation verify", "Supported versions", "SBOM"} {
		if !strings.Contains(string(sec), s) {
			t.Errorf("SECURITY.md lacks %q", s)
		}
	}
}

// The scripts carry their own self-tests (offline, throwaway repositories, fake resolver/gh).
// scripts/check-dco_test.sh already runs in the dco job.
func TestScriptSelfTests(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tests, _ := filepath.Glob(filepath.Join(root, "scripts", "*_test.sh"))
	if len(tests) < 6 {
		t.Fatalf("expected the release script self-tests, found %v", tests)
	}
	for _, s := range tests {
		if strings.HasSuffix(s, "check-dco_test.sh") {
			continue
		}
		s := s
		t.Run(filepath.Base(s), func(t *testing.T) {
			out, err := exec.Command("sh", s).CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
}
