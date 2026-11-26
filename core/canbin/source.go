package canbin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	module      = "github.com/dodwmd/companyinacan"
	mainPackage = module + "/cmd/can"
	// DefaultBaseURL is where release assets are downloaded from: <base>/<version>/<file>.
	DefaultBaseURL = "https://github.com/dodwmd/companyinacan/releases/download"
	maxArchive     = 200 << 20
)

// Arches are the server architectures a `can` can be installed for.
var Arches = []string{"amd64", "arm64"}

// Asset is a release archive holding `can` for one platform (the "cli" section of a release
// manifest, keyed "linux/<arch>").
type Asset struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

var (
	reVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	reSHA     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// CheckAssets validates the "cli" section of a release manifest of the given version.
func CheckAssets(version string, assets map[string]Asset) error {
	if len(assets) == 0 {
		return nil
	}
	if !reVersion.MatchString(version) {
		return fmt.Errorf("version %q is not a release version, so its can binaries cannot be located", version)
	}
	for platform, a := range assets {
		arch, ok := strings.CutPrefix(platform, "linux/")
		if !ok || !slices.Contains(Arches, arch) {
			return fmt.Errorf("cli: unknown platform %q", platform)
		}
		if want := "can_" + version + "_linux_" + arch + ".tar.gz"; a.File != want {
			return fmt.Errorf("cli %s: file %q must be %q", platform, a.File, want)
		}
		if !reSHA.MatchString(a.SHA256) {
			return fmt.Errorf("cli %s: sha256 must be 64 lowercase hex characters", platform)
		}
	}
	return nil
}

// Resolver finds a `can` for the server, in this order: the file the operator names; the release
// archive a release manifest pins by sha256; the running binary, when it is a static linux `can`
// of the server's architecture; a cross-compile of the source checkout.
type Resolver struct {
	Build Build  // of the running `can`
	File  string // --can-binary

	Assets         map[string]Asset // from the release manifest
	ReleaseVersion string
	ReleaseDir     string // the manifest's directory: archives downloaded next to it are used

	BaseURL string
	HTTP    *http.Client
	Self    func() (string, error) // path of the running binary
	Dirs    []string               // where to look for a source checkout (this directory or above)
	// Compile builds cmd/can of the checkout for linux/arch into out.
	Compile func(ctx context.Context, checkout, arch, ldflags, out string) error
}

// NewResolver is a Resolver with the real seams.
func NewResolver(b Build, file string) *Resolver {
	r := &Resolver{Build: b, File: file, BaseURL: DefaultBaseURL, HTTP: &http.Client{Timeout: 10 * time.Minute}, Self: os.Executable, Compile: goBuild}
	if wd, err := os.Getwd(); err == nil {
		r.Dirs = append(r.Dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		r.Dirs = append(r.Dirs, filepath.Dir(exe))
	}
	return r
}

// FindCheckout returns the root of the companyinacan source checkout at or above one of dirs.
func FindCheckout(dirs ...string) string {
	for _, d := range dirs {
		for d != "" {
			b, err := os.ReadFile(filepath.Join(d, "go.mod")) //nolint:gosec // fixed file name
			if err == nil && strings.HasPrefix(strings.TrimSpace(string(b)), "module "+module+"\n") {
				if st, err := os.Stat(filepath.Join(d, "cmd", "can")); err == nil && st.IsDir() {
					return d
				}
			}
			up := filepath.Dir(d)
			if up == d {
				break
			}
			d = up
		}
	}
	return ""
}

// git runs git in a checkout; "" when it cannot answer.
func git(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output() //nolint:gosec // fixed arguments
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Identity is what this `can` is, to compare the server's with: its own stamp, or for an
// unstamped build run from a checkout (`go run ./cmd/can`, as `make up` does) the checkout's
// commit, which is what would be installed.
func (r *Resolver) Identity() Build {
	b := r.Build
	if !b.hasCommit() && !b.hasVersion() {
		if co := FindCheckout(r.Dirs...); co != "" {
			if c := git(co, "rev-parse", "--short=12", "HEAD"); c != "" {
				b.Commit = c
			}
		}
	}
	return b
}

// Available reports whether there is any way at all to get a binary. It is checked before a
// server is paid for; the architecture is only known once there is one.
func (r *Resolver) Available() error {
	if r.File != "" {
		_, _, err := r.fromFile("")
		return err
	}
	if len(r.Assets) > 0 {
		return nil
	}
	if _, _, ok := r.self(); ok {
		return nil
	}
	if FindCheckout(r.Dirs...) != "" {
		if _, err := exec.LookPath("go"); err == nil {
			return nil
		}
		return errors.New("there is a source checkout but no `go` in PATH to build the server's can with: install Go, or pass --can-binary FILE (a linux can) or --release-manifest FILE")
	}
	return errors.New("no way to get a can binary for the server: pass --release-manifest FILE (a release's release-images.json), --can-binary FILE (a static linux build), or run from a source checkout with Go installed")
}

// Binary implements Source.
func (r *Resolver) Binary(ctx context.Context, arch string) (Binary, error) {
	if !slices.Contains(Arches, arch) {
		return Binary{}, fmt.Errorf("no can build for linux/%s", arch)
	}
	if r.File != "" {
		b, _, err := r.fromFile(arch)
		return b, err
	}
	if len(r.Assets) > 0 {
		a, ok := r.Assets["linux/"+arch]
		if !ok {
			return Binary{}, fmt.Errorf("release %s has no can for linux/%s", r.ReleaseVersion, arch)
		}
		b, err := r.fromRelease(ctx, a)
		if err == nil && r.Build.hasVersion() && r.Build.Version != r.ReleaseVersion {
			b.Notes = append(b.Notes, fmt.Sprintf("the server's can is the release manifest's %s; this can is %s", r.ReleaseVersion, r.Build.Version))
		}
		return b, err
	}
	// An unstamped binary (`go run`) in a checkout is better built from it: the server's `can`
	// then says which commit it is.
	co := FindCheckout(r.Dirs...)
	stamped := r.Build.hasVersion() || r.Build.hasCommit()
	if p, a, ok := r.self(); ok && a == arch && (stamped || co == "") {
		data, err := os.ReadFile(p) //nolint:gosec // the running binary
		if err != nil {
			return Binary{}, err
		}
		return Binary{Data: data, Origin: "this can binary"}, nil
	}
	if co == "" {
		return Binary{}, fmt.Errorf("no linux/%s can binary for the server: pass --release-manifest FILE (a release's release-images.json), --can-binary FILE, or run from a source checkout with Go installed", arch)
	}
	return r.compile(ctx, co, arch)
}

// self is the running binary when it can be copied to a server: `can` itself (not a test or a
// wrapper), a linux ELF, statically linked.
func (r *Resolver) self() (path, arch string, ok bool) {
	if r.Self == nil {
		return "", "", false
	}
	p, err := r.Self()
	if err != nil {
		return "", "", false
	}
	info, err := buildinfo.ReadFile(p)
	if err != nil || info.Path != mainPackage {
		return "", "", false
	}
	arch, static, err := inspectFile(p)
	return p, arch, err == nil && static
}

func inspectFile(p string) (arch string, static bool, err error) {
	f, err := os.Open(p) //nolint:gosec // operator-chosen path
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	return inspect(f)
}

// inspect reads the architecture of a linux executable and whether it needs a dynamic loader.
func inspect(rd io.ReaderAt) (arch string, static bool, err error) {
	e, err := elf.NewFile(rd)
	if err != nil {
		return "", false, errors.New("not a linux executable")
	}
	if e.OSABI != elf.ELFOSABI_NONE && e.OSABI != elf.ELFOSABI_LINUX {
		return "", false, errors.New("not a linux executable")
	}
	switch e.Machine {
	case elf.EM_X86_64:
		arch = "amd64"
	case elf.EM_AARCH64:
		arch = "arm64"
	default:
		return "", false, fmt.Errorf("built for %s, not amd64 or arm64", e.Machine)
	}
	static = true
	for _, p := range e.Progs {
		static = static && p.Type != elf.PT_INTERP
	}
	return arch, static, nil
}

// fromFile reads --can-binary; arch "" accepts either architecture.
func (r *Resolver) fromFile(arch string) (Binary, string, error) {
	data, err := os.ReadFile(r.File) //nolint:gosec // operator-chosen path
	if err != nil {
		return Binary{}, "", fmt.Errorf("--can-binary: %w", err)
	}
	got, static, err := inspect(bytes.NewReader(data))
	switch {
	case err != nil:
		return Binary{}, "", fmt.Errorf("--can-binary %s: %w", r.File, err)
	case arch != "" && got != arch:
		return Binary{}, "", fmt.Errorf("--can-binary %s is linux/%s, but the server is linux/%s", r.File, got, arch)
	}
	b := Binary{Data: data, Origin: r.File}
	if !static {
		b.Notes = []string{r.File + " is dynamically linked: build it with CGO_ENABLED=0 so that it does not depend on the server's libraries"}
	}
	return b, got, nil
}

// fromRelease takes the archive from next to the manifest or downloads it, checks its sha256
// against the manifest and returns the `can` inside.
func (r *Resolver) fromRelease(ctx context.Context, a Asset) (Binary, error) {
	local := filepath.Join(r.ReleaseDir, a.File)
	origin := local
	raw, err := readLimited(func() (io.ReadCloser, error) { return os.Open(local) }) //nolint:gosec // validated file name beside the manifest
	if errors.Is(err, os.ErrNotExist) {
		origin = strings.TrimRight(r.BaseURL, "/") + "/" + r.ReleaseVersion + "/" + a.File
		raw, err = readLimited(func() (io.ReadCloser, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin, nil)
			if err != nil {
				return nil, err
			}
			resp, err := r.HTTP.Do(req)
			if err != nil {
				return nil, err
			}
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			return resp.Body, nil
		})
	}
	if err != nil {
		return Binary{}, fmt.Errorf("get the release's can from %s: %w (download %s and put it next to the release manifest, or pass --can-binary)", origin, err, a.File)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != a.SHA256 {
		return Binary{}, fmt.Errorf("%s has sha256 %s but the release manifest records %s: refusing to install it", origin, got, a.SHA256)
	}
	data, err := untarCan(raw)
	if err != nil {
		return Binary{}, fmt.Errorf("%s: %w", origin, err)
	}
	return Binary{Data: data, Origin: "release " + r.ReleaseVersion + " (" + a.File + ")", Version: r.ReleaseVersion}, nil
}

func readLimited(open func() (io.ReadCloser, error)) ([]byte, error) {
	rc, err := open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxArchive+1))
	if err == nil && len(b) > maxArchive {
		err = errors.New("larger than any can release archive")
	}
	return b, err
}

func untarCan(gz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no `can` in the archive")
		}
		if err != nil {
			return nil, err
		}
		if h.Name == "can" && h.Typeflag == tar.TypeReg {
			return readLimited(func() (io.ReadCloser, error) { return io.NopCloser(tr), nil })
		}
	}
}

