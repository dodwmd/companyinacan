package canbin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// shellHost runs the real scripts with this machine's bash, against a temporary directory.
type shellHost struct {
	t       *testing.T
	uname   string // overrides `uname -m`
	corrupt bool
	copies  int
}

func (h *shellHost) Run(ctx context.Context, script string) (string, error) {
	if h.uname != "" {
		script = "uname() { echo " + h.uname + "; }\n" + script
	}
	cmd := exec.CommandContext(ctx, "bash", "-s") //nolint:gosec // test
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *shellHost) Copy(_ context.Context, p string, b []byte, mode string) error {
	h.copies++
	if h.corrupt {
		b = append(append([]byte{}, b...), '\n')
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	m, err := strconv.ParseUint(mode, 8, 32) // as SSHRunner does: the file exists already (mktemp)
	if err != nil {
		return err
	}
	return os.Chmod(p, os.FileMode(m))
}

type bytesSource struct {
	data    []byte
	version string
	arches  []string
}

func (s *bytesSource) Binary(_ context.Context, arch string) (Binary, error) {
	s.arches = append(s.arches, arch)
	return Binary{Data: s.data, Origin: "a test", Version: s.version}, nil
}

// script is a stand-in `can`.
func script(version string) []byte {
	return []byte("#!/bin/sh\necho '{\"version\":\"" + version + "\",\"commit\":\"abc123\",\"arch\":\"x\"}'\n")
}

func dest(t *testing.T) (string, *shellHost) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil || runtime.GOOS != "linux" {
		t.Skip("needs linux and bash")
	}
	return filepath.Join(t.TempDir(), "bin", "can"), &shellHost{t: t}
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	e, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var n []string
	for _, f := range e {
		n = append(n, f.Name())
	}
	return n
}

func TestInstallUpgradeAndNoopOnARealShell(t *testing.T) {
	d, h := dest(t)
	ctx := context.Background()
	h.uname = "aarch64"
	src := &bytesSource{data: script("v1.0.0")}
	r, err := Install(ctx, h, d, src)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed || r.Previous != nil || r.Build.Version != "v1.0.0" || r.Arch != "arm64" || src.arches[0] != "arm64" {
		t.Fatalf("%+v %v", r, src.arches)
	}
	if st, err := os.Stat(d); err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%v", err)
	}
	if r, err = Install(ctx, h, d, src); err != nil || r.Changed || r.Build.Version != "v1.0.0" || h.copies != 1 {
		t.Fatalf("an identical binary must not be uploaded again: %v %+v copies=%d", err, r, h.copies)
	}
	src.data = script("v1.1.0")
	if r, err = Install(ctx, h, d, src); err != nil || !r.Changed || r.Previous == nil || r.Previous.Version != "v1.0.0" || r.Build.Version != "v1.1.0" {
		t.Fatalf("%v %+v", err, r)
	}
	if !strings.Contains(r.Summary(), "v1.0.0") || !strings.Contains(r.Summary(), "v1.1.0") {
		t.Fatal(r.Summary())
	}
	// a promised version that matches is accepted
	if r, err = Install(ctx, h, d, &bytesSource{data: script("v1.2.0"), version: "v1.2.0"}); err != nil || r.Build.Version != "v1.2.0" {
		t.Fatalf("%v %+v", err, r)
	}
	if n := leftovers(t, filepath.Dir(d)); len(n) != 1 {
		t.Fatalf("leftovers: %v", n)
	}
}

func TestAFailedInstallKeepsThePreviousBinary(t *testing.T) {
	for name, tc := range map[string]struct {
		next    []byte
		version string
		corrupt bool
		want    string
	}{
		"corrupted upload": {next: script("v2.0.0"), corrupt: true, want: "different sha256"},
		"does not run":     {next: []byte("#!/bin/sh\nexit 1\n"), want: "does not run"},
		"says nothing":     {next: []byte("#!/bin/sh\nexit 0\n"), want: "not a can"},
		"empty version":    {next: script(""), want: "not a can"},
		"wrong version":    {next: script("v2.0.0"), version: "v3.0.0", want: "does not report version v3.0.0"},
	} {
		t.Run(name, func(t *testing.T) {
			d, h := dest(t)
			ctx := context.Background()
			if _, err := Install(ctx, h, d, &bytesSource{data: script("v1.0.0")}); err != nil {
				t.Fatal(err)
			}
			h.corrupt = tc.corrupt
			_, err := Install(ctx, h, d, &bytesSource{data: tc.next, version: tc.version})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
			got, _ := os.ReadFile(d) //nolint:gosec // test
			if string(got) != string(script("v1.0.0")) {
				t.Fatalf("the previous binary was replaced: %q", got)
			}
			if n := leftovers(t, filepath.Dir(d)); len(n) != 1 {
				t.Fatalf("leftovers: %v", n)
			}
		})
	}
}

func TestInstallRefusesAnUnknownArchitecture(t *testing.T) {
	d, h := dest(t)
	h.uname = "armv7l"
	src := &bytesSource{data: script("v1")}
	if _, err := Install(context.Background(), h, d, src); err == nil || !strings.Contains(err.Error(), "armv7l") || len(src.arches) != 0 {
		t.Fatalf("got %v", err)
	}
}

