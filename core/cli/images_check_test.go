package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
	"github.com/dodwmd/companyinacan/core/modimages"
)

// No test asks a real registry: unless a test says otherwise nothing is pullable.
func init() {
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
}

func (n *nodeExec) Run(ctx context.Context, c cluster.Cmd) (string, error) {
	if line := strings.Join(c.Args, " "); strings.Contains(line, "get nodes") {
		return n.images, nil
	} else if strings.Contains(line, "get pods") {
		return "", nil
	}
	return n.fileExec.Run(ctx, c)
}

func TestApplyRefusesAModuleWhoseImageTheClusterCannotGet(t *testing.T) {
	f := &nodeExec{images: "docker.io/library/can-core:dev\nghcr.io/dodwmd/can-forms:" + modimages.Version() + "\n"}
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
	f.images += modimages.Ref("can-work") + "\n"
	if code, _, errs := run(t, "apply", dir, "--modules-dir", "../../modules"); code != ExitOK || len(f.written) == 0 {
		t.Fatalf("loaded: code=%d err=%q", code, errs)
	}
}
