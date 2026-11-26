package launch

import (
	"context"
	"errors"
	"fmt"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/canbin"
	"github.com/dodwmd/companyinacan/core/provider"
)

// InstallCLI puts src's `can` on the server behind host, and records in state which one it is.
func InstallCLI(ctx context.Context, host provider.Runner, src canbin.Source, st *provider.Store) (canbin.Result, error) {
	if src == nil {
		return canbin.Result{}, errors.New("no source of a can binary for the server")
	}
	dest := backup.DefaultSettings().CanBin
	res, err := canbin.Install(ctx, host, dest, src)
	if err != nil {
		return canbin.Result{}, fmt.Errorf("install %s on the server: %w", dest, err)
	}
	return res, st.Mutate(func(s *provider.State) error {
		s.CLI = &provider.CLIState{Version: res.Build.Version, Commit: res.Build.Commit, Arch: res.Arch, SHA256: res.SHA256}
		return nil
	})
}

// cli installs or upgrades the server's `can`, once per run. When backups are configured a
// failure stops the run; otherwise it is a warning.
func (r *runner) cli(ctx context.Context, backups bool) (bool, string, error) {
	res, err := InstallCLI(ctx, r.d.Host, r.o.CLI, r.d.Store)
	if err != nil && !backups {
		// The state may not know of a timer another directory installed: ask the server, and
		// treat no answer as "there may be one".
		on, terr := r.timerOnHost(ctx, backup.DefaultSettings())
		backups = on || terr != nil
	}
	if err != nil {
		if backups {
			return false, "", fmt.Errorf("%w. Backups are configured and need it: erasures do NOT reach the bucket without a current can on the server, and a restore would bring erased data back", err)
		}
		r.res.Warnings = append(r.res.Warnings, fmt.Sprintf("%v. `can restore` and `can backup` on the server need it; fix the cause and run `can up` again", err))
		return false, "NOT installed, see the warning", nil
	}
	r.res.CLI = &res
	r.res.Warnings = append(r.res.Warnings, res.Notes...)
	r.say("      %s", res.Summary())
	return !res.Changed, "", nil
}
