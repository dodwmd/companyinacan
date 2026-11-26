package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/dodwmd/companyinacan/core/doctor"
	"github.com/dodwmd/companyinacan/core/provider"
)

const serverCheckTimeout = 60 * time.Second

// serverCheck asks the company's server over SSH whether pods are kept away from the node
// (specs/038-node-isolation). It changes nothing, and every way of not getting a verdict is a
// failure. Host command output is not echoed beyond the script's own problem lines.
func serverCheck(ctx context.Context, cmd *cobra.Command, g *globalOpts, dir, modulesDir string) doctor.Result {
	r := doctor.Result{Tool: "node firewall", Required: true, Status: doctor.StatusUnreachable}
	t, err := load(cmd, g, dir, modulesDir)
	if err != nil {
		r.Remedy = "company.yaml could not be loaded (see `can validate`)"
		return r
	}
	tgt, err := provider.TargetFor(t.spec, t.store, t.deps)
	if err != nil {
		r.Remedy = t.redact.Redact(err.Error())
		return r
	}
	runner := t.deps.Runner
	if runner == nil {
		runner = func(tg provider.Target) provider.Runner { return provider.NewSSHRunner(tg) }
	}
	ctx, cancel := context.WithTimeout(ctx, serverCheckTimeout)
	defer cancel()
	out, err := runner(tgt).Run(ctx, provider.FirewallCheckScript())
	if err != nil {
		r.Remedy = "the server could not be checked over ssh"
		return r
	}
	c := provider.ParseFirewallCheck(out)
	if c.OK {
		r.Status, r.Path = doctor.StatusOK, c.Detail
		return r
	}
	r.Status, r.Remedy = doctor.StatusFailed, t.redact.Redact(c.Detail)
	return r
}

// firewallWarning asks a running server whether it has pod-to-node isolation. It never fails the
// command: a server that cannot be asked is reported by the checks that need it.
func firewallWarning(ctx context.Context, r provider.Runner) string {
	ctx, cancel := context.WithTimeout(ctx, serverCheckTimeout)
	defer cancel()
	out, err := r.Run(ctx, provider.FirewallCheckScript())
	if err != nil {
		return ""
	}
	return provider.FirewallWarning(out)
}
