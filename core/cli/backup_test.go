package cli

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup"
)

func runIn(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Execute(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func seams(t *testing.T, r backup.Runner) {
	t.Helper()
	old := backupSeams
	t.Cleanup(func() { backupSeams = old })
	backupSeams.runner = r
}

func credsFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "creds.env")
	if err := os.WriteFile(p, []byte("AWS_ACCESS_KEY_ID=AKIAFAKE\nAWS_SECRET_ACCESS_KEY=fakesecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const testCompany = "schema: 1\nname: Acme\ndomain: acme.example\nprovider: {type: ssh, options: {host: 203.0.113.7}}\n"

// makeKit writes an encrypted kit file and its passphrase file, using the CLI.
func makeKit(t *testing.T) (kit, pass, company string) {
	t.Helper()
	company = writeCompany(t, testCompany)
	tmp := t.TempDir()
	seams(t, noRepoRunner())
	kit, pass = filepath.Join(tmp, "kit.enc"), filepath.Join(tmp, "pass")
	if err := os.WriteFile(pass, []byte("a long enough passphrase\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runIn(t, "", "backup", "kit", "--dir", company, "--endpoint", "https://s3.example.invalid",
		"--bucket", "acme", "--credentials-file", credsFile(t), "--out", kit, "--passphrase-file", pass)
	if code != ExitOK {
		t.Fatalf("kit: code=%d %s", code, errs)
	}
	return kit, pass, company
}

func noRepoRunner() *backup.FakeRunner {
	return &backup.FakeRunner{Handle: func(c backup.Cmd) ([]byte, error) {
		if c.Name == "restic" && c.Args[0] == "cat" {
			return nil, &backup.CmdError{Code: 10, Stderr: "no repository"}
		}
		return nil, nil
	}}
}

func TestBackupKitPrintOnce(t *testing.T) {
	seams(t, noRepoRunner())
	company := writeCompany(t, testCompany)
	args := []string{"backup", "kit", "--dir", company, "--endpoint", "https://s3.example.invalid", "--bucket", "acme", "--credentials-file", credsFile(t), "--password-file", filepath.Join(t.TempDir(), "none")}
	code, out, _ := runIn(t, "show\nsaved\n", args...)
	if code != ExitOK || !strings.Contains(out, "Restic password:") || !strings.Contains(out, "fakesecret") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	code, out, _ = runIn(t, "no\n", args...)
	if code != ExitFailure || strings.Contains(out, "fakesecret") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	// Nothing was written into the company repo.
	ents, _ := os.ReadDir(company)
	if len(ents) != 1 {
		t.Errorf("company dir polluted: %v", ents)
	}
}

func TestBackupKitRefusesGitTree(t *testing.T) {
	seams(t, noRepoRunner())
	company := writeCompany(t, testCompany)
	_ = os.Mkdir(filepath.Join(company, ".git"), 0o755)
	pass := filepath.Join(t.TempDir(), "p")
	_ = os.WriteFile(pass, []byte("a long enough passphrase"), 0o600)
	code, _, errs := runIn(t, "", "backup", "kit", "--dir", company, "--endpoint", "https://e.invalid", "--bucket", "b",
		"--credentials-file", credsFile(t), "--out", filepath.Join(company, "kit.enc"), "--passphrase-file", pass)
	if code != ExitFailure || !strings.Contains(errs, "git working tree") {
		t.Fatalf("code=%d %s", code, errs)
	}
}

func TestRestorePlan(t *testing.T) {
	kit, pass, company := makeKit(t)
	fr := &backup.FakeRunner{}
	seams(t, fr)
	code, out, errs := runIn(t, "", "restore", "--kit", kit, "--passphrase-file", pass, "--plan", "--dir", company)
	if code != ExitOK {
		t.Fatalf("code=%d %s", code, errs)
	}
	for i := 1; i <= 6; i++ {
		if !strings.Contains(out, fmt.Sprintf("%d. ", i)) {
			t.Errorf("step %d missing:\n%s", i, out)
		}
	}
	if len(fr.Calls) != 0 {
		t.Errorf("--plan ran commands: %v", fr.Calls)
	}
	if _, err := os.Stat(filepath.Join(company, ".can", "restore.json")); err == nil {
		t.Error("--plan wrote state")
	}
}

func TestRestoreWrongPassphrase(t *testing.T) {
	kit, _, company := makeKit(t)
	bad := filepath.Join(t.TempDir(), "bad")
	_ = os.WriteFile(bad, []byte("not the passphrase!!"), 0o600)
	code, _, errs := runIn(t, "", "restore", "--kit", kit, "--passphrase-file", bad, "--plan", "--dir", company)
	if code != ExitFailure || !strings.Contains(errs, "passphrase") {
		t.Fatalf("code=%d %s", code, errs)
	}
}

func TestRestoreWithoutCoreIsNotImplemented(t *testing.T) {
	kit, pass, company := makeKit(t)
	seams(t, &backup.FakeRunner{})
	code, _, errs := runIn(t, "", "restore", "--kit", kit, "--passphrase-file", pass, "--dir", company)
	if code != ExitNotImplemented || !strings.Contains(errs, "E03") {
		t.Fatalf("code=%d %s", code, errs)
	}
	// Progress is recorded so the restore can resume once can-core exists.
	if _, err := os.Stat(filepath.Join(company, ".can", "restore.json")); err != nil {
		t.Errorf("state not written: %v", err)
	}
}

func TestRestoreKitFromStdin(t *testing.T) {
	company := writeCompany(t, testCompany)
	kit, err := backup.NewKit(backup.KitInput{CompanyName: "A", Domain: "a.example", Endpoint: "https://e.invalid", Bucket: "b",
		AccessKeyID: "k", SecretAccessKey: "s", ProviderType: "ssh"})
	if err != nil {
		t.Fatal(err)
	}
	seams(t, &backup.FakeRunner{})
	code, out, errs := runIn(t, kit.Text(), "restore", "--kit", "-", "--plan", "--dir", company)
	if code != ExitOK || !strings.Contains(out, "1. ") {
		t.Fatalf("code=%d %s %s", code, out, errs)
	}
}

func TestBackupStatusOff(t *testing.T) {
	t.Setenv("RESTIC_REPOSITORY", "")
	code, out, _ := runIn(t, "", "backup", "status", "--dumps-dir", t.TempDir())
	if code != ExitFailure || !strings.Contains(out, "OFF") {
		t.Fatalf("code=%d %s", code, out)
	}
}

func TestBackupVerifyWithFakeRestic(t *testing.T) {
	dumps := "/var/lib/can/dumps"
	content := []byte("dump-data")
	sum := fmt.Sprintf("%x", sha256.Sum256(content))
	fr := &backup.FakeRunner{Handle: func(c backup.Cmd) ([]byte, error) {
		switch c.Args[0] {
		case "snapshots":
			return []byte(`[{"id":"s1","time":"2026-11-25T03:30:00Z","tags":["can"]}]`), nil
		case "restore":
			var target string
			var incs []string
			for i, a := range c.Args {
				if a == "--target" {
					target = c.Args[i+1]
				}
				if a == "--include" {
					incs = append(incs, c.Args[i+1])
				}
			}
			for _, inc := range incs {
				p := filepath.Join(target, inc)
				_ = os.MkdirAll(filepath.Dir(p), 0o755)
				body := content
				if strings.HasSuffix(inc, backup.ManifestName) {
					body = []byte(sum + "  ./modules/a.dump\n")
				}
				_ = os.WriteFile(p, body, 0o600)
			}
		}
		return nil, nil
	}}
	seams(t, fr)
	code, out, errs := runIn(t, "", "backup", "verify", "--repo", "s3:https://e.invalid/b/restic", "--env-file", credsFile(t),
		"--password-file", "/nonexistent-pw", "--dumps-dir", dumps)
	if code != ExitOK || !strings.Contains(out, "verify OK") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errs)
	}
	var sawCheck bool
	for _, c := range fr.Calls {
		sawCheck = sawCheck || c.Args[0] == "check"
		if strings.Join(c.Args, " ") == "" || strings.Contains(strings.Join(c.Args, " "), "fakesecret") {
			t.Errorf("credential on argv: %v", c.Args)
		}
	}
	if !sawCheck {
		t.Error("restic check not run")
	}
}

func TestBackupInstall(t *testing.T) {
	kit, pass, _ := makeKit(t)
	company := writeCompany(t, testCompany)
	fr := &backup.FakeRunner{Handle: func(c backup.Cmd) ([]byte, error) {
		if c.Name == "restic" && c.Args[0] == "cat" {
			return nil, &backup.CmdError{Code: 10, Stderr: "no repository"}
		}
		return nil, nil
	}}
	seams(t, fr)
	root := t.TempDir()
	code, out, errs := runIn(t, "", "backup", "install", "--dir", company, "--root", root, "--kit", kit, "--passphrase-file", pass, "--enable")
	if code != ExitOK {
		t.Fatalf("code=%d %s %s", code, out, errs)
	}
	for p, mode := range map[string]os.FileMode{
		"/usr/local/sbin/can-backup":                   0o700,
		"/etc/systemd/system/can-backup.service":       0o644,
		"/etc/systemd/system/can-backup.timer":         0o644,
		"/etc/can/backup.d/" + backup.PasswordFileName: 0o600,
		"/etc/can/backup.d/" + backup.EnvFileName:      0o600,
	} {
		st, err := os.Stat(filepath.Join(root, p))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if st.Mode().Perm() != mode {
			t.Errorf("%s mode %v want %v", p, st.Mode().Perm(), mode)
		}
	}
	envBody, _ := os.ReadFile(filepath.Join(root, "/etc/can/backup.d/"+backup.EnvFileName))
	if !strings.Contains(string(envBody), "RESTIC_REPOSITORY=") {
		t.Error("the installed env file does not name the repository")
	}
	var cmds []string
	for _, c := range fr.Calls {
		cmds = append(cmds, c.Name+" "+strings.Join(c.Args, " "))
	}
	joined := strings.Join(cmds, "\n")
	for _, w := range []string{"restic init", "systemctl daemon-reload", "systemctl enable --now can-backup.timer"} {
		if !strings.Contains(joined, w) {
			t.Errorf("missing %q in\n%s", w, joined)
		}
	}
	svc, _ := os.ReadFile(filepath.Join(root, "/etc/systemd/system/can-backup.service"))
	if strings.Contains(string(svc), "fakesecret") {
		t.Error("credentials in the unit")
	}
}

func TestBackupInstallPlanNeedsNoKit(t *testing.T) {
	company := writeCompany(t, testCompany)
	code, out, errs := runIn(t, "", "backup", "install", "--dir", company, "--plan")
	if code != ExitOK || !strings.Contains(out, "restic backup") {
		t.Fatalf("code=%d %s %s", code, out, errs)
	}
	code, _, errs = runIn(t, "", "backup", "install", "--dir", company, "--root", t.TempDir())
	if code != ExitFailure || !strings.Contains(errs, "--kit") {
		t.Fatalf("code=%d %s", code, errs)
	}
}

func TestLedgerAddCmd(t *testing.T) {
	l := filepath.Join(t.TempDir(), "l.jsonl")
	code, out, _ := runIn(t, "", "backup", "ledger-add", "--ledger", l, "--subject", "subj-1", "--class", "lead-submission")
	if code != ExitOK || !strings.Contains(out, "added=true") {
		t.Fatalf("code=%d %s", code, out)
	}
	code, _, errs := runIn(t, "", "backup", "ledger-add", "--ledger", l, "--subject", "jane@example.com", "--class", "x")
	if code != ExitFailure || !strings.Contains(errs, "PII") {
		t.Fatalf("code=%d %s", code, errs)
	}
}

func kitArgs(t *testing.T, extra ...string) []string {
	t.Helper()
	company := writeCompany(t, testCompany)
	return append([]string{"backup", "kit", "--dir", company, "--endpoint", "https://s3.example.invalid", "--bucket", "acme",
		"--credentials-file", credsFile(t)}, extra...)
}

func TestBackupKitRefusesWhenRepositoryExists(t *testing.T) {
	for name, err := range map[string]error{"exists": nil, "wrong password": &backup.CmdError{Code: 12, Stderr: "wrong password"}} {
		fr := &backup.FakeRunner{Handle: func(backup.Cmd) ([]byte, error) { return nil, err }}
		seams(t, fr)
		code, out, errs := runIn(t, "show\nsaved\n", kitArgs(t, "--password-file", filepath.Join(t.TempDir(), "none"))...)
		if code != ExitFailure || !strings.Contains(errs, "already exist") || strings.Contains(out, "Restic password") {
			t.Errorf("%s: code=%d out=%q err=%q", name, code, out, errs)
		}
	}
}

func TestBackupKitReusesExistingPassword(t *testing.T) {
	fr := noRepoRunner()
	seams(t, fr)
	pw := filepath.Join(t.TempDir(), "restic.password")
	_ = os.WriteFile(pw, []byte("the-existing-restic-password-0123\n"), 0o600)
	code, out, errs := runIn(t, "show\nsaved\n", kitArgs(t, "--password-file", pw)...)
	if code != ExitOK || !strings.Contains(out, "the-existing-restic-password-0123") || !strings.Contains(errs, "reusing") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errs)
	}
}