func TestInstallLocal(t *testing.T) {
	dir := t.TempDir()
	self, d := filepath.Join(dir, "self"), filepath.Join(dir, "usr", "local", "bin", "can")
	if err := os.WriteFile(self, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, changed, err := InstallLocal(self, d)
	sum := sha256.Sum256([]byte("one"))
	if err != nil || !changed || sha != hex.EncodeToString(sum[:]) {
		t.Fatal(sha, changed, err)
	}
	if st, _ := os.Stat(d); st.Mode().Perm() != 0o755 {
		t.Fatal(st.Mode())
	}
	if _, changed, err = InstallLocal(self, d); err != nil || changed {
		t.Fatal(changed, err)
	}
	if _, changed, err = InstallLocal(d, d); err != nil || changed { // run as the installed binary
		t.Fatal(changed, err)
	}
	if n := leftovers(t, filepath.Dir(d)); len(n) != 1 {
		t.Fatalf("leftovers: %v", n)
	}
}

// fakeELF is a header only: enough for the architecture and linking checks.
func fakeELF(t *testing.T, m elf.Machine, interp bool) []byte {
	t.Helper()
	h := elf.Header64{Type: uint16(elf.ET_EXEC), Machine: uint16(m), Version: 1, Ehsize: 64, Phentsize: 56}
	copy(h.Ident[:], "\x7fELF")
	h.Ident[elf.EI_CLASS], h.Ident[elf.EI_DATA], h.Ident[elf.EI_VERSION] = byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), 1
	var progs []elf.Prog64
	if interp {
		progs = append(progs, elf.Prog64{Type: uint32(elf.PT_INTERP)})
		h.Phoff, h.Phnum = 64, 1
	}
	var b bytes.Buffer
	for _, v := range []any{h, progs} {
		if err := binary.Write(&b, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCanBinaryFileMustMatchTheServer(t *testing.T) {
	ctx := context.Background()
	r := &Resolver{File: writeTemp(t, fakeELF(t, elf.EM_AARCH64, false))}
	if err := r.Available(); err != nil {
		t.Fatal(err)
	}
	if b, err := r.Binary(ctx, "arm64"); err != nil || len(b.Notes) != 0 {
		t.Fatal(b.Notes, err)
	}
	if _, err := r.Binary(ctx, "amd64"); err == nil || !strings.Contains(err.Error(), "linux/arm64, but the server is linux/amd64") {
		t.Fatal(err)
	}
	r.File = writeTemp(t, fakeELF(t, elf.EM_X86_64, true))
	if b, err := r.Binary(ctx, "amd64"); err != nil || !strings.Contains(strings.Join(b.Notes, " "), "dynamically linked") {
		t.Fatal(b.Notes, err)
	}
	r.File = writeTemp(t, []byte("#!/bin/sh\n"))
	if err := r.Available(); err == nil || !strings.Contains(err.Error(), "not a linux executable") {
		t.Fatal(err)
	}
}

func archive(t *testing.T, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, f := range [][2]string{{"can", content}, {"LICENSE", "x"}} {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o755, Size: int64(len(f[1])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(tw.Close(), zw.Close()); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestReleaseArchiveIsVerifiedAgainstTheManifest(t *testing.T) {
	ctx := context.Background()
	tgz := archive(t, "the arm64 can")
	sum := sha256.Sum256(tgz)
	const file = "can_v1.2.3_linux_arm64.tar.gz"
	assets := map[string]Asset{"linux/arm64": {File: file, SHA256: hex.EncodeToString(sum[:])}}
	if err := CheckAssets("v1.2.3", assets); err != nil {
		t.Fatal(err)
	}
	served := tgz
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if served == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(served)
	}))
	defer srv.Close()
	r := &Resolver{Build: Build{Version: "v1.2.3"}, Assets: assets, ReleaseVersion: "v1.2.3", ReleaseDir: t.TempDir(), BaseURL: srv.URL, HTTP: srv.Client()}

	b, err := r.Binary(ctx, "arm64")
	if err != nil || string(b.Data) != "the arm64 can" || b.Version != "v1.2.3" || len(b.Notes) != 0 || asked[0] != "/v1.2.3/"+file {
		t.Fatalf("%v %q %v", err, b.Data, asked)
	}
	if _, err := r.Binary(ctx, "amd64"); err == nil || !strings.Contains(err.Error(), "no can for linux/amd64") {
		t.Fatal(err)
	}
	served = archive(t, "evil")
	if _, err := r.Binary(ctx, "arm64"); err == nil || !strings.Contains(err.Error(), "refusing to install") {
		t.Fatal(err)
	}
	served = nil
	if _, err := r.Binary(ctx, "arm64"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatal(err)
	}
	// downloaded by hand, next to the manifest: no network
	if err := os.WriteFile(filepath.Join(r.ReleaseDir, file), tgz, 0o600); err != nil {
		t.Fatal(err)
	}
	asked = nil
	if b, err = r.Binary(ctx, "arm64"); err != nil || string(b.Data) != "the arm64 can" || len(asked) != 0 {
		t.Fatal(err, asked)
	}
	r.Build.Version = "v1.0.0"
	if b, err = r.Binary(ctx, "arm64"); err != nil || len(b.Notes) != 1 {
		t.Fatal(err, b.Notes)
	}
}

func TestCheckAssets(t *testing.T) {
	good := strings.Repeat("a", 64)
	for name, a := range map[string]map[string]Asset{
		"platform": {"darwin/arm64": {File: "can_v1.0.0_darwin_arm64.tar.gz", SHA256: good}},
		"path":     {"linux/arm64": {File: "../can_v1.0.0_linux_arm64.tar.gz", SHA256: good}},
		"digest":   {"linux/arm64": {File: "can_v1.0.0_linux_arm64.tar.gz", SHA256: "abc"}},
	} {
		if CheckAssets("v1.0.0", a) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if CheckAssets("latest", map[string]Asset{"linux/arm64": {}}) == nil {
		t.Error("a non-version accepted")
	}
}

func checkout(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "cmd", "can"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "go.mod"), []byte("module github.com/dodwmd/companyinacan\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCrossCompileFromACheckout(t *testing.T) {
	co := checkout(t)
	var gotArch, flags string
	r := &Resolver{Build: Build{Version: "v1.0.0", Commit: "none", Date: "x"}, Dirs: []string{filepath.Join(co, "cmd", "can")},
		Self: func() (string, error) { return os.Args[0], nil }, // a test binary is not `can`
		Compile: func(_ context.Context, dir, arch, ldflags, out string) error {
			gotArch, flags = dir+" "+arch, ldflags
			return os.WriteFile(out, []byte("built"), 0o600)
		}}
	b, err := r.Binary(context.Background(), "arm64")
	if err != nil || string(b.Data) != "built" || gotArch != co+" arm64" {
		t.Fatal(err, gotArch)
	}
	for _, want := range []string{"core/cli.version=v1.0.0", "core/cli.commit=none", "core/cli.date=unknown"} {
		if !strings.Contains(flags, want) {
			t.Errorf("%s not in %s", want, flags)
		}
	}
	r.Dirs = []string{t.TempDir()}
	if _, err := r.Binary(context.Background(), "arm64"); err == nil || !strings.Contains(err.Error(), "--release-manifest") {
		t.Fatal(err)
	}
	if err := r.Available(); err == nil {
		t.Fatal("nothing is available")
	}
}

func TestCompareAndParseVersion(t *testing.T) {
	for _, tc := range []struct {
		a, b Build
		want Comparison
	}{
		{Build{Version: "v1", Commit: "abcdef012345"}, Build{Version: "v1", Commit: "abcdef0"}, Same},
		{Build{Version: "v1"}, Build{Version: "v2"}, Differs},
		{Build{Version: "dev", Commit: "aaa"}, Build{Version: "dev", Commit: "bbb"}, Differs},
		{Build{Version: "dev", Commit: "none"}, Build{Version: "v2", Commit: "bbb"}, Unknown},
		{Build{Version: "v1", Commit: "none"}, Build{Version: "v1"}, Same},
	} {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("%v %v: %v", tc.a, tc.b, got)
		}
	}
	b, err := ParseVersion(`noise {"version":"v1 <script>","commit":"abc"}`)
	if err != nil || b.Version != "invalid" || b.Commit != "abc" {
		t.Fatal(b, err)
	}
	if _, err := ParseVersion("command not found"); err == nil {
		t.Fatal("want an error")
	}
}

func TestASomeoneElsesCheckoutIsNotBuilt(t *testing.T) {
	co := checkout(t)
	old := ownedByUser
	ownedByUser = func(p string) bool { return !strings.HasPrefix(p, co) }
	t.Cleanup(func() { ownedByUser = old })
	r := &Resolver{Dirs: []string{filepath.Join(co, "cmd")}, Compile: func(context.Context, string, string, string, string) error {
		t.Fatal("compiled another user's checkout")
		return nil
	}}
	if got := FindCheckout(r.Dirs...); got != "" {
		t.Fatal(got)
	}
	for _, err := range []error{r.Available(), func() error { _, err := r.Binary(context.Background(), "amd64"); return err }()} {
		if err == nil || !strings.Contains(err.Error(), co+" belongs to another user") {
			t.Fatal(err)
		}
	}
}

func TestTheCheckoutIsNamedBeforeItIsBuilt(t *testing.T) {
	co := checkout(t)
	var log bytes.Buffer
	r := &Resolver{Dirs: []string{co}, Log: &log, Compile: func(_ context.Context, _, _, _, out string) error {
		if !strings.Contains(log.String(), co) {
			t.Fatalf("not said before compiling: %q", log.String())
		}
		return os.WriteFile(out, []byte("x"), 0o600)
	}}
	if _, err := r.Binary(context.Background(), "amd64"); err != nil {
		t.Fatal(err)
	}
}
