package backup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResticArgvAndCredentials(t *testing.T) {
	fr := &FakeRunner{Handle: func(c Cmd) ([]byte, error) {
		if len(c.Args) > 0 && c.Args[0] == "snapshots" {
			return []byte(`[{"id":"aaaa1111bbbb2222","short_id":"aaaa1111","time":"2026-11-20T03:30:00Z","paths":["/var/lib/can/dumps"],"tags":["can"]},
{"id":"cccc3333dddd4444","short_id":"cccc3333","time":"2026-11-21T03:30:00Z","paths":["/var/lib/can/dumps"],"tags":["can"]}]`), nil
		}
		return nil, nil
	}}
	r := &Restic{R: fr, Repo: Repo{
		Repository: "s3:https://h/b/r", PasswordFile: "/etc/can/backup.d/restic.password",
		Env: []string{"AWS_ACCESS_KEY_ID=k", "AWS_SECRET_ACCESS_KEY=s"},
	}}
	ctx := context.Background()
	snaps, err := r.Snapshots(ctx)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("%v %v", err, snaps)
	}
	if snaps[0].ID != "cccc3333dddd4444" {
		t.Errorf("snapshots must be newest first, got %s", snaps[0].ID)
	}
	if err := r.Restore(ctx, "latest", "/tmp/x", []string{"/var/lib/can/dumps"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := r.Forget(ctx, 35); err != nil {
		t.Fatal(err)
	}
	joined := make([]string, len(fr.Calls))
	for i, c := range fr.Calls {
		joined[i] = c.Name + " " + strings.Join(c.Args, " ")
		env := strings.Join(c.Env, "\n")
		if !strings.Contains(env, "RESTIC_REPOSITORY=s3:https://h/b/r") || !strings.Contains(env, "RESTIC_PASSWORD_FILE=/etc/can/backup.d/restic.password") {
			t.Errorf("call %d lacks repo/password-file env: %v", i, c.Env)
		}
		if strings.Contains(joined[i], "pw") || strings.Contains(joined[i], "AWS_") {
			t.Errorf("credential on argv: %s", joined[i])
		}
	}
	want := []string{
		"restic snapshots --json",
		"restic restore latest --target /tmp/x --include /var/lib/can/dumps",
		"restic check --read-data",
		"restic forget --group-by  --keep-within 35d --prune --tag can",
	}
	for i, w := range want {
		if joined[i] != w {
			t.Errorf("call %d = %q, want %q", i, joined[i], w)
		}
	}
}

func TestEnsureInit(t *testing.T) {
	calls := 0
	fr := &FakeRunner{Handle: func(c Cmd) ([]byte, error) {
		calls++
		if c.Args[0] == "cat" {
			return nil, &CmdError{Code: 10, Stderr: "Is there a repository at the following location?"}
		}
		return nil, nil
	}}
	r := &Restic{R: fr, Repo: Repo{Repository: "r", PasswordFile: "p"}}
	created, err := r.EnsureInit(context.Background())
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	fr2 := &FakeRunner{}
	created, err = (&Restic{R: fr2, Repo: r.Repo}).EnsureInit(context.Background())
	if err != nil || created {
		t.Fatalf("existing repo: created=%v err=%v", created, err)
	}
	// Any other failure (network, bad credentials) must not trigger init.
	fr3 := &FakeRunner{Handle: func(Cmd) ([]byte, error) { return nil, &CmdError{Code: 1, Stderr: "connection refused"} }}
	if _, err := (&Restic{R: fr3, Repo: r.Repo}).EnsureInit(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	for _, c := range fr3.Calls {
		if c.Args[0] == "init" {
			t.Fatal("init attempted after an unrelated failure")
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "restic.env")
	_ = os.WriteFile(p, []byte("# c\nAWS_ACCESS_KEY_ID=abc\nexport AWS_SECRET_ACCESS_KEY=\"s e\"\n\nAWS_DEFAULT_REGION='eu'\n"), 0o600)
	got, err := LoadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"AWS_ACCESS_KEY_ID=abc", "AWS_SECRET_ACCESS_KEY=s e", "AWS_DEFAULT_REGION=eu"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", got)
	}
}

func TestExecRunnerError(t *testing.T) {
	_, err := ExecRunner{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}})
	var ce *CmdError
	if !errors.As(err, &ce) || ce.Code != 3 || !strings.Contains(ce.Stderr, "boom") {
		t.Fatalf("err = %#v", err)
	}
	out, err := ExecRunner{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "cat; echo $X"}, Env: []string{"X=1"}, Stdin: strings.NewReader("in\n")})
	if err != nil || string(out) != "in\n1\n" {
		t.Fatalf("%q %v", out, err)
	}
}

// --- verify, status -------------------------------------------------------