func TestBackupKitReusedPasswordMustOpenExistingRepo(t *testing.T) {
	pw := filepath.Join(t.TempDir(), "restic.password")
	_ = os.WriteFile(pw, []byte("the-existing-restic-password-0123\n"), 0o600)
	// Repository exists and the password opens it: fine.
	seams(t, &backup.FakeRunner{})
	if code, _, errs := runIn(t, "show\nsaved\n", kitArgs(t, "--password-file", pw)...); code != ExitOK {
		t.Fatalf("code=%d %s", code, errs)
	}
	// Repository exists and restic says wrong password (exit 12): refuse.
	seams(t, &backup.FakeRunner{Handle: func(backup.Cmd) ([]byte, error) { return nil, &backup.CmdError{Code: 12, Stderr: "wrong password"} }})
	code, out, errs := runIn(t, "show\nsaved\n", kitArgs(t, "--password-file", pw)...)
	if code != ExitFailure || !strings.Contains(errs, "does not open") || strings.Contains(out, "Restic password") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestBackupKitFailsClosedWhenResticCannotRun(t *testing.T) {
	seams(t, &backup.FakeRunner{Handle: func(backup.Cmd) ([]byte, error) {
		return nil, errors.New(`exec: "restic": executable file not found in $PATH`)
	}})
	for _, args := range [][]string{
		kitArgs(t, "--password-file", filepath.Join(t.TempDir(), "none")),
		kitArgs(t, "--password-file", func() string {
			p := filepath.Join(t.TempDir(), "pw")
			_ = os.WriteFile(p, []byte("existing-password-0123456789"), 0o600)
			return p
		}()),
	} {
		code, out, errs := runIn(t, "show\nsaved\n", args...)
		if code != ExitFailure || !strings.Contains(errs, "0.17") || strings.Contains(out, "Restic password") {
			t.Errorf("code=%d out=%q err=%q", code, out, errs)
		}
	}
}

func TestBackupKitPassphraseConfirmation(t *testing.T) {
	seams(t, noRepoRunner())
	old := newPassphraseSource
	t.Cleanup(func() { newPassphraseSource = old })
	answers := []string{"first passphrase!!", "different passphrase"}
	newPassphraseSource = func(string) func(string) (string, error) {
		return func(string) (string, error) { a := answers[0]; answers = answers[1:]; return a, nil }
	}
	out := filepath.Join(t.TempDir(), "kit.enc")
	code, _, errs := runIn(t, "", kitArgs(t, "--out", out, "--password-file", filepath.Join(t.TempDir(), "none"))...)
	if code != ExitFailure || !strings.Contains(errs, "do not match") {
		t.Fatalf("code=%d %s", code, errs)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("kit written despite mismatched passphrases")
	}
	answers = []string{"same passphrase!!", "same passphrase!!"}
	if code, _, errs := runIn(t, "", kitArgs(t, "--out", out, "--password-file", filepath.Join(t.TempDir(), "none"))...); code != ExitOK {
		t.Fatalf("code=%d %s", code, errs)
	}
}

func TestRestoreHelpSaysNotFunctional(t *testing.T) {
	_, out, _ := runIn(t, "", "restore", "--help")
	if !strings.Contains(out, "NOT FUNCTIONAL") {
		t.Errorf("help must state restore is not functional yet:\n%s", out)
	}
}

func TestInstallWritesQuotedEnvAndFailureUnit(t *testing.T) {
	kit, pass, _ := makeKit(t)
	company := writeCompany(t, testCompany)
	root := t.TempDir()
	if code, _, errs := runIn(t, "", "backup", "install", "--dir", company, "--root", root, "--kit", kit, "--passphrase-file", pass); code != ExitOK {
		t.Fatalf("code=%d %s", code, errs)
	}
	env, _ := os.ReadFile(filepath.Join(root, "etc/can/backup.d", backup.EnvFileName))
	if !strings.Contains(string(env), "AWS_SECRET_ACCESS_KEY='fakesecret'") {
		t.Errorf("env values not quoted:\n%s", env)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/systemd/system/can-backup-failure.service")); err != nil {
		t.Error("failure unit not installed")
	}
}

func TestLedgerSyncRefusesPlainHTTP(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	_ = os.WriteFile(pw, []byte("pw-pw-pw-pw-pw-pw-pw"), 0o600)
	l := filepath.Join(dir, "l.jsonl")
	_, _, _ = runIn(t, "", "backup", "ledger-add", "--ledger", l, "--subject", "s1", "--class", "c")
	code, _, errs := runIn(t, "", "backup", "ledger-sync", "--ledger", l, "--repo", "s3:http://127.0.0.1:9/b/restic", "--password-file", pw, "--env-file", credsFile(t))
	if code != ExitFailure || !strings.Contains(errs, "https") {
		t.Fatalf("code=%d %s", code, errs)
	}
}

// QA 9: on the server `can backup status` finds the repository in the installed env file; no
// --repo and no shell variables are needed.
func TestBackupStatusReadsTheRepositoryFromTheEnvFile(t *testing.T) {
	t.Setenv("RESTIC_REPOSITORY", "")
	env := filepath.Join(t.TempDir(), backup.EnvFileName)
	if err := os.WriteFile(env, []byte("AWS_ACCESS_KEY_ID='a'\nRESTIC_REPOSITORY='s3:https://e.invalid/b/restic'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fr := &backup.FakeRunner{Handle: func(backup.Cmd) ([]byte, error) {
		return []byte(`[{"id":"s1","time":"2026-11-25T03:30:00Z","tags":["can"]}]`), nil
	}}
	seams(t, fr)
	_, out, errs := runIn(t, "", "backup", "status", "--env-file", env, "--dumps-dir", t.TempDir())
	if strings.Contains(out, "OFF") || len(fr.Calls) == 0 {
		t.Fatalf("the repository in the env file was not used: %s %s", out, errs)
	}
}
