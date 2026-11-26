package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/canbin"
	"github.com/dodwmd/companyinacan/core/launch"
	"github.com/dodwmd/companyinacan/core/provider"
)

// testSelf stands in for the running `can`: the test binary is not one.
var testSelf string

func init() {
	f, err := os.CreateTemp("", "fake-can-*")
	if err != nil {
		panic(err)
	}
	if _, err := f.WriteString("this is can"); err != nil {
		panic(err)
	}
	testSelf = f.Name()
	_ = f.Close()
	selfBinary = func() (string, error) { return testSelf, nil }
}

func TestBackupInstallInstallsTheRunningCan(t *testing.T) {
	kit, pass, _ := makeKit(t)
	company, root := writeCompany(t, testCompany), t.TempDir()
	seams(t, noRepoRunner())
	code, out, errs := runIn(t, "", "backup", "install", "--dir", company, "--root", root, "--kit", kit, "--passphrase-file", pass)
	if code != ExitOK {
		t.Fatalf("code=%d %s", code, errs)
	}
	p := filepath.Join(root, "usr/local/bin/can")
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "this is can" || !strings.Contains(out, p) {
		t.Fatalf("%v %q %q", err, b, out)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestBackupInstallWithoutACanInstallsNothing(t *testing.T) {
	kit, pass, _ := makeKit(t)
	company, root := writeCompany(t, testCompany), t.TempDir()
	seams(t, noRepoRunner())
	old := selfBinary
	selfBinary = func() (string, error) { return "", errors.New("not a linux can") }
	t.Cleanup(func() { selfBinary = old })
	code, _, errs := runIn(t, "", "backup", "install", "--dir", company, "--root", root, "--kit", kit, "--passphrase-file", pass)
	if code != ExitFailure || !strings.Contains(errs, "not a linux can") {
		t.Fatalf("code=%d %s", code, errs)
	}
	if e, _ := os.ReadDir(root); len(e) != 0 {
		t.Fatalf("something was installed: %v", e)
	}
}

// canHost is the cliRunner of a server that also answers the scripts of canbin.Install.
type canHost struct {
	*cliRunner
	installed string // the version of /usr/local/bin/can; "" = none
}

func (h *canHost) Run(ctx context.Context, script string) (string, error) {
	version := func(v string) string { return `{"version":"` + v + `","commit":"none"}` }
	switch {
	case strings.HasPrefix(script, "# can: which can"):
		out := "CAN_ARCH aarch64\n"
		if h.installed != "" {
			out += "CAN_CLI_SHA 00\nCAN_CLI_VERSION " + version(h.installed) + "\n"
		}
		return out + "CAN_CLI_PROBED\n", nil
	case strings.HasPrefix(script, "# can: prepare the upload"):
		return "CAN_CLI_TMP /usr/local/bin/.can.Ab12Cd34\n", nil
	case strings.HasPrefix(script, "# can: install the can binary"):
		h.installed = string(h.copied["/usr/local/bin/.can.Ab12Cd34"])
		return "CAN_CLI_VERSION " + version(h.installed) + "\nCAN_CLI_INSTALLED\n", nil
	}
	return h.cliRunner.Run(ctx, script)
}

type fakeCanSource struct {
	version     string
	unavailable error
}

func (f fakeCanSource) Binary(_ context.Context, _ string) (canbin.Binary, error) {
	return canbin.Binary{Data: []byte(f.version), Origin: "a test"}, f.unavailable
}
func (f fakeCanSource) Available() error       { return f.unavailable }
func (f fakeCanSource) Identity() canbin.Build { return canbin.Build{Version: f.version} }

func useCanSource(t *testing.T, s fakeCanSource) {
	t.Helper()
	old := newCanSource
	newCanSource = func(string, launch.ImageSet) canSource { return s }
	t.Cleanup(func() { newCanSource = old })
}

func canServer(t *testing.T) (string, *canHost) {
	t.Helper()
	dir := sshCompany(t)
	h := &canHost{cliRunner: newRunner()}
	useLifecycle(t, func() provider.Deps { return provider.Deps{Runner: func(provider.Target) provider.Runner { return h }} }, goodResolver())
	return dir, h
}

func TestProvisionOnlyInstallsCanAndStatusReportsSkew(t *testing.T) {
	dir, h := canServer(t)
	useCanSource(t, fakeCanSource{version: "v1.0.0"})
	code, out, errs := run(t, "up", dir, "--provision-only", "--modules-dir", "../../modules")
	if code != ExitOK || !strings.Contains(out, "installed can v1.0.0 linux/arm64") || h.installed != "v1.0.0" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	code, out, _ = run(t, "status", dir, "--modules-dir", "../../modules")
	if code != ExitOK || !strings.Contains(out, "server can: v1.0.0 linux/arm64") || strings.Contains(out, "skew") {
		t.Fatalf("code=%d out=%q", code, out)
	}

	useCanSource(t, fakeCanSource{version: "v1.1.0"}) // the operator's can is newer now
	code, out, _ = run(t, "status", dir, "--output", "json", "--modules-dir", "../../modules")
	var res statusResult
	if code != ExitOK || json.Unmarshal([]byte(out), &res) != nil || res.ServerCLI == nil || !res.ServerCLI.Skew || res.ServerCLI.Version != "v1.0.0" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "version skew") {
		t.Fatal(res.Warnings)
	}
}

func TestStatusSaysWhenABackedUpServerHasNoCan(t *testing.T) {
	dir, _ := canServer(t)
	useCanSource(t, fakeCanSource{version: "v1.0.0", unavailable: errors.New("none")})
	lifecycle.access = func(context.Context, provider.Target) (func(), error) { return nil, errors.New("no tunnel") }
	code, out, errs := run(t, "up", dir, "--provision-only", "--modules-dir", "../../modules")
	if code != ExitOK || !strings.Contains(out, "warning: install /usr/local/bin/can") {
		t.Fatalf("without backups it is a warning: code=%d out=%q err=%q", code, out, errs)
	}
	if err := provider.NewStore(dir).Mutate(func(s *provider.State) error {
		s.Launch = &provider.LaunchState{BackupInstalled: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, out, _ = run(t, "status", dir, "--modules-dir", "../../modules")
	if !strings.Contains(out, "server can: not installed") || !strings.Contains(out, "erasures do NOT reach the backup bucket") {
		t.Fatalf("out=%q", out)
	}
	// and with backups, provisioning fails rather than leave the server without it
	code, _, errs = run(t, "up", dir, "--provision-only", "--modules-dir", "../../modules")
	if code != ExitFailure || !strings.Contains(errs, "Backups are configured") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestUpRefusesAnUnusableCanBeforeProvisioning(t *testing.T) {
	dir, r := setup(t)
	useStack(t, false, stackResult(), nil)
	script := filepath.Join(t.TempDir(), "can")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // test
		t.Fatal(err)
	}
	code, _, errs := run(t, stackArgs(dir, "--can-binary", script)...)
	if code != ExitFailure || !strings.Contains(errs, "not a linux executable") || !strings.Contains(errs, "nothing has been provisioned") || len(r.ran) != 0 {
		t.Fatalf("code=%d err=%q ran=%d", code, errs, len(r.ran))
	}
}

func TestUpHandsTheStackACanSource(t *testing.T) {
	dir, _ := setup(t)
	spy := useStack(t, false, stackResult(), nil)
	useCanSource(t, fakeCanSource{version: "v1.0.0"})
	if code, _, errs := run(t, stackArgs(dir)...); code != ExitOK {
		t.Fatal(errs)
	}
	if spy.opts.CLI == nil {
		t.Fatal("the stack has no can to install")
	}
}

// `can restore` installs the running can under --root once it has succeeded.
func TestInstallSelfPutsCanUnderRoot(t *testing.T) {
	root := t.TempDir()
	line, err := installSelf(root)
	if err != nil || !strings.Contains(line, "is now this can") {
		t.Fatal(line, err)
	}
	if line, err = installSelf(root); err != nil || !strings.Contains(line, "already holds") {
		t.Fatal(line, err)
	}
}

// `up --provision-only` never read the default release manifest before: a broken one must not stop it.
func TestABrokenDefaultManifestDoesNotStopProvisioning(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("deploy", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launch.DefaultReleaseManifest, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warn strings.Builder
	if src, err := provisionCanSource(&stackFlags{}, &warn); err != nil || src == nil || !strings.Contains(warn.String(), "release-images.json") {
		t.Fatalf("%v %q", err, warn.String())
	}
	if _, err := provisionCanSource(&stackFlags{releaseManifest: launch.DefaultReleaseManifest}, &warn); err == nil {
		t.Fatal("a manifest the operator names must be readable")
	}
}
