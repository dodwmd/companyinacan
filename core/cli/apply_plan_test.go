package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
)

type planExec struct {
	fileExec
	calls []string
}

func (f *planExec) Run(ctx context.Context, c cluster.Cmd) (string, error) {
	line := strings.Join(c.Args, " ")
	f.calls = append(f.calls, line)
	if strings.Contains(line, "/can-core modules plan") {
		return `{"actions":[{"kind":"upgrade","module":"forms","reason":"manifest changed","widens":["new permission leads.erase"],"held":"approval","approval":"0123456789abcdef"}]}`, nil
	}
	return f.fileExec.Run(ctx, c)
}

func TestApplyPreviewsTheClusterPlanAndApproves(t *testing.T) {
	use := func() *planExec {
		f := &planExec{}
		old := newExecutor
		t.Cleanup(func() { newExecutor = old })
		newExecutor = func(*cobra.Command) cluster.Executor { return f }
		return f
	}
	dir := writeCompany(t, applyCompany)
	args := []string{"apply", dir, "--modules-dir", "../../modules"}

	f := use()
	code, out, errs := run(t, append(args, "--plan")...)
	if code != ExitOK || len(f.calls) != 1 || len(f.written) != 0 {
		t.Fatalf("--plan changed something: %d %v %q", code, f.calls, errs)
	}
	for _, want := range []string{"waiting for approval: upgrade: forms", "newly allowed: new permission leads.erase", "can apply --approve forms"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q in %q", want, out)
		}
	}

	f = use()
	code, out, errs = run(t, append(args, "--approve", "forms")...)
	if code != ExitOK || !strings.Contains(out, "approved the upgrade of forms") || !strings.Contains(strings.Join(f.calls, "\n"), "modules approve forms 0123456789abcdef") {
		t.Fatalf("%d %q %q %v", code, out, errs, f.calls)
	}

	f = use()
	if code, _, errs := run(t, append(args, "--approve", "crm")...); code != ExitFailure || !strings.Contains(errs, "no upgrade of that module") || len(f.written) != 0 {
		t.Fatalf("%d %q %v", code, errs, f.calls)
	}
}
