package canbin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dodwmd/companyinacan/core/provider"
)

// Path is where the server's `can` lives (backup.Settings.CanBin).
const Path = "/usr/local/bin/can"

// Binary is a `can` for one server architecture.
type Binary struct {
	Data    []byte
	Origin  string // where it came from, for the operator
	Version string // the version it must report; "" when any will do
	Notes   []string
}

// Source supplies a linux `can` for an architecture (amd64 or arm64).
type Source interface {
	Binary(ctx context.Context, arch string) (Binary, error)
}

// ServerState is what the server has now.
type ServerState struct {
	Arch      string
	Installed bool
	SHA256    string
	Runs      bool // it answered `can version`
	Build     Build
}

var rePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// ProbeScript reads the server's architecture and the `can` it has. It changes nothing.
func ProbeScript(dest string) string {
	return fmt.Sprintf(`# can: which can does the server have
set -u
echo "CAN_ARCH $(uname -m)"
if [ -f '%[1]s' ] && [ -x '%[1]s' ]; then
  echo "CAN_CLI_SHA $(sha256sum '%[1]s' | cut -d' ' -f1)"
  echo "CAN_CLI_VERSION $('%[1]s' version --output json 2>/dev/null | tr -d '\n')"
fi
echo CAN_CLI_PROBED
`, dest)
}

// Probe asks the server for its architecture and installed `can`.
func Probe(ctx context.Context, r provider.Runner, dest string) (ServerState, error) {
	if !rePath.MatchString(dest) {
		return ServerState{}, fmt.Errorf("invalid destination %q", dest)
	}
	out, err := r.Run(ctx, ProbeScript(dest))
	if err != nil {
		return ServerState{}, fmt.Errorf("ask the server which can it has: %w", err)
	}
	if !strings.Contains(out, "CAN_CLI_PROBED") {
		return ServerState{}, errors.New("ask the server which can it has: no answer")
	}
	var s ServerState
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		v = strings.TrimSpace(v)
		switch k {
		case "CAN_ARCH":
			s.Arch = v
		case "CAN_CLI_SHA":
			s.Installed, s.SHA256 = true, v
		case "CAN_CLI_VERSION":
			if b, err := ParseVersion(v); err == nil {
				s.Runs, s.Build = true, b
			}
		}
	}
	return s, nil
}

