package launch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/canbin"
	"github.com/dodwmd/companyinacan/core/provider"
)

// fakeCLI is a source of a "can binary" whose content is its version.
type fakeCLI struct {
	version string
	err     error
	arches  []string
}

func (f *fakeCLI) Binary(_ context.Context, arch string) (canbin.Binary, error) {
	f.arches = append(f.arches, arch)
	if f.err != nil {
		return canbin.Binary{}, f.err
	}
	return canbin.Binary{Data: []byte(f.version + " " + arch), Origin: "a test"}, nil
}

const (
	cliPath = "/usr/local/bin/can"
	cliTmp  = "/usr/local/bin/.can.Ab12Cd34"
)

// cli simulates the server's side of canbin.Install on the hostFake: what was uploaded to the
// temporary file is checked against the script's checksum and "renamed" over the binary.
func (h *hostFake) cli(script string) (string, bool, error) {
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	version := func(b []byte) string {
		v, _, _ := strings.Cut(string(b), " ")
		return fmt.Sprintf(`{"version":%q,"commit":"none"}`, v)
	}
	switch {
	case strings.HasPrefix(script, "# can: which can"):
		arch := h.uname
		if arch == "" {
			arch = "x86_64"
		}
		out := "CAN_ARCH " + arch + "\n"
		if b, ok := h.copied[cliPath]; ok {
			out += "CAN_CLI_SHA " + sum(b) + "\nCAN_CLI_VERSION " + version(b) + "\n"
		}
		return out + "CAN_CLI_PROBED\n", true, nil
	case strings.HasPrefix(script, "# can: prepare the upload"):
		delete(h.copied, cliTmp)
		return "CAN_CLI_TMP " + cliTmp + "\n", true, nil
	case strings.HasPrefix(script, "# can: install the can binary"):
		b := h.copied[cliTmp]
		delete(h.copied, cliTmp)
		if h.corrupt {
			b = append(b, 'x')
		}
		if !strings.Contains(script, "'"+sum(b)+"'") {
			return "CAN_CLI_SHA_MISMATCH " + sum(b) + "\n", true, errors.New("exit 3")
		}
		h.copied[cliPath] = b
		return "CAN_CLI_VERSION " + version(b) + "\nCAN_CLI_INSTALLED\n", true, nil
	}
	return "", false, nil
}

func cliStack(t *testing.T) (Options, Deps, *hostFake, *fakeCLI) {
	t.Helper()
	k := &fakeKube{rules: []kubeRule{{contains: "admin bootstrap", out: bootstrapOut}}}
	o, d, _ := newStack(t, k, &fakeLoader{})
	h := &hostFake{}
	d.Host = h
	src := &fakeCLI{version: "v1.0.0"}
	o.CLI = src
	return o, d, h, src
}

func phaseNames(r *Result) string {
	var n []string
	for _, p := range r.Phases {
		n = append(n, p.Name)
	}
	return strings.Join(n, ",")
}

func TestUpInstallsAndUpgradesTheServersCan(t *testing.T) {
	o, d, h, src := cliStack(t)
	h.uname = "aarch64"
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if string(h.copied[cliPath]) != "v1.0.0 arm64" || res.CLI == nil || !res.CLI.Changed || res.CLI.Arch != "arm64" {
		t.Fatalf("%q %+v", h.copied[cliPath], res.CLI)
	}
	if !strings.Contains(phaseNames(res), "routes,cli,ready") {
		t.Fatal(phaseNames(res))
	}
	st, _ := d.Store.Load()
	if st.CLI == nil || st.CLI.Version != "v1.0.0" || st.CLI.Arch != "arm64" || len(st.CLI.SHA256) != 64 {
		t.Fatalf("%+v", st.CLI)
	}

	// the same binary again: nothing is uploaded
	h.ran = nil
	if res, err = Run(context.Background(), o, d); err != nil || res.CLI.Changed {
		t.Fatalf("%v %+v", err, res.CLI)
	}
	if strings.Contains(strings.Join(h.ran, "\n"), "# can: install the can binary") {
		t.Fatal("an unchanged can was installed again")
	}

	src.version = "v1.1.0"
	if res, err = Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if !res.CLI.Changed || res.CLI.Previous == nil || res.CLI.Previous.Version != "v1.0.0" || res.CLI.Build.Version != "v1.1.0" {
		t.Fatalf("%+v", res.CLI)
	}
}

