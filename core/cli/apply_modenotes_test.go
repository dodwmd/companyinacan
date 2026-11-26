package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/cluster"
)

type companyExec struct {
	fileExec
	inForce string
}

func (c *companyExec) Run(ctx context.Context, cmd cluster.Cmd) (string, error) {
	if strings.Contains(strings.Join(cmd.Args, " "), "get configmap company") {
		return c.inForce, nil
	}
	return c.fileExec.Run(ctx, cmd)
}

// Plain `can apply` prints no plan (can-core reconciles) but still says when equity's identity mode
// changes.
func TestApplyPrintsTheEquityModeChange(t *testing.T) {
	const base = "schema: 1\nname: A\ndomain: a.example\nprovider: {type: local}\nmodules:\n  people: {}\n"
	attest, directory := base+"  equity: {}\n", base+"  equity: {settings: {identity: directory}}\n"
	for name, c := range map[string]struct{ was, next, say string }{
		"opting in":  {attest, directory, "note: equity verifies holders (directory mode)"},
		"opting out": {directory, attest, "note: equity returns to attestation mode"},
		"unchanged":  {directory, directory, ""},
	} {
		old := newExecutor
		f := &companyExec{inForce: c.was}
		newExecutor = func(*cobra.Command) cluster.Executor { return f }
		code, out, errs := run(t, "apply", writeCompany(t, c.next), "--modules-dir", "../../modules", "--skip-image-check")
		newExecutor = old
		if code != ExitOK || strings.Contains(out, "note: equity") != (c.say != "") || !strings.Contains(out, c.say) {
			t.Errorf("%s: code=%d out=%q err=%q", name, code, out, errs)
		}
	}
}