// Arch maps `uname -m` to a Go architecture.
func Arch(machine string) (string, error) {
	switch machine {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("no can build for the server's architecture %q (linux/amd64 and linux/arm64 are supported)", machine)
}

// Result is what Install did.
type Result struct {
	Changed  bool
	Previous *Build // the replaced binary, when it answered `can version`
	Build    Build  // what is installed now
	SHA256   string
	Arch     string
	Origin   string
	Notes    []string
}

// Summary is one line for the operator.
func (r Result) Summary() string {
	switch {
	case !r.Changed:
		return fmt.Sprintf("can %s linux/%s already on the server", r.Build, r.Arch)
	case r.Previous != nil:
		return fmt.Sprintf("replaced can %s with %s linux/%s from %s, sha256 verified", r.Previous, r.Build, r.Arch, r.Origin)
	}
	return fmt.Sprintf("installed can %s linux/%s from %s, sha256 verified", r.Build, r.Arch, r.Origin)
}

// installScript checks the upload and renames it over dest. want is the version it must report
// ("" = any), validated by the caller.
func installScript(dest, tmp, sha, want string) string {
	check := ""
	if want != "" {
		check = fmt.Sprintf(`case "$v" in *'"version":"%s"'*) ;; *) echo "CAN_CLI_WRONG_VERSION $v"; exit 6 ;; esac
`, want)
	}
	return fmt.Sprintf(`# can: install the can binary
set -eu
trap 'rm -f "%[2]s"' EXIT
got=$(sha256sum '%[2]s' | cut -d' ' -f1)
if [ "$got" != '%[3]s' ]; then echo "CAN_CLI_SHA_MISMATCH $got"; exit 3; fi
if ! v=$('%[2]s' version --output json); then echo CAN_CLI_DOES_NOT_RUN; exit 4; fi
%[4]sif [ -d '%[1]s' ]; then echo CAN_CLI_IS_A_DIRECTORY; exit 5; fi
mv -f '%[2]s' '%[1]s'
echo "CAN_CLI_VERSION $(printf '%%s' "$v" | tr -d '\n')"
echo CAN_CLI_INSTALLED
`, dest, tmp, sha, check)
}

// Install makes dest on the server the binary src supplies for its architecture. It does nothing
// when that very file is already there. The upload goes to a temporary file beside dest, which is
// checked against the checksum and run on the server before it is renamed over dest, so a failed
// or interrupted install leaves the previous binary in place.
func Install(ctx context.Context, r provider.Runner, dest string, src Source) (Result, error) {
	st, err := Probe(ctx, r, dest)
	if err != nil {
		return Result{}, err
	}
	arch, err := Arch(st.Arch)
	if err != nil {
		return Result{}, err
	}
	b, err := src.Binary(ctx, arch)
	if err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(b.Data)
	res := Result{SHA256: hex.EncodeToString(sum[:]), Arch: arch, Origin: b.Origin, Notes: b.Notes}
	if st.Installed && st.SHA256 == res.SHA256 && st.Runs {
		res.Build = st.Build
		return res, nil
	}
	if st.Runs {
		prev := st.Build
		res.Previous = &prev
	}
	if b.Version != "" && !reField.MatchString(b.Version) {
		return Result{}, fmt.Errorf("invalid version %q from %s", b.Version, b.Origin)
	}
	dir := path.Dir(dest)
	tmp := path.Join(dir, "."+path.Base(dest)+".new")
	if _, err := r.Run(ctx, fmt.Sprintf("# can: prepare the upload\nmkdir -p '%s'\nrm -f '%s'\n", dir, tmp)); err != nil {
		return Result{}, fmt.Errorf("prepare %s on the server: %w", dir, err)
	}
	if err := upload(ctx, r, tmp, b.Data); err != nil {
		return Result{}, fmt.Errorf("upload can to the server: %w", err)
	}
	out, err := r.Run(ctx, installScript(dest, tmp, res.SHA256, b.Version))
	switch {
	case strings.Contains(out, "CAN_CLI_WRONG_VERSION"):
		return Result{}, fmt.Errorf("the can from %s does not report version %s, as expected: nothing was replaced: %s", b.Origin, b.Version, strings.TrimSpace(out))
	case strings.Contains(out, "CAN_CLI_SHA_MISMATCH"):
		return Result{}, fmt.Errorf("the can binary arrived on the server with a different sha256 (expected %s): the upload was corrupted, nothing was replaced", res.SHA256)
	case strings.Contains(out, "CAN_CLI_DOES_NOT_RUN"):
		return Result{}, fmt.Errorf("the linux/%s can binary from %s does not run on the server, nothing was replaced: %s", arch, b.Origin, strings.TrimSpace(out))
	case err != nil || !strings.Contains(out, "CAN_CLI_INSTALLED"):
		return Result{}, fmt.Errorf("install %s on the server: %v: %s", dest, err, strings.TrimSpace(out))
	}
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "CAN_CLI_VERSION "); ok {
			if res.Build, err = ParseVersion(v); err != nil {
				return Result{}, fmt.Errorf("the installed %s: %w", dest, err)
			}
		}
	}
	if res.Build.Version == "" {
		return Result{}, fmt.Errorf("the installed %s did not report its version", dest)
	}
	res.Changed = true
	return res, nil
}

func upload(ctx context.Context, r provider.Runner, dest string, data []byte) error {
	if sc, ok := r.(provider.StreamCopier); ok {
		return sc.CopyFrom(ctx, dest, bytes.NewReader(data), "0755")
	}
	return r.Copy(ctx, dest, data, "0755")
}

// InstallLocal copies the binary at self to dest on this machine (`can backup install` and
// `can restore`, which run on the server), through a temporary file in the same directory that is
// checked against the source's checksum and then renamed over dest.
func InstallLocal(self, dest string) (sha string, changed bool, err error) {
	data, err := os.ReadFile(self) //nolint:gosec // the running binary
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(data)
	sha = hex.EncodeToString(sum[:])
	if have, err := fileSHA256(dest); err == nil && have == sha {
		return sha, false, os.Chmod(dest, 0o755) //nolint:gosec // an executable for every user
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil { //nolint:gosec // system directory
		return "", false, err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".can-*.new")
	if err != nil {
		return "", false, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, err = f.Write(data)
	if err = errors.Join(err, f.Chmod(0o755), f.Sync(), f.Close()); err != nil {
		return "", false, err
	}
	got, err := fileSHA256(tmp)
	if err != nil {
		return "", false, err
	}
	if got != sha {
		return "", false, fmt.Errorf("the copy of %s has a different sha256: not installed", self)
	}
	return sha, true, os.Rename(tmp, dest)
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // fixed or operator-chosen path
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