func TestBackupsFailWithoutACanOnTheServer(t *testing.T) {
	for name, tc := range map[string]struct {
		configure func(t *testing.T, o *Options, d Deps, h *hostFake, src *fakeCLI)
		want      string
	}{
		"backup flags, no binary": {func(t *testing.T, o *Options, _ Deps, _ *hostFake, src *fakeCLI) {
			o.Backup = backupInstall(t, *o)
			src.err = errors.New("no binary for you")
		}, "no binary for you"},
		"backup flags, no source": {func(t *testing.T, o *Options, _ Deps, _ *hostFake, _ *fakeCLI) {
			o.Backup, o.CLI = backupInstall(t, *o), nil
		}, "no source of a can binary"},
		"backup flags, corrupted upload": {func(t *testing.T, o *Options, _ Deps, h *hostFake, _ *fakeCLI) {
			o.Backup, h.corrupt = backupInstall(t, *o), true
		}, "different sha256"},
		"backup flags, unknown architecture": {func(t *testing.T, o *Options, _ Deps, h *hostFake, _ *fakeCLI) {
			o.Backup, h.uname = backupInstall(t, *o), "armv7l"
		}, "armv7l"},
		"installed earlier, no flags": {func(t *testing.T, _ *Options, d Deps, _ *hostFake, src *fakeCLI) {
			installedState(t, d, BackupInstallVersion, true)
			src.err = errors.New("no binary for you")
		}, "no binary for you"},
		// another directory installed the timer: only the server knows
		"timer on the server, state does not know": {func(_ *testing.T, _ *Options, _ Deps, _ *hostFake, src *fakeCLI) {
			src.err = errors.New("no binary for you")
		}, "no binary for you"},
	} {
		t.Run(name, func(t *testing.T) {
			o, d, h, src := cliStack(t)
			tc.configure(t, &o, d, h, src)
			_, err := Run(context.Background(), o, d)
			var pe *PhaseError
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "erasures") {
				t.Fatalf("got %v", err)
			}
			if name != "timer on the server, state does not know" && pe.Phase != PhaseBackup {
				t.Fatalf("phase %s", pe.Phase)
			}
			if _, ok := h.copied[cliPath]; ok {
				t.Fatal("something was installed")
			}
			if _, ok := h.copied["/etc/systemd/system/can-backup.timer"]; ok {
				t.Fatal("the backup was installed without its can")
			}
		})
	}
}

func TestWithoutBackupsAMissingCanIsAWarning(t *testing.T) {
	o, d, h, src := cliStack(t)
	h.out = map[string]string{"CAN_TIMER_OK; else": "CAN_TIMER_MISSING\n"}
	src.err = errors.New("no binary for you")
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if res.CLI != nil || !strings.Contains(strings.Join(res.Warnings, "\n"), "no binary for you") {
		t.Fatalf("%+v %v", res.CLI, res.Warnings)
	}
}

func TestInstalledBackupKeepsItsCanCurrentWithoutFlags(t *testing.T) {
	o, d, h, _ := cliStack(t)
	installedState(t, d, BackupInstallVersion, true)
	res, err := Run(context.Background(), o, d)
	if err != nil {
		t.Fatal(err)
	}
	if h.copied[cliPath] == nil || !strings.Contains(phaseNames(res), "routes,backup,ready") {
		t.Fatalf("%s", phaseNames(res))
	}
	st, err := d.Store.Load()
	if err != nil || st.Launch.Phases[PhaseBackup] == "" {
		t.Fatalf("%v %+v", err, st.Launch)
	}
}

// A re-run of an up-to-date backup install returns early; the can is upgraded all the same.
func TestCurrentBackupInstallStillUpgradesTheCan(t *testing.T) {
	o, d, h, src := cliStack(t)
	o.Backup = backupInstall(t, o)
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if err := MarkKitShown(d.Store); err != nil {
		t.Fatal(err)
	}
	h.out = map[string]string{pwRead: string(h.copied["/etc/can/backup.d/restic.password"])}
	delete(h.copied, "/usr/local/sbin/can-backup")
	src.version = "v2.0.0"
	if _, err := Run(context.Background(), o, d); err != nil {
		t.Fatal(err)
	}
	if string(h.copied[cliPath]) != "v2.0.0 amd64" {
		t.Fatalf("%q", h.copied[cliPath])
	}
}

func TestInstallCLIForgetsNothingWhenItFails(t *testing.T) {
	_, d, h, src := cliStack(t)
	if _, err := InstallCLI(context.Background(), h, src, d.Store); err != nil {
		t.Fatal(err)
	}
	src.version, h.corrupt = "v9.0.0", true
	if _, err := InstallCLI(context.Background(), h, src, d.Store); err == nil {
		t.Fatal("want an error")
	}
	st, _ := d.Store.Load()
	if st.CLI.Version != "v1.0.0" || string(h.copied[cliPath]) != "v1.0.0 amd64" {
		t.Fatalf("%+v %q", st.CLI, h.copied[cliPath])
	}
}

var _ provider.Runner = (*hostFake)(nil)
