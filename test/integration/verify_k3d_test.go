//go:build integration

package integration

// `can verify --server` (specs/052-real-server-verification) against what `can up` just deployed.
// The k3d node stands in for the server: the same scripts run there through `docker exec` instead
// of ssh, so the kubectl queries, the audit export and the edge limit are the real ones. The host
// half (systemd, ufw, the node firewall, certificates) does not exist in k3d and is not asserted.

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/verify"
)

type nodeRunner struct{}

func (nodeRunner) Run(ctx context.Context, script string) (string, error) {
	// k3d writes the node's kubeconfig where K3S_KUBECONFIG_OUTPUT says, not at the path a server has.
	const shell = `export KUBECONFIG="${K3S_KUBECONFIG_OUTPUT:-/etc/rancher/k3s/k3s.yaml}"; exec sh -s`
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", nodeName(), "sh", "-c", shell) //nolint:gosec // test helper
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (nodeRunner) Copy(context.Context, string, []byte, string) error {
	return errors.New("verify must not copy anything to the server")
}

func verifyOnK3d(t *testing.T, domain string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	checks, facts := verify.CollectServer(ctx, nodeRunner{}, verify.ServerOptions{})
	routes := facts.Routes()
	edgeNet := verify.Net{Now: time.Now, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(address)
		if err != nil || port != "80" {
			return nil, errors.New("only the load balancer's port 80 is used")
		}
		return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:80")
	}}
	limit := len(facts["login_limit"]) > 0 && facts["login_limit"][0] != ""
	checks = append(checks, verify.CheckHTTP(ctx, edgeNet, "127.0.0.1", domain, routes)...)
	checks = append(checks,
		verify.CheckClientIP(ctx, edgeNet, nodeRunner{}, "127.0.0.1", domain, routes),
		verify.CheckRateLimit(ctx, edgeNet, "127.0.0.1", domain, routes, limit))
	got := map[string]verify.Check{}
	for _, c := range checks {
		got[c.ID] = c
		t.Logf("verify: %-4s %-28s %s", c.Status, c.ID, c.Detail)
	}
	for _, id := range []string{"server.ssh", "server.k3s", "server.memory", "cluster.workloads", "edge.client-address-policy", "public.site", "public.admin", "edge.rate-limit"} {
		if got[id].Status != verify.Pass {
			t.Errorf("can verify on k3d: %s is %q: %s", id, got[id].Status, got[id].Detail)
		}
	}
	// k3d's load balancer hides the real client address, so either verdict is right here; a skip
	// would mean the probe could not read can-core's audit log.
	if got["edge.client-address"].Status == verify.Skip {
		t.Errorf("can verify on k3d: no client address verdict: %s", got["edge.client-address"].Detail)
	}
}
