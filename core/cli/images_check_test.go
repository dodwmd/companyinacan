package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/modimages"
	"github.com/dodwmd/companyinacan/modules"
)

// No test asks a real registry: unless a test says otherwise nothing is pullable.
func init() {
	imageListWait, imageListEvery = 3*time.Millisecond, time.Millisecond
	imagePullable = func(context.Context, string) (bool, error) { return false, nil }
}

func usePullable(t *testing.T, f func(string) (bool, error)) *[]string {
	t.Helper()
	old := imagePullable
	t.Cleanup(func() { imagePullable = old })
	asked := &[]string{}
	imagePullable = func(_ context.Context, ref string) (bool, error) {
		*asked = append(*asked, ref)
		return f(ref)
	}
	return asked
}

// imagesDirWith is a directory as `make images ONLY=...` writes it.
func imagesDirWith(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	var entries []string
	for _, n := range names {
		body := "tar-" + n
		if err := os.WriteFile(filepath.Join(dir, n+".tar"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256([]byte(body))
		entries = append(entries, fmt.Sprintf(`%q:{"ref":%q,"file":%q,"sha256":%q}`, n, modimages.Ref(n), n+".tar", hex.EncodeToString(h[:])))
	}
	if err := os.WriteFile(filepath.Join(dir, "images.json"), []byte(`{"schema":1,"images":{`+strings.Join(entries, ",")+"}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func withModules(t *testing.T, dir, modules string) {
	t.Helper()
	p := filepath.Join(dir, "company.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, append(b, []byte("modules:\n"+modules)...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func shipped(spy *stackSpy) []string {
	var out []string
	for _, i := range spy.opts.Images.Upload() {
		out = append(out, i.Name)
	}
	return out
}

func TestUpShipsExactlyTheImagesCompanyYamlEnables(t *testing.T) {
	dir, _ := setup(t)
	withModules(t, dir, "  work: {}\n  people: {}\n  jobs: {enabled: false}\n")
	spy := useStack(t, false, stackResult(), nil)
	code, _, errs := run(t, stackArgs(dir, "--print-secrets")...)
	if code != ExitOK {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	// forms is the default bundle; jobs is disabled.
	want := []string{"can-core", "can-static", "can-forms", "can-work", "can-people"}
	if got := shipped(spy); !slices.Equal(got, want) {
		t.Fatalf("shipped %v, want %v", got, want)
	}
}

func TestUpRefusesAnEnabledModuleWithoutItsImageBeforeProvisioning(t *testing.T) {
	dir, r := setup(t)
	testImagesDir2 = imagesDirWith(t, "can-core", "can-static", "can-forms")
	withModules(t, dir, "  work: {}\n")
	spy := useStack(t, false, stackResult(), nil)
	code, _, errs := run(t, stackArgs(dir)...)
	for _, want := range []string{modimages.Ref("can-work"), "module work", "make images", "nothing has been provisioned"} {
		if code != ExitFailure || !strings.Contains(errs, want) {
			t.Fatalf("code=%d err=%q lacks %q", code, errs, want)
		}
	}
	if len(r.ran) != 0 || spy.called != 0 {
		t.Fatal("something was changed")
	}

	// a published image is pulled by the server; the override skips the question
	asked := usePullable(t, func(string) (bool, error) { return true, nil })
	if code, _, errs := run(t, stackArgs(dir, "--print-secrets")...); code != ExitOK {
		t.Fatalf("pullable: code=%d err=%q", code, errs)
	}
	if !slices.Equal(*asked, []string{modimages.Ref("can-work")}) {
		t.Fatalf("asked the registry for %v", *asked)
	}
	usePullable(t, func(string) (bool, error) { return false, errors.New("no network") })
	if code, _, errs := run(t, stackArgs(dir, "--print-secrets")...); code != ExitFailure || !strings.Contains(errs, "no network") {
		t.Fatalf("unreachable registry: code=%d err=%q", code, errs)
	}
	if code, _, errs := run(t, stackArgs(dir, "--print-secrets", "--skip-image-check")...); code != ExitOK {
		t.Fatalf("skipped: code=%d err=%q", code, errs)
	}
}

// A wrapped module's script hooks run in can-hooks: without it, enabling the module stops backups.
func TestUpNeedsTheHooksImageForScriptHooks(t *testing.T) {
	dir, _ := setup(t)
	testImagesDir2 = imagesDirWith(t, "can-core", "can-static", "can-forms")
	withModules(t, dir, "  wiki: {}\n")
	usePullable(t, func(ref string) (bool, error) { return !strings.Contains(ref, "can-hooks"), nil })
	useStack(t, false, stackResult(), nil)
	if code, _, errs := run(t, stackArgs(dir)...); code != ExitFailure || !strings.Contains(errs, modimages.Ref("can-hooks")) {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestUpFromAReleaseManifestChecksModuleImagesArePublished(t *testing.T) {
	dir, r := setup(t)
	d := "@sha256:" + strings.Repeat("a", 64)
	rel := filepath.Join(t.TempDir(), "release-images.json")
	if err := os.WriteFile(rel, []byte(`{"schema":1,"images":{"can-core":{"ref":"ghcr.io/x/can-core`+d+`"},"can-static":{"ref":"ghcr.io/x/can-static`+d+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	useStack(t, false, stackResult(), nil)
	code, _, errs := run(t, "up", dir, "--release-manifest", rel, "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(errs, modimages.Ref("can-forms")) || len(r.ran) != 0 {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

// nodeExec answers the node's image list.
type nodeExec struct {
	fileExec
	images string
	late   string // listed only from the second query on, as after a fresh import
	asked  int
}

func (n *nodeExec) Run(ctx context.Context, c cluster.Cmd) (string, error) {
	if line := strings.Join(c.Args, " "); strings.Contains(line, "get nodes") {
		if n.asked++; n.asked > 1 {
			return n.images + n.late, nil
		}
		return n.images, nil
	} else if strings.Contains(line, "get pods") {
		return "", nil
	}
	return n.fileExec.Run(ctx, c)
}

func TestApplyRefusesAModuleWhoseImageTheClusterCannotGet(t *testing.T) {
	f := &nodeExec{images: "docker.io/library/can-core:dev\n" + modimages.Ref("can-forms") + "\n"}
	old := newExecutor
	t.Cleanup(func() { newExecutor = old })
	newExecutor = func(*cobra.Command) cluster.Executor { return f }
	dir := writeCompany(t, "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nmodules:\n  forms: {}\n  work: {}\n")
	code, _, errs := run(t, "apply", dir, "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(errs, modimages.Ref("can-work")) || strings.Contains(errs, modimages.Ref("can-forms")) || !strings.Contains(errs, "nothing was changed") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	if len(f.written) != 0 {
		t.Fatalf("applied %v", f.written)
	}
	// loaded just now: the node lists it a little later
	f.asked, f.late = 0, modimages.Ref("can-work")+"\n"
	if code, _, errs := run(t, "apply", dir, "--modules-dir", "../../modules"); code != ExitOK || len(f.written) == 0 {
		t.Fatalf("loaded: code=%d err=%q", code, errs)
	}
}

// The query is run by real kubectl, offline: a container that never got its image (ImagePullBackOff
// after a failed install) must not count as holding it.
func TestPodImagesQueryIgnoresContainersWithoutTheirImage(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("no kubectl")
	}
	list := filepath.Join(t.TempDir(), "pods.json")
	// `patch --local` evaluates the query over one object (and treats a field `items` as a list), so
	// the pods are another field of an object of an unknown kind
	obj := `{"apiVersion":"example.test/v1","kind":"Probe","metadata":{"name":"x"},"pods":[
	 {"status":{"containerStatuses":[{"image":"ghcr.io/o/pulled:1","imageID":"ghcr.io/o/pulled@sha256:ab","state":{"running":{}}}]}},
	 {"status":{"containerStatuses":[{"image":"ghcr.io/o/backoff:1","imageID":"","state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}]}`
	if err := os.WriteFile(list, []byte(obj), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("kubectl", "patch", "--local", "-f", list, "--type=merge", "-p", `{"metadata":{"labels":{"a":"b"}}}`, "-o", strings.Replace(podImagesQuery, ".items[*]", ".pods[*]", 1)) //nolint:gosec // test
	cmd.Env = append(os.Environ(), "KUBECONFIG=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl: %v", err)
	}
	if got := strings.Fields(string(out)); !slices.Equal(got, []string{"ghcr.io/o/pulled:1"}) {
		t.Fatalf("held: %v", got)
	}
}

// A release archive or `go install` has no modules directory: the image check and --images-from
// must see what a checkout sees, or the forms image would not be shipped.
func TestUpWithoutAModulesDirectoryShipsAndChecksTheModuleImages(t *testing.T) {
	checkout, err := filepath.Abs(filepath.Join("..", "..", "modules"))
	if err != nil {
		t.Fatal(err)
	}
	// Every shipped module, built in and from the directory, needs the same images.
	entries, err := os.ReadDir(checkout)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := strings.TrimPrefix(e.Name(), "_")
		want, wantUnknown := modimages.Required(checkout, []string{id})
		got, gotUnknown := modimages.RequiredFS(modules.Shipped, []string{id})
		if !reflect.DeepEqual(got, want) || len(gotUnknown) != len(wantUnknown) {
			t.Errorf("%s: built in %v %v, from the directory %v %v", id, got, gotUnknown, want, wantUnknown)
		}
	}

	dir, _ := setup(t)
	spy := useStack(t, false, stackResult(), nil)
	withModules(t, dir, "  forms: {}\n  work: {}\n")
	held := imagesDirWith(t, "can-core", "can-static", "can-forms", "can-work", "can-people")
	lacking := imagesDirWith(t, "can-core", "can-static", "can-forms")
	t.Chdir(t.TempDir())
	t.Setenv("CAN_MODULES_DIR", "")

	code, out, errs := run(t, "up", dir, "--images-from", held)
	if code != ExitOK || strings.Contains(out+errs, "images not checked") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	got := shipped(spy)
	slices.Sort(got)
	if want := []string{"can-core", "can-forms", "can-static", "can-work"}; !slices.Equal(got, want) {
		t.Fatalf("shipped %v, want %v", got, want)
	}
	code, _, errs = run(t, "up", dir, "--images-from", lacking)
	if code != ExitFailure || !strings.Contains(errs, modimages.Ref("can-work")) || !strings.Contains(errs, "nothing has been provisioned") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}
