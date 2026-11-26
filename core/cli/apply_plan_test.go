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
	// The read of the cluster's company.yaml (conflict check) changes nothing and is not counted.
	if strings.Contains(line, "get configmap company --ignore-not-found -o json") {
		return f.fileExec.Run(ctx, c)
	}
	f.calls = append(f.calls, line)
	if strings.Contains(line, "/can-core modules plan") {
		return `{"actions":[{"kind":"upgrade","module":"forms","reason":"manifest changed","widens":["new permission leads.erase"],"held":"approval","approval":"0123456789abcdef","not_applied":["switching off the public routes (they stay public)"],"urgent":true}]}`, nil
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
	for _, want := range []string{"waiting for approval: upgrade: forms", "newly allowed: new permission leads.erase", "can apply --approve forms=0123456789abcdef", "NOT APPLIED: switching off the public routes", "takes it offline"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q in %q", want, out)
		}
	}

	// An apply that cannot take the exposure away syncs the rest and fails, loudly.
	use()
	code, out, errs = run(t, args...)
	if code != ExitFailure || !strings.Contains(out, "synced company.yaml") || !strings.Contains(errs, "module forms: NOT APPLIED: switching off the public routes") || !strings.Contains(errs, "Two ways out") {
		t.Fatalf("%d %q %q", code, out, errs)
	}

	f = use()
	code, out, errs = run(t, append(args, "--approve", "forms=0123456789abcdef")...)
	if code != ExitOK || !strings.Contains(out, "approved the upgrade of forms") || !strings.Contains(strings.Join(f.calls, "\n"), "modules approve forms 0123456789abcdef") {
		t.Fatalf("%d %q %q %v", code, out, errs, f.calls)
	}

	// Without the code shown, with another one, or for a module with nothing waiting: nothing changes.
	for arg, want := range map[string]string{"forms": "give the code", "forms=ffffffffffffffff": "does not match", "crm=0123456789abcdef": "no upgrade of that module"} {
		f = use()
		if code, _, errs := run(t, append(args, "--approve", arg)...); code != ExitFailure || !strings.Contains(errs, want) || len(f.written) != 0 || len(f.calls) != 1 {
			t.Fatalf("%s: %d %q %v", arg, code, errs, f.calls)
		}
	}
}
