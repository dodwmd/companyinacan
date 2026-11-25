package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
)

type fakeExec struct {
	calls []string
	out   map[string]string
	err   map[string]error
}

func (f *fakeExec) Run(_ context.Context, c cluster.Cmd) (string, error) {
	f.calls = append(f.calls, c.String())
	return f.out[c.String()], f.err[c.String()]
}

// stubDev installs fake tool lookup and executor for the duration of a test.
func stubDev(t *testing.T, present bool, f *fakeExec) {
	t.Helper()
	oldLook, oldEx := lookPath, newExecutor
	t.Cleanup(func() { lookPath, newExecutor = oldLook, oldEx })
	lookPath = func(n string) (string, error) {
		if present {
			return "/bin/" + n, nil
		}
		return "", os.ErrNotExist
	}
	newExecutor = func(*cobra.Command) cluster.Executor { return f }
}

const listKey = "k3d cluster list companyinacan-dev --output json"

func TestDevWithoutSubcommandIsUsageError(t *testing.T) {
	code, _, errs := run(t, "dev")
	if code != ExitUsage || !strings.Contains(errs, "subcommand") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestDevUpDryRunPrintsPlanAndRunsNothing(t *testing.T) {
	f := &fakeExec{}
	stubDev(t, false, f) // even with no tools installed
	code, out, _ := run(t, "dev", "up", "--dry-run")
	if code != ExitOK || len(f.calls) != 0 {
		t.Fatalf("code=%d calls=%v", code, f.calls)
	}
	if !strings.HasPrefix(out, "k3d cluster create companyinacan-dev --config dev/k3d.yaml\n") || !strings.Contains(out, "apply -k deploy/base") {
		t.Fatalf("plan:\n%s", out)
	}
}

func TestDevUpFailsWithoutPrerequisites(t *testing.T) {
	f := &fakeExec{}
	stubDev(t, false, f)
	code, _, errs := run(t, "dev", "up")
	if code != ExitFailure || len(f.calls) != 0 || !strings.Contains(errs, "k3d") {
		t.Fatalf("code=%d calls=%v err=%q", code, f.calls, errs)
	}
}

func TestDevUpRunsPlanFromRepoRoot(t *testing.T) {
	f := &fakeExec{err: map[string]error{listKey: errors.New("No nodes found for given cluster")}}
	stubDev(t, true, f)
	t.Chdir("../..")
	code, out, errs := run(t, "dev", "up")
	if code != ExitOK || !strings.Contains(out, "is up") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	if len(f.calls) < 3 || f.calls[1] != "k3d cluster create companyinacan-dev --config dev/k3d.yaml" {
		t.Fatalf("calls=%v", f.calls)
	}
}

func TestDevUpFailsOutsideRepoRoot(t *testing.T) {
	f := &fakeExec{}
	stubDev(t, true, f)
	t.Chdir(t.TempDir())
	code, _, errs := run(t, "dev", "up")
	if code != ExitFailure || !strings.Contains(errs, "dev/k3d.yaml") || len(f.calls) != 0 {
		t.Fatalf("code=%d err=%q calls=%v", code, errs, f.calls)
	}
}

func TestDevUpPropagatesCommandFailure(t *testing.T) {
	f := &fakeExec{err: map[string]error{listKey: errors.New("No nodes found"),
		"k3d cluster create companyinacan-dev --config dev/k3d.yaml": errors.New("docker daemon not running")}}
	stubDev(t, true, f)
	t.Chdir("../..")
	code, _, errs := run(t, "dev", "up")
	if code != ExitFailure || !strings.Contains(errs, "docker daemon not running") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestDevBadNameIsUsageError(t *testing.T) {
	stubDev(t, true, &fakeExec{})
	code, _, errs := run(t, "dev", "down", "--name", "Bad Name")
	if code != ExitUsage || !strings.Contains(errs, "invalid cluster name") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestDevDown(t *testing.T) {
	f := &fakeExec{out: map[string]string{listKey: `[{"name":"companyinacan-dev","serversCount":1,"serversRunning":1}]`}}
	stubDev(t, true, f)
	code, _, _ := run(t, "dev", "down")
	if code != ExitOK || len(f.calls) != 2 || f.calls[1] != "k3d cluster delete companyinacan-dev" {
		t.Fatalf("code=%d calls=%v", code, f.calls)
	}
	f.calls = nil
	code, out, _ := run(t, "dev", "down", "--dry-run")
	if code != ExitOK || len(f.calls) != 0 || !strings.Contains(out, "k3d cluster delete") {
		t.Fatalf("dry-run code=%d out=%q calls=%v", code, out, f.calls)
	}
}

func TestDevStatusExitCodes(t *testing.T) {
	for out, want := range map[string]int{
		`[{"name":"companyinacan-dev","serversCount":1,"serversRunning":1}]`: ExitOK,
		`[{"name":"companyinacan-dev","serversCount":1,"serversRunning":0}]`: ExitFailure,
		`[]`: ExitFailure,
	} {
		stubDev(t, true, &fakeExec{out: map[string]string{listKey: out}})
		if code, _, _ := run(t, "dev", "status"); code != want {
			t.Errorf("%s: code=%d want %d", out, code, want)
		}
	}
	stubDev(t, true, &fakeExec{out: map[string]string{listKey: `[{"name":"companyinacan-dev","serversCount":1,"serversRunning":1}]`}})
	code, out, _ := run(t, "dev", "status", "--output", "json")
	if code != ExitOK || !strings.Contains(out, `"running":true`) {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestDevDoctor(t *testing.T) {
	stubDev(t, true, &fakeExec{})
	if code, out, _ := run(t, "dev", "doctor"); code != ExitOK || !strings.HasPrefix(out, "ok") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	stubDev(t, false, &fakeExec{})
	code, out, _ := run(t, "dev", "doctor")
	if code != ExitFailure || !strings.Contains(out, "k3d") || !strings.Contains(out, "kubectl") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if code, out, _ := run(t, "dev", "doctor", "--output", "json"); code != ExitFailure || !strings.HasPrefix(out, "[") {
		t.Fatalf("json code=%d out=%q", code, out)
	}
}
