package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Target is an SSH destination.
type Target struct {
	Host    string
	User    string // default root
	Port    int    // default 22
	KeyFile string // optional identity file
	// KnownHosts, if set, replaces ~/.ssh/known_hosts (used for cloud servers, whose IPs get recycled).
	KnownHosts string
}

func (t Target) user() string {
	if t.User == "" {
		return "root"
	}
	return t.User
}

func (t Target) port() int {
	if t.Port == 0 {
		return 22
	}
	return t.Port
}

// Runner executes work on a remote host. Providers only ever see this
// interface, so tests substitute a fake and never open a socket.
type Runner interface {
	// Run feeds script to `bash -s` as root (via sudo when the user is not
	// root) and returns combined output.
	Run(ctx context.Context, script string) (string, error)
	// Copy writes data to remotePath with the given octal mode (e.g. "0600").
	Copy(ctx context.Context, remotePath string, data []byte, mode string) error
}

// SSHRunner runs commands with the system `ssh` binary, which keeps the
// dependency list small and honours the operator's ssh config and agent.
type SSHRunner struct {
	T Target
	// Command builds the process; replaced in tests.
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// NewSSHRunner returns a runner for t.
func NewSSHRunner(t Target) *SSHRunner {
	return &SSHRunner{T: t, Command: exec.CommandContext}
}

var (
	reHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.:-]*[A-Za-z0-9])?$`)
	reUser = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	rePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	reMode = regexp.MustCompile(`^0[0-7]{3}$`)
)

// SSHArgs returns the argument vector for running remote on t. It is exported
// for tests and for the tunnel, and validates every value that reaches the
// command line.
func SSHArgs(t Target, extra []string, remote string) ([]string, error) {
	if !reHost.MatchString(t.Host) {
		return nil, fmt.Errorf("invalid ssh host %q", t.Host)
	}
	if !reUser.MatchString(t.user()) {
		return nil, fmt.Errorf("invalid ssh user %q", t.User)
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-p", strconv.Itoa(t.port()),
	}
	if t.KeyFile != "" {
		args = append(args, "-i", t.KeyFile, "-o", "IdentitiesOnly=yes")
	}
	if t.KnownHosts != "" {
		args = append(args, "-o", "UserKnownHostsFile="+t.KnownHosts)
	}
	args = append(args, extra...)
	args = append(args, t.user()+"@"+t.Host)
	if remote != "" {
		args = append(args, remote)
	}
	return args, nil
}

func (r *SSHRunner) bash() string {
	if r.T.user() == "root" {
		return "bash -s"
	}
	return "sudo -n bash -s"
}

func (r *SSHRunner) exec(ctx context.Context, remote string, stdin io.Reader) (string, error) {
	args, err := SSHArgs(r.T, nil, remote)
	if err != nil {
		return "", err
	}
	cmd := r.Command(ctx, "ssh", args...)
	cmd.Stdin = stdin
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("ssh %s: %w", r.T.Host, err)
	}
	return out.String(), nil
}

// Run implements Runner.
func (r *SSHRunner) Run(ctx context.Context, script string) (string, error) {
	return r.exec(ctx, r.bash(), strings.NewReader(script))
}

// Copy implements Runner.
func (r *SSHRunner) Copy(ctx context.Context, remotePath string, data []byte, mode string) error {
	return r.CopyFrom(ctx, remotePath, bytes.NewReader(data), mode)
}

// StreamCopier is implemented by runners that can copy from a reader without holding the whole
// file in memory (container image tarballs are tens of MiB).
type StreamCopier interface {
	CopyFrom(ctx context.Context, remotePath string, r io.Reader, mode string) error
}

// CopyFrom implements StreamCopier.
func (r *SSHRunner) CopyFrom(ctx context.Context, remotePath string, data io.Reader, mode string) error {
	if !rePath.MatchString(remotePath) || strings.Contains(remotePath, "..") {
		return fmt.Errorf("invalid remote path %q", remotePath)
	}
	if !reMode.MatchString(mode) {
		return fmt.Errorf("invalid file mode %q", mode)
	}
	sudo := ""
	if r.T.user() != "root" {
		sudo = "sudo -n "
	}
	remote := fmt.Sprintf("umask 077 && %stee '%s' >/dev/null && %schmod %s '%s'", sudo, remotePath, sudo, mode, remotePath)
	_, err := r.exec(ctx, remote, data)
	return err
}