func hashOf(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func fakeRepo(files map[string]string) *FakeRestic {
	f := &FakeRestic{Snaps: []Snapshot{{ID: "snap1", Time: time.Unix(1_800_000_000, 0)}}, Files: map[string][]byte{}}
	var man strings.Builder
	for p, c := range files {
		f.Files["/var/lib/can/dumps/"+p] = []byte(c)
		fmt.Fprintf(&man, "%s  ./%s\n", hashOf([]byte(c)), p)
	}
	f.Files["/var/lib/can/host-state/"+ManifestName] = []byte(man.String())
	return f
}

func TestVerifyOK(t *testing.T) {
	f := fakeRepo(map[string]string{"state.db": "s", "modules/a.dump": "A", "modules/b.dump": "B", "company.yaml": "c"})
	tmp := t.TempDir()
	rep, err := Verify(context.Background(), f, VerifyOptions{DumpsDir: "/var/lib/can/dumps", Sample: 2, TempDir: tmp})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.CheckOK || len(rep.Sampled) != 2 || len(rep.Mismatched) != 0 || rep.Snapshot != "snap1" {
		t.Fatalf("%+v", rep)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("temp restore not cleaned up: %v", left)
	}
	if !f.CheckCalled {
		t.Error("restic check not run")
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	f := fakeRepo(map[string]string{"modules/a.dump": "A"})
	f.Corrupt = map[string][]byte{"/var/lib/can/dumps/modules/a.dump": []byte("X")}
	rep, err := Verify(context.Background(), f, VerifyOptions{DumpsDir: "/var/lib/can/dumps", Sample: 5, TempDir: t.TempDir()})
	if err == nil || len(rep.Mismatched) != 1 {
		t.Fatalf("err=%v rep=%+v", err, rep)
	}
}

func TestVerifyFailures(t *testing.T) {
	ctx := context.Background()
	o := VerifyOptions{DumpsDir: "/var/lib/can/dumps", Sample: 1, TempDir: t.TempDir()}
	f := fakeRepo(map[string]string{"a": "A"})
	f.CheckErr = errors.New("pack 123 damaged")
	if rep, err := Verify(ctx, f, o); err == nil || rep.CheckOK {
		t.Errorf("check failure must fail verify: %v", err)
	}
	empty := &FakeRestic{}
	if _, err := Verify(ctx, empty, o); err == nil {
		t.Error("no snapshots must fail verify")
	}
	noMan := fakeRepo(map[string]string{"a": "A"})
	delete(noMan.Files, "/var/lib/can/host-state/"+ManifestName)
	if _, err := Verify(ctx, noMan, o); err == nil {
		t.Error("missing manifest must fail verify")
	}
}

func TestParseManifestRejectsTraversal(t *testing.T) {
	if _, err := ParseManifest([]byte(strings.Repeat("a", 64) + "  ./../../etc/passwd\n")); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := ParseManifest([]byte("nothex  ./a\n")); err == nil {
		t.Fatal("bad hash accepted")
	}
}

func TestStatus(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	rep := Status(context.Background(), nil, dir, now, 24*time.Hour)
	if rep.Enabled || len(rep.Problems) == 0 {
		t.Errorf("no restic must report backups off: %+v", rep)
	}
	_ = WriteMarker(filepath.Join(dir, MarkerName), Marker{RunID: "r", FinishedAt: now.Add(-time.Hour)})
	f := &FakeRestic{Snaps: []Snapshot{{ID: "s", Time: now.Add(-time.Hour)}}}
	rep = Status(context.Background(), f, dir, now, 24*time.Hour)
	if !rep.Enabled || len(rep.Problems) != 0 || rep.Snapshots != 1 {
		t.Errorf("%+v", rep)
	}
	old := &FakeRestic{Snaps: []Snapshot{{ID: "s", Time: now.Add(-72 * time.Hour)}}}
	rep = Status(context.Background(), old, dir, now, 24*time.Hour)
	if len(rep.Problems) == 0 {
		t.Error("stale snapshot not flagged")
	}
}

func TestEnvFileQuotingRoundTrip(t *testing.T) {
	in := []string{"AWS_ACCESS_KEY_ID=AKIA", "AWS_SECRET_ACCESS_KEY=pa ss'w\"o$rd`x`\\y;&|<>#"}
	p := filepath.Join(t.TempDir(), "restic.env")
	if err := os.WriteFile(p, []byte(FormatEnvFile(in)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadEnvFile(p)
	if err != nil || strings.Join(got, "\n") != strings.Join(in, "\n") {
		t.Fatalf("go parse: %v %q", err, got)
	}
	// The backup script sources the same file with bash: both must agree.
	out, err := exec.Command("bash", "-c", `set -a; . "$1"; printf %s "$AWS_SECRET_ACCESS_KEY"`, "x", p).Output()
	if err != nil || string(out) != strings.TrimPrefix(in[1], "AWS_SECRET_ACCESS_KEY=") {
		t.Fatalf("bash parse: %v %q", err, out)
	}
}
