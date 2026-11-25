//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Diagnostic for the pod-to-node finding (never fails on the result): does ANY NetworkPolicy
// stop a pod from reaching the node's own address in k3s, and does a host firewall rule do it?
// Results are logged and recorded in docs/measurements/phase0-ci.md.
func TestDiagnosePodToNodeEgress(t *testing.T) {
	phase0(t)
	ip := nodeIP(t)
	plainNS(t, "diag-deny")
	deny := `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: default-deny, namespace: diag-deny}
spec: {podSelector: {}, policyTypes: [Ingress, Egress]}
`
	if out, err := kubectlStdin(deny, "apply", "-f", "-"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	other := listener(t, "kube-system", "diag-listener", "app=diag", 5432)
	// Probe repeatedly after the policy exists: shows whether "reachable" is a programming delay.
	for _, wait := range []time.Duration{0, 20 * time.Second, 40 * time.Second} {
		time.Sleep(wait)
		t.Logf("DIAG +%s default-deny only: pod -> node %s:6443 reachable=%v; pod -> kubelet :10250 reachable=%v; pod -> pod %s:5432 reachable=%v",
			wait, ip, reachPort(t, "diag-deny", "dn", ip, 6443), reachPort(t, "diag-deny", "dk", ip, 10250), other, reachPort(t, "diag-deny", "dp", other, 5432))
	}

	// Host firewall: drop pod-CIDR traffic to the node's API port in the node's INPUT chain.
	nodeC := "k3d-" + strings.TrimPrefix(kubeContext(), "k3d-") + "-server-0"
	add := []string{"exec", nodeC, "iptables", "-I", "INPUT", "1", "-s", "10.42.0.0/16", "-d", ip, "-p", "tcp", "--dport", "6443", "-j", "DROP"}
	del := []string{"exec", nodeC, "iptables", "-D", "INPUT", "-s", "10.42.0.0/16", "-d", ip, "-p", "tcp", "--dport", "6443", "-j", "DROP"}
	if out, err := exec.Command("docker", add...).CombinedOutput(); err != nil { //nolint:gosec // test helper
		t.Logf("DIAG could not add the host firewall rule: %v %s", err, out)
		return
	}
	t.Cleanup(func() { _ = exec.Command("docker", del...).Run() }) //nolint:gosec // test helper
	t.Logf("DIAG host iptables INPUT drop (pod CIDR -> node:6443): pod -> node reachable=%v", reachPort(t, "diag-deny", "dfw", ip, 6443))
}
