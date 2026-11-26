//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/canbin"
)

// nodeHost is a provider.Runner over `docker exec`: the k3d node stands in for the server, so the
// real install scripts of core/canbin run there with a real cross-compiled `can`.
type nodeHost struct{ node string }

func (h nodeHost) exec(ctx context.Context, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", "-i", h.node}, args...)...) //nolint:gosec // test helper
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker exec %s: %w: %s", h.node, err, out)
	}
	return string(out), nil
}

func (h nodeHost) Run(ctx context.Context, script string) (string, error) {
	return h.exec(ctx, []byte(script), "sh", "-s")
}

func (h nodeHost) Copy(ctx context.Context, p string, data []byte, mode string) error {
	_, err := h.exec(ctx, data, "sh", "-c", `cat >"$1" && chmod "$2" "$1"`, "sh", p, mode)
	return err
}

// canSource builds `can` for the node from this checkout, as `can up` does from source.
func canSource(t *testing.T) *canbin.Resolver {
	t.Helper()
	r := canbin.NewResolver(canbin.Build{}, "")
	r.Dirs = []string{repoRoot(t)}
	return r
}

// checkNodeCan asserts that the server has a `can` that runs and has what the backup script calls.
func checkNodeCan(t *testing.T, h nodeHost) {
	t.Helper()
	out, err := h.Run(context.Background(), canbin.Path+" version --output json && "+canbin.Path+" backup --help")
	if err != nil || !strings.Contains(out, `"version"`) || !strings.Contains(out, "ledger-import") || !strings.Contains(out, "ledger-sync") {
		t.Fatalf("the server's can: %v\n%s", err, out)
	}
}