// compile cross-compiles the checkout, stamped so that `can version` on the server says what it
// was built from.
func (r *Resolver) compile(ctx context.Context, checkout, arch string) (Binary, error) {
	version, commit, date := r.Build.Version, git(checkout, "rev-parse", "--short=12", "HEAD"), ""
	// The commit time, not the clock: the same sources give the same bytes, so nothing is re-uploaded.
	if t, err := strconv.ParseInt(git(checkout, "show", "-s", "--format=%ct", "HEAD"), 10, 64); err == nil {
		date = time.Unix(t, 0).UTC().Format(time.RFC3339)
	}
	var notes []string
	if commit == "" {
		commit = r.Build.Commit
	} else if r.Build.hasCommit() && Compare(Build{Commit: commit}, Build{Commit: r.Build.Commit}) == Differs {
		notes = append(notes, fmt.Sprintf("the server's can was built from the checkout at commit %s, but this can was built from %s: run `make build` and `can up` again", commit, r.Build.Commit))
		version = "dev"
	}
	if version == "" {
		version = "dev"
	}
	if commit == "" {
		commit = "none"
	}
	if date == "" {
		date = "unknown"
	}
	flags := "-s -w"
	for _, f := range [][2]string{{"version", version}, {"commit", commit}, {"date", date}} {
		if !reField.MatchString(f[1]) {
			return Binary{}, fmt.Errorf("cannot stamp %s %q into the server's can", f[0], f[1])
		}
		flags += " -X " + module + "/core/cli." + f[0] + "=" + f[1]
	}
	dir, err := os.MkdirTemp("", "can-build-")
	if err != nil {
		return Binary{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out := filepath.Join(dir, "can")
	if err := r.Compile(ctx, checkout, arch, flags, out); err != nil {
		return Binary{}, fmt.Errorf("build can for linux/%s from %s: %w (or pass --can-binary FILE or --release-manifest FILE)", arch, checkout, err)
	}
	data, err := os.ReadFile(out) //nolint:gosec // just built
	if err != nil {
		return Binary{}, err
	}
	return Binary{Data: data, Origin: "a build of the checkout " + checkout, Notes: notes}, nil
}

func goBuild(ctx context.Context, checkout, arch, ldflags, out string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", out, "./cmd/can") //nolint:gosec // fixed command
	cmd.Dir = checkout
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch, "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}
