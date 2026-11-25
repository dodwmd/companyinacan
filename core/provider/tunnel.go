package provider

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// K3sAPIPort is the k3s API server port on the host. It is never exposed by
// the firewall; the CLI reaches it through an SSH tunnel.
const K3sAPIPort = 6443

// K3sKubeconfigPath is where k3s writes its admin kubeconfig on the host.
const K3sKubeconfigPath = "/etc/rancher/k3s/k3s.yaml"

// FreeLocalPort asks the kernel for an unused loopback port.
func FreeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// TunnelArgs returns the ssh arguments forwarding 127.0.0.1:localPort to the
// k3s API on the host's loopback.
func TunnelArgs(t Target, localPort int) ([]string, error) {
	if localPort < 1024 || localPort > 65535 {
		return nil, fmt.Errorf("invalid local port %d", localPort)
	}
	fwd := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, K3sAPIPort)
	return SSHArgs(t, []string{"-N", "-o", "ExitOnForwardFailure=yes", "-L", fwd}, "")
}

// Tunnel is a running ssh -L process.
type Tunnel struct {
	Port int
	cmd  *exec.Cmd
}

// DefaultTunnelPort is the local port written into the retrieved kubeconfig.
const DefaultTunnelPort = 16443

// OpenTunnel starts the tunnel on port (0 = any free port) and waits until the
// local port accepts connections (or timeout). Close stops it.
func OpenTunnel(ctx context.Context, t Target, port int, timeout time.Duration) (*Tunnel, error) {
	if port == 0 {
		var err error
		if port, err = FreeLocalPort(); err != nil {
			return nil, err
		}
	}
	args, err := TunnelArgs(t, port)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh tunnel: %w", err)
	}
	tun := &Tunnel{Port: port, cmd: cmd}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return tun, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	tun.Close()
	return nil, fmt.Errorf("ssh tunnel to %s did not come up within %s", t.Host, timeout)
}

// Close stops the tunnel process.
func (t *Tunnel) Close() {
	if t == nil || t.cmd == nil || t.cmd.Process == nil {
		return
	}
	_ = t.cmd.Process.Kill()
	_ = t.cmd.Wait()
}

var reServer = regexp.MustCompile(`(?m)^(\s*server:\s*)https://[^\s]+\s*$`)

// RewriteKubeconfig points a k3s kubeconfig at the local end of the SSH tunnel
// and renames the cluster/context/user from "default" to name.
func RewriteKubeconfig(raw string, localPort int, name string) (string, error) {
	if !reServer.MatchString(raw) {
		return "", fmt.Errorf("kubeconfig has no https server entry")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString(name) {
		return "", fmt.Errorf("invalid kubeconfig name %q", name)
	}
	out := reServer.ReplaceAllString(raw, fmt.Sprintf("${1}https://127.0.0.1:%d", localPort))
	out = regexp.MustCompile(`(?m)^(\s*(?:-\s+)?(?:name|cluster|user|current-context):\s*)default\s*$`).ReplaceAllString(out, "${1}"+name)
	return out, nil
}

// KubeconfigPath is where the retrieved kubeconfig is stored.
func KubeconfigPath(dir string) string { return filepath.Join(dir, ".can", "kubeconfig") }

// FetchKubeconfig reads the k3s kubeconfig from the host, rewrites it for the
// tunnel and stores it (mode 0600) under dir/.can/kubeconfig.
func FetchKubeconfig(ctx context.Context, r Runner, dir, name string, localPort int) (string, error) {
	raw, err := r.Run(ctx, "cat "+K3sKubeconfigPath)
	if err != nil {
		return "", fmt.Errorf("read kubeconfig: %w", err)
	}
	if !strings.Contains(raw, "apiVersion:") {
		return "", fmt.Errorf("unexpected kubeconfig output from host")
	}
	out, err := RewriteKubeconfig(raw, localPort, name)
	if err != nil {
		return "", err
	}
	p := KubeconfigPath(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(out), 0o600); err != nil {
		return "", err
	}
	return p, nil
}
