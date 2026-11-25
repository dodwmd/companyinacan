//go:build integration

// Package integration holds cluster-backed tests (epic E02). They need a running
// `make dev` cluster (k3d + docker + kubectl) and are excluded from `make test`.
// Run: make dev && go test -tags integration -count=1 ./test/integration/...
//
// STATUS: written without access to a cluster; never executed by the author. Treat the
// first real run as the verification and fix whatever it reveals.
package integration

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func context() string {
	if c := os.Getenv("CAN_DEV_CONTEXT"); c != "" {
		return c
	}
	return "k3d-companyinacan-dev"
}

func kubectl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("kubectl", append([]string{"--context", context()}, args...)...).CombinedOutput() //nolint:gosec // test helper
	return strings.TrimSpace(string(out)), err
}

func mustKubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectl(t, args...)
	if err != nil {
		t.Fatalf("kubectl %v: %v\n%s", args, err, out)
	}
	return out
}

func TestPlatformNamespacesExist(t *testing.T) {
	for _, ns := range []string{"can-system", "can-data", "can-portal"} {
		mustKubectl(t, "get", "namespace", ns)
		if out := mustKubectl(t, "-n", ns, "get", "networkpolicy", "default-deny", "-o", "jsonpath={.spec.policyTypes}"); !strings.Contains(out, "Ingress") || !strings.Contains(out, "Egress") {
			t.Errorf("%s default-deny policyTypes=%s", ns, out)
		}
	}
}

func TestTraefikPersistsACMEStore(t *testing.T) {
	out := mustKubectl(t, "-n", "kube-system", "get", "pvc", "-o", "name")
	if !strings.Contains(out, "traefik") {
		t.Fatalf("no Traefik PVC found:\n%s", out)
	}
	policy := mustKubectl(t, "-n", "kube-system", "get", "svc", "traefik", "-o", "jsonpath={.spec.externalTrafficPolicy}")
	if policy != "Local" {
		t.Errorf("externalTrafficPolicy=%q", policy)
	}
}

// probe runs a throw-away pod in ns and returns its output; the pod is removed afterwards.
func probe(t *testing.T, ns, name string, labels string, script string) (string, error) {
	t.Helper()
	args := []string{"-n", ns, "run", name, "--rm", "-i", "--restart=Never", "--quiet",
		"--image=busybox:1.37.0", "--labels=" + labels, "--command", "--", "sh", "-c", script}
	return kubectl(t, args...)
}

func TestDNSRewriteLocaltestMe(t *testing.T) {
	traefik := mustKubectl(t, "-n", "kube-system", "get", "svc", "traefik", "-o", "jsonpath={.spec.clusterIP}")
	// can-data is default-deny with DNS allowed, so this also proves the DNS allow works.
	out, err := probe(t, "can-data", "dns-probe", "app=probe", "nslookup foo.localtest.me")
	if err != nil || !strings.Contains(out, traefik) {
		t.Fatalf("foo.localtest.me should resolve to the Traefik ClusterIP %s: %v\n%s", traefik, err, out)
	}
	out, _ = probe(t, "can-data", "dns-probe-admin", "app=probe", "nslookup admin.localtest.me")
	if strings.Contains(out, traefik) {
		t.Fatalf("admin.localtest.me must not be rewritten to Traefik:\n%s", out)
	}
}

func TestDefaultDenyBlocksPodToPod(t *testing.T) {
	// Two pods in can-data: default-deny (egress allows only DNS; ingress only from can-core
	// and module namespaces) means the probe must not reach the listener.
	mustKubectl(t, "-n", "can-data", "run", "deny-target", "--image=busybox:1.37.0", "--restart=Never",
		"--command", "--", "sh", "-c", "nc -lk -p 5432")
	t.Cleanup(func() {
		_, _ = kubectl(t, "-n", "can-data", "delete", "pod", "deny-target", "--now", "--ignore-not-found")
	})
	mustKubectl(t, "-n", "can-data", "wait", "--for=condition=Ready", "pod/deny-target", "--timeout=120s")
	ip := mustKubectl(t, "-n", "can-data", "get", "pod", "deny-target", "-o", "jsonpath={.status.podIP}")
	out, _ := probe(t, "can-data", "deny-probe", "app=probe", "nc -z -w 3 "+ip+" 5432 && echo REACHED")
	if strings.Contains(out, "REACHED") {
		t.Fatalf("probe reached %s:5432 despite default-deny", ip)
	}
}

func TestCoreDNSCustomLoaded(t *testing.T) {
	out := mustKubectl(t, "-n", "kube-system", "get", "configmap", "coredns-custom", "-o", "jsonpath={.data}")
	if !strings.Contains(out, "localtest") {
		t.Fatalf("coredns-custom missing rewrite: %s", out)
	}
}
