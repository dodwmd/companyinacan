package cli

// The `can` on the company's server (specs/061-can-on-server): which one `can up` installs, and
// what `can status` says about the one there. `can backup install` and `can restore` run on the
// server and install the running binary.

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/canbin"
	"github.com/dodwmd/companyinacan/core/launch"
	"github.com/dodwmd/companyinacan/core/provider"
)

// canSource is what `can up` uses to put `can` on a server.
type canSource interface {
	canbin.Source
	Available() error
	Identity() canbin.Build
}

func currentBuild() canbin.Build { return canbin.Build{Version: version, Commit: commit, Date: date} }

// newCanSource is a seam: file is --can-binary, set the resolved images (a release manifest also
// pins the `can` archives).
var newCanSource = func(file string, set launch.ImageSet) canSource {
	r := canbin.NewResolver(currentBuild(), file)
	if set.Source == launch.SourceRelease {
		r.Assets, r.ReleaseVersion, r.ReleaseDir = set.CLI, set.Version, set.Dir
	}
	return r
}

// provisionCanSource is the source for `can up --provision-only`, which needs no images: the
// release manifest is read only if one is named (or the checkout has the default one).
func provisionCanSource(f *stackFlags, warn io.Writer) (canSource, error) {
	set := launch.ImageSet{}
	manifest := f.releaseManifest
	if manifest == "" && f.imagesFrom == "" {
		if _, err := os.Stat(launch.DefaultReleaseManifest); err == nil {
			manifest = launch.DefaultReleaseManifest
		}
	}
	if manifest != "" {
		loaded, err := launch.LoadRelease(manifest)
		switch {
		case err == nil:
			set = loaded
		case f.releaseManifest != "":
			return nil, err
		default:
			// Nobody asked for the default manifest here: it must not stop a run that only
			// provisions.
			fmt.Fprintf(warn, "warning: %v; not using it for the server's can\n", err)
		}
	}
	src := newCanSource(f.canBinary, set)
	if f.canBinary != "" { // a named file that cannot be used stops the run before any server exists
		if err := src.Available(); err != nil {
			return nil, err
		}
	}
	return src, nil
}

// installProvisioned installs `can` on a server `can up --provision-only` made, which is what
// `can restore` is then run with. It fails the command only when this directory has backups on.
func (t *target) installProvisioned(ctx context.Context, host provider.Runner, src canSource, out io.Writer, res *upResult) error {
	r, err := launch.InstallCLI(ctx, host, src, t.store)
	if err != nil {
		if st, lerr := t.store.Load(); lerr != nil || (st.Launch != nil && st.Launch.BackupInstalled) {
			return fmt.Errorf("%w. Backups are configured and need it", err)
		}
		w := fmt.Sprintf("%v. `can restore` on this server needs it; fix the cause and run `can up --provision-only` again", err)
		res.Warnings = append(res.Warnings, w)
		fmt.Fprintln(out, "warning: "+w)
		return nil
	}
	fmt.Fprintln(out, r.Summary())
	for _, n := range r.Notes {
		res.Warnings = append(res.Warnings, n)
		fmt.Fprintln(out, "warning: "+n)
	}
	return nil
}

type serverCLIStatus struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Arch      string `json:"arch,omitempty"`
	// Skew is true when the server's `can` is a different build from the one asking.
	Skew bool `json:"skew,omitempty"`
}

// serverCLI asks the server which `can` it has. The string is a warning ("" when there is
// nothing to warn about). A server that cannot be asked is only mentioned when backups are on.
func (t *target) serverCLI(ctx context.Context, r provider.Runner) (*serverCLIStatus, string) {
	st, err := t.store.Load()
	if err != nil {
		return nil, ""
	}
	backups := st.Launch != nil && st.Launch.BackupInstalled
	ctx, cancel := context.WithTimeout(ctx, serverCheckTimeout)
	defer cancel()
	s, err := canbin.Probe(ctx, r, backup.DefaultSettings().CanBin)
	if err != nil {
		if backups {
			return nil, "could not check which can the server has: the backup needs it"
		}
		return nil, ""
	}
	res := &serverCLIStatus{Installed: s.Installed, Version: s.Build.Version, Commit: s.Build.Commit}
	res.Arch, _ = canbin.Arch(s.Arch)
	why := ""
	if backups {
		why = ": erasures do NOT reach the backup bucket, and a restore would bring erased data back"
	}
	switch {
	case !s.Installed:
		return res, backup.DefaultSettings().CanBin + " is not on the server" + why + "; run `can up` to install it"
	case !s.Runs:
		return res, backup.DefaultSettings().CanBin + " on the server does not run" + why + "; run `can up` to replace it"
	}
	local := newCanSource("", launch.ImageSet{}).Identity()
	if canbin.Compare(s.Build, local) == canbin.Differs {
		res.Skew = true
		return res, fmt.Sprintf("version skew: the server's can is %s, this one is %s; run `can up` to update the server", s.Build, local)
	}
	return res, ""
}

// selfBinary is the running `can`, for the commands that run on the server. A seam: tests are
// not `can`.
var selfBinary = func() (string, error) {
	if runtime.GOOS != "linux" {
		return "", errors.New("this is not a linux can")
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	info, err := buildinfo.ReadFile(p)
	if err != nil || info.Path != "github.com/dodwmd/companyinacan/cmd/can" {
		return "", fmt.Errorf("%s is not a can binary", p)
	}
	return p, nil
}

// installSelf copies the running `can` to the place the backup script and timers run it from,
// under root. It returns a line for the operator.
func installSelf(root string) (string, error) {
	dest := filepath.Join(root, backup.DefaultSettings().CanBin)
	self, err := selfBinary()
	if err != nil {
		return "", fmt.Errorf("install %s: %w", dest, err)
	}
	sha, changed, err := canbin.InstallLocal(self, dest)
	if err != nil {
		return "", fmt.Errorf("install %s: %w", dest, err)
	}
	verb := "already holds"
	if changed {
		verb = "is now"
	}
	return fmt.Sprintf("%s %s this can, %s (sha256 %s)", dest, verb, currentBuild(), sha), nil
}
