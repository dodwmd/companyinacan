package launch

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Kube runs kubectl. Everything that changes the cluster goes through it, so tests substitute a
// fake and the argument vector is the only thing a process listing can show: secrets are never
// arguments, they travel on stdin.
type Kube interface {
	Run(ctx context.Context, stdin []byte, args ...string) (stdout string, err error)
}

// ExecKube is Kube over the kubectl binary.
type ExecKube struct {
	Bin        string // default kubectl
	Kubeconfig string // --kubeconfig; empty = kubectl's default resolution
	Context    string // --context; empty = the kubeconfig's current context
}

// Run implements Kube.
func (k ExecKube) Run(ctx context.Context, stdin []byte, args ...string) (string, error) {
	bin := k.Bin
	if bin == "" {
		bin = "kubectl"
	}
	var full []string
	if k.Kubeconfig != "" {
		full = append(full, "--kubeconfig", k.Kubeconfig)
	}
	if k.Context != "" {
		full = append(full, "--context", k.Context)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, bin, full...) //nolint:gosec // fixed binary; arguments are built by this package from validated values
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out.String(), nil
}
