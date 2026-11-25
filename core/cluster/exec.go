package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ExecExecutor runs commands on the local machine. Stderr is copied to Stderr (if set)
// and included in returned errors.
type ExecExecutor struct {
	Stderr io.Writer
}

// Run implements Executor.
func (e ExecExecutor) Run(ctx context.Context, c Cmd) (string, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...) //nolint:gosec // fixed binaries (k3d, kubectl); arguments come from validated Config
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if e.Stderr != nil {
		_, _ = io.Copy(e.Stderr, bytes.NewReader(errb.Bytes()))
	}
	if err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return out.String(), fmt.Errorf("%w: %s", err, msg)
		}
		return out.String(), err
	}
	return out.String(), nil
}
