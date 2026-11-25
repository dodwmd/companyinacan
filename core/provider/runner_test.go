package provider

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSSHArgs(t *testing.T) {
	args, err := SSHArgs(Target{Host: "203.0.113.7", User: "deploy", Port: 2222, KeyFile: "/k/id"}, nil, "bash -s")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"BatchMode=yes", "-p 2222", "-i /k/id", "IdentitiesOnly=yes", "deploy@203.0.113.7 bash -s"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	// defaults
	args, _ = SSHArgs(Target{Host: "h.example"}, nil, "")
	if !strings.Contains(strings.Join(args, " "), "-p 22") || args[len(args)-1] != "root@h.example" {
		t.Errorf("defaults wrong: %v", args)
	}
}

func TestSSHArgsRejectInjection(t *testing.T) {
	for _, tg := range []Target{
		{Host: "-oProxyCommand=evil"}, {Host: "a b"}, {Host: "h;ls"}, {Host: ""},
		{Host: "h", User: "root;ls"}, {Host: "h", User: "-x"},
	} {
		if _, err := SSHArgs(tg, nil, ""); err == nil {
			t.Errorf("%+v: want error", tg)
		}
	}
}

// helper-process pattern: the "ssh" binary is this test binary.
func fakeCommand(stdoutFile string) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cs := append([]string{"-test.run=TestHelperProcess", "--", name}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], cs...)
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER=1", "HELPER_OUT="+stdoutFile)
		return cmd
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER") != "1" {
		return
	}
	// record argv and stdin to HELPER_OUT, then echo a marker
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	in, _ := os.ReadFile("/dev/stdin")
	_ = os.WriteFile(os.Getenv("HELPER_OUT"), []byte(strings.Join(args, "\x00")+"\x01"+string(in)), 0o600)
	_, _ = os.Stdout.WriteString("remote-output\n")
	os.Exit(0)
}

func TestSSHRunnerRunAndCopy(t *testing.T) {
	rec := t.TempDir() + "/rec"
	r := &SSHRunner{T: Target{Host: "203.0.113.7", User: "deploy"}, Command: fakeCommand(rec)}

	out, err := r.Run(context.Background(), "echo hi")
	if err != nil || out != "remote-output\n" {
		t.Fatalf("run: %q %v", out, err)
	}
	b, _ := os.ReadFile(rec)
	argv, stdin, _ := strings.Cut(string(b), "\x01")
	if !strings.HasPrefix(argv, "ssh\x00") || !strings.HasSuffix(argv, "deploy@203.0.113.7\x00sudo -n bash -s") {
		t.Errorf("argv: %q", argv)
	}
	if stdin != "echo hi" {
		t.Errorf("script must travel on stdin, got %q", stdin)
	}

	if err := r.Copy(context.Background(), "/usr/local/sbin/x.sh", []byte("DATA"), "0700"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(rec)
	argv, stdin, _ = strings.Cut(string(b), "\x01")
	if stdin != "DATA" || !strings.Contains(argv, "sudo -n tee '/usr/local/sbin/x.sh'") || !strings.Contains(argv, "chmod 0700") {
		t.Errorf("copy: argv=%q stdin=%q", argv, stdin)
	}
}

func TestSSHRunnerRootDoesNotSudo(t *testing.T) {
	rec := t.TempDir() + "/rec"
	r := &SSHRunner{T: Target{Host: "h.example"}, Command: fakeCommand(rec)}
	if _, err := r.Run(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(rec)
	if strings.Contains(string(b), "sudo") {
		t.Error("root must not use sudo")
	}
}

func TestSSHRunnerCopyValidates(t *testing.T) {
	r := &SSHRunner{T: Target{Host: "h"}, Command: fakeCommand(t.TempDir() + "/rec")}
	for _, p := range []string{"relative", "/a/../b", "/a b", "/a';rm", ""} {
		if err := r.Copy(context.Background(), p, nil, "0600"); err == nil {
			t.Errorf("path %q: want error", p)
		}
	}
	if err := r.Copy(context.Background(), "/ok", nil, "777"); err == nil {
		t.Error("bad mode: want error")
	}
}

func TestSSHRunnerReportsFailure(t *testing.T) {
	r := &SSHRunner{T: Target{Host: "h"}, Command: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}}
	if _, err := r.Run(context.Background(), "x"); err == nil {
		t.Fatal("want error")
	}
}
