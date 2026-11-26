package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/backup"
)

func TestBackupSnapshotsUsesTheKitAlone(t *testing.T) {
	kit, pass, _ := makeKit(t)
	t.Setenv("RESTIC_REPOSITORY", "/somewhere/else")
	listing := `[{"id":"aaaaaaaaaaaa","short_id":"aaaaaaaa","time":"2026-11-26T03:30:00Z","tags":["can"]},{"id":"bbbbbbbbbbbb","short_id":"bbbbbbbb","time":"2026-11-25T03:30:00Z","tags":["can","inconsistent"]}]`
	var failWith error
	fr := &backup.FakeRunner{Handle: func(backup.Cmd) ([]byte, error) { return []byte(listing), failWith }}
	seams(t, fr)
	mem := backup.NewMemStore()
	backupSeams.store = func(*backup.Kit) backup.ObjectStore { return mem }
	args := []string{"backup", "snapshots", "--kit", kit, "--passphrase-file", pass}

	code, out, errs := runIn(t, "", args...)
	if code != ExitOK || !strings.Contains(out, "aaaaaaaa") || !strings.Contains(out, "usable by a restore: 1") || !strings.Contains(out, "erasure ledger: none") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
	c := fr.Calls[0]
	if !c.CleanEnv || !slices.Contains(c.Args, "--no-lock") || !slices.Contains(c.Args, "--no-cache") ||
		!slices.Contains(c.Env, "RESTIC_REPOSITORY=s3:https://s3.example.invalid/acme/restic") || !slices.Contains(c.Env, "AWS_SECRET_ACCESS_KEY=fakesecret") {
		t.Errorf("restic call: %v %v clean=%v", c.Args, c.Env, c.CleanEnv)
	}
	if strings.Contains(out+errs, "fakesecret") {
		t.Error("a credential was printed")
	}

	// A ledger the kit cannot decrypt fails the gate.
	if err := mem.Put(context.Background(), backup.LedgerObjectKey, []byte("sealed under another password")); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runIn(t, "", args...); code != ExitFailure || !strings.Contains(errs, "erasure ledger") {
		t.Errorf("bad ledger: %d %q", code, errs)
	}
	mem = backup.NewMemStore()
	listing = "[]"
	if code, _, errs := runIn(t, "", args...); code != ExitFailure || !strings.Contains(errs, "no snapshot") {
		t.Errorf("empty repository: %d %q", code, errs)
	}
	failWith = &backup.CmdError{Cmd: "restic", Code: 12, Stderr: "wrong password"}
	if code, _, errs := runIn(t, "", args...); code != ExitFailure || !strings.Contains(errs, "does NOT open") {
		t.Errorf("wrong password: %d %q", code, errs)
	}
	if code, _, _ := runIn(t, "", "backup", "snapshots"); code != ExitUsage {
		t.Errorf("without a kit: %d", code)
	}
}

func TestCleanEnvLeavesTheShellBehind(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD", "from-the-shell")
	t.Setenv("AWS_SESSION_TOKEN", "from-the-shell")
	out, err := backup.ExecRunner{}.Run(context.Background(), backup.Cmd{Name: "env", Env: []string{"ONLY=this"}, CleanEnv: true})
	if err != nil {
		t.Skip("no env command:", err)
	}
	if s := string(out); strings.Contains(s, "from-the-shell") || !strings.Contains(s, "ONLY=this") {
		t.Errorf("environment: %s", s)
	}
}
