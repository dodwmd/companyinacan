//go:build integration

// Package integration holds cluster-backed tests (epic E02). They need a running
// `make dev` cluster (k3d + docker + kubectl) and are excluded from `make test`.
// Run: make dev && go test -tags integration -count=1 ./test/integration/...
//
// STATUS: the author had no cluster. The non-required `dev-cluster` GitHub workflow ran these
// on a real k3d cluster and all passed (first run, PR #5). Not yet covered: ACME issuance,
// externalTrafficPolicy behaviour end to end, can-core RBAC (no can-core exists yet) and the
// module-namespace templates.
package integration

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func kubeContext() string {
	if c := os.Getenv("CAN_DEV_CONTEXT"); c != "" {
		return c
	}
	return "k3d-companyinacan-dev"
}

func kubectl(args ...string) (string, error) {
	out, err := exec.Command("kubectl", append([]string{"--context", kubeContext()}, args...)...).CombinedOutput() //nolint:gosec // test helper
	return strings.TrimSpace(string(out)), err
}

func kubectlStdin(stdin string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--context", kubeContext()}, args...)...) //nolint:gosec // test helper
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustKubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectl(args...)
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

// probe runs a throw-away busybox pod in ns, prints its output and fails the test if the pod
// itself could not run (a failing probe pod must never be read as "traffic blocked").
func probe(t *testing.T, ns, name, labels, script string) string {
	t.Helper()
	out, err := kubectl("-n", ns, "run", name, "--rm", "-i", "--restart=Never", "--quiet",
		"--image=busybox:1.37.0", "--labels="+labels, "--command", "--", "sh", "-c", script)
	if err != nil {
		t.Fatalf("probe pod %s/%s failed to run: %v\n%s", ns, name, err, out)
	}
	return out
}

func TestDNSRewriteLocaltestMe(t *testing.T) {
	traefik := mustKubectl(t, "-n", "kube-system", "get", "svc", "traefik", "-o", "jsonpath={.spec.clusterIP}")
	// can-data is default-deny with only DNS allowed, so this also proves the DNS allow works.
	out := probe(t, "can-data", "dns-probe", "app=probe", "nslookup foo.localtest.me")
	if !strings.Contains(out, traefik) {
		t.Fatalf("foo.localtest.me should resolve to the Traefik ClusterIP %s:\n%s", traefik, out)
	}
	// admin.* is pinned: it must resolve to the real public answer (127.0.0.1), not Traefik.
	out = probe(t, "can-data", "dns-probe-admin", "app=probe", "nslookup admin.localtest.me")
	if strings.Contains(out, traefik) {
		t.Fatalf("admin.localtest.me must not be rewritten to Traefik:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1") {
		t.Fatalf("admin.localtest.me should resolve to its public address 127.0.0.1 (upstream DNS reachable?):\n%s", out)
	}
}

const reachScript = "if nc -z -w 3 %s 5432; then echo REACHED; else echo BLOCKED; fi"

func reach(t *testing.T, ns, name, labels, ip string) bool {
	t.Helper()
	out := probe(t, ns, name, labels, strings.Replace(reachScript, "%s", ip, 1))
	switch {
	case strings.Contains(out, "REACHED"):
		return true
	case strings.Contains(out, "BLOCKED"):
		return false
	}
	t.Fatalf("probe %s/%s gave no verdict:\n%s", ns, name, out)
	return false
}

func TestDefaultDenyBlocksPodToPod(t *testing.T) {
	mustKubectl(t, "-n", "can-data", "run", "deny-target", "--image=busybox:1.37.0", "--restart=Never",
		"--labels=app=deny-target", "--command", "--", "sh", "-c", "nc -lk -p 5432")
	t.Cleanup(func() {
		_, _ = kubectl("-n", "can-data", "delete", "pod", "deny-target", "--now", "--ignore-not-found")
	})
	mustKubectl(t, "-n", "can-data", "wait", "--for=condition=Ready", "pod/deny-target", "--timeout=120s")
	ip := mustKubectl(t, "-n", "can-data", "get", "pod", "deny-target", "-o", "jsonpath={.status.podIP}")

	// Denied by default (egress of the probe and ingress of the target are both default-deny).
	if reach(t, "can-data", "deny-probe", "app=probe", ip) {
		t.Fatalf("probe reached %s:5432 despite default-deny", ip)
	}

	// Positive control: add explicit allows for exactly this pair and the same probe must get
	// through. This proves the listener works and the probe is able to succeed, so the denial
	// above is the policy's doing and not a broken probe.
	allow := `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: test-allow-probe-egress, namespace: can-data}
spec:
  podSelector: {matchLabels: {app: probe}}
  policyTypes: [Egress]
  egress:
    - to: [{podSelector: {matchLabels: {app: deny-target}}}]
      ports: [{protocol: TCP, port: 5432}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: test-allow-target-ingress, namespace: can-data}
spec:
  podSelector: {matchLabels: {app: deny-target}}
  policyTypes: [Ingress]
  ingress:
    - from: [{podSelector: {matchLabels: {app: probe}}}]
      ports: [{protocol: TCP, port: 5432}]
`
	if out, err := kubectlStdin(allow, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply control policies: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl("-n", "can-data", "delete", "networkpolicy", "test-allow-probe-egress", "test-allow-target-ingress", "--ignore-not-found")
	})
	if !pollReach(t, "can-data", "allow-probe", "app=probe", ip) {
		t.Fatalf("positive control failed: explicitly allowed probe could not reach %s:5432", ip)
	}
}

// pollReach retries with bounded backoff (about 2 minutes in all): policy programming by
// kube-router is asynchronous, so a probe made right after the policy is applied may still be denied.
func pollReach(t *testing.T, ns, name, labels, ip string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for i := 0; ; i++ {
		if reach(t, ns, fmt.Sprintf("%s-%d", name, i), labels, ip) {
			return true
		}
		if time.Now().Add(backoff(i)).After(deadline) {
			return false
		}
		time.Sleep(backoff(i))
	}
}

func TestDefaultDenyBlocksCrossNamespaceIngress(t *testing.T) {
	mustKubectl(t, "-n", "can-data", "run", "ingress-target", "--image=busybox:1.37.0", "--restart=Never",
		"--labels=app=ingress-target", "--command", "--", "sh", "-c", "nc -lk -p 5432")
	t.Cleanup(func() {
		_, _ = kubectl("-n", "can-data", "delete", "pod", "ingress-target", "--now", "--ignore-not-found")
	})
	mustKubectl(t, "-n", "can-data", "wait", "--for=condition=Ready", "pod/ingress-target", "--timeout=120s")
	ip := mustKubectl(t, "-n", "can-data", "get", "pod", "ingress-target", "-o", "jsonpath={.status.podIP}")

	// An unpolicied namespace has unrestricted egress, so a blocked connection is purely the
	// ingress default-deny (and the absence of an allow for this source) in can-data.
	mustKubectl(t, "create", "namespace", "probe-ext")
	t.Cleanup(func() { _, _ = kubectl("delete", "namespace", "probe-ext", "--wait=false", "--ignore-not-found") })
	if reach(t, "probe-ext", "ext-probe", "app=probe", ip) {
		t.Fatalf("a pod in an unrelated namespace reached can-data %s:5432", ip)
	}

	// Positive control: explicitly admit probe-ext to the target and the same probe must get
	// through, so the BLOCKED above means "denied by policy", not "not listening".
	allow := `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: test-allow-ext-ingress, namespace: can-data}
spec:
  podSelector: {matchLabels: {app: ingress-target}}
  policyTypes: [Ingress]
  ingress:
    - from: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: probe-ext}}}]
      ports: [{protocol: TCP, port: 5432}]
`
	if out, err := kubectlStdin(allow, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply control policy: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl("-n", "can-data", "delete", "networkpolicy", "test-allow-ext-ingress", "--ignore-not-found")
	})
	if !pollReach(t, "probe-ext", "ext-allow-probe", "app=probe", ip) {
		t.Fatalf("positive control failed: explicitly admitted probe could not reach %s:5432", ip)
	}
}

func TestCoreDNSCustomLoaded(t *testing.T) {
	out := mustKubectl(t, "-n", "kube-system", "get", "configmap", "coredns-custom", "-o", "jsonpath={.data}")
	if !strings.Contains(out, "localtest") {
		t.Fatalf("coredns-custom missing rewrite: %s", out)
	}
}
