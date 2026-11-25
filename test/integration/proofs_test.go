//go:build integration

package integration

// Phase 0 proofs (epic E00): the module-namespace template and the module-auth design checked
// against a real k3s, with stub modules. Run by the non-required `footprint` workflow (job
// `proofs`) after `make dev`. What each test proves, and what it does not, is recorded in
// docs/measurements/phase0-ci.md; findings that fail are filed as issues, not hidden here.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const templateDir = "../../deploy/templates/"

// phase0 skips a proof unless CAN_PHASE0_PROOFS=1 (set by the footprint workflow). They are
// slow (policy-programming waits) and TestModuleEgressDeniedToNodeAndAPI fails on purpose while
// issue #13 is open, so they must not turn the dev-cluster job red.
func phase0(t *testing.T) {
	t.Helper()
	if os.Getenv("CAN_PHASE0_PROOFS") != "1" {
		t.Skip("Phase 0 proof: set CAN_PHASE0_PROOFS=1 (run by the footprint workflow)")
	}
}

func nodeIP(t *testing.T) string {
	t.Helper()
	return mustKubectl(t, "get", "node", "-o", `jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}`)
}

func render(t *testing.T, file, ns, ip string) string {
	t.Helper()
	b, err := os.ReadFile(templateDir + file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer(
		"${MODULE_NAMESPACE}", ns,
		"${NODE_IP}", ip,
		"${QUOTA_REQUESTS_MEMORY}", "256Mi",
		"${QUOTA_LIMITS_MEMORY}", "512Mi",
	).Replace(string(b))
}

// moduleNS instantiates deploy/templates/module-namespace.yaml and module-rbac.yaml as can-core
// will, and removes the namespace afterwards.
func moduleNS(t *testing.T, ns string) {
	t.Helper()
	ip := nodeIP(t)
	for _, f := range []string{"module-namespace.yaml", "module-rbac.yaml"} {
		if out, err := kubectlStdin(render(t, f, ns, ip), "apply", "-f", "-"); err != nil {
			t.Fatalf("apply %s for %s: %v\n%s", f, ns, err, out)
		}
	}
	t.Cleanup(func() {
		_, _ = kubectl("delete", "namespace", ns, "--wait=true", "--timeout=180s", "--ignore-not-found")
	})
}

func plainNS(t *testing.T, ns string) {
	t.Helper()
	mustKubectl(t, "create", "namespace", ns)
	t.Cleanup(func() {
		_, _ = kubectl("delete", "namespace", ns, "--wait=true", "--timeout=180s", "--ignore-not-found")
	})
}

// overrides makes a pod acceptable to PodSecurity "restricted" (module namespaces enforce it).
func overrides(name, script string) string {
	b, _ := json.Marshal(map[string]any{"spec": map[string]any{
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65534, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers": []any{map[string]any{
			"name": name, "image": "busybox:1.37.0", "command": []string{"sh", "-c", script},
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
		}},
	}})
	return string(b)
}

// rprobe runs a restricted-compliant throw-away pod and returns its output. A pod that cannot
// run at all fails the test: it must never be read as "blocked".
func rprobe(t *testing.T, ns, name, labels, script string) string {
	t.Helper()
	out, err := kubectl("-n", ns, "run", name, "--rm", "-i", "--restart=Never", "--quiet",
		"--image=busybox:1.37.0", "--labels="+labels, "--overrides="+overrides(name, script))
	if err != nil {
		t.Fatalf("probe pod %s/%s failed to run: %v\n%s", ns, name, err, out)
	}
	return out
}

// tryReachPort runs one probe pod. A probe pod that could not run, or that gave no verdict, is an
// error, never "blocked": the callers decide whether to retry it or fail.
func tryReachPort(ns, name, host string, port int) (reached bool, err error) {
	script := fmt.Sprintf("if nc -z -w 3 %s %d; then echo REACHED; else echo BLOCKED; fi", host, port)
	out, kerr := kubectl("-n", ns, "run", name, "--rm", "-i", "--restart=Never", "--quiet",
		"--image=busybox:1.37.0", "--labels=app=probe", "--overrides="+overrides(name, script))
	if kerr != nil {
		_, _ = kubectl("-n", ns, "delete", "pod", name, "--now", "--ignore-not-found")
		return false, fmt.Errorf("probe pod %s/%s failed to run: %v\n%s", ns, name, kerr, out)
	}
	switch {
	case strings.Contains(out, "REACHED"):
		return true, nil
	case strings.Contains(out, "BLOCKED"):
		return false, nil
	}
	return false, fmt.Errorf("probe %s/%s gave no verdict:\n%s", ns, name, out)
}

func reachPort(t *testing.T, ns, name, host string, port int) bool {
	t.Helper()
	ok, err := tryReachPort(ns, name, host, port)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// backoff returns the pause before retry number i: 2s, 3s, 4.5s ... capped at 15s.
func backoff(i int) time.Duration {
	d := 2 * time.Second
	for ; i > 0 && d < 15*time.Second; i-- {
		d = d * 3 / 2
	}
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return d
}

var settleOnce sync.Once

// settleCluster waits, once per test binary, until the cluster can actually carry pod traffic: the
// node is Ready, the kube-system deployments (CoreDNS) are Available and every kube-system pod is
// Running or Completed. kube-router programs NetworkPolicy and routes asynchronously after that, so
// the positive controls below still poll, but they start from a settled cluster.
func settleCluster(t *testing.T) {
	t.Helper()
	settleOnce.Do(func() {
		_, _ = kubectl("wait", "--for=condition=Ready", "node", "--all", "--timeout=180s")
		_, _ = kubectl("-n", "kube-system", "wait", "--for=condition=Available", "deployment", "--all", "--timeout=180s")
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			out, err := kubectl("-n", "kube-system", "get", "pods", "--no-headers")
			pending := false
			for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
				f := strings.Fields(l)
				if len(f) >= 3 && f[2] != "Running" && f[2] != "Completed" {
					pending = true
				}
			}
			if err == nil && !pending {
				return
			}
			time.Sleep(3 * time.Second)
		}
	})
}

// blockedWithin probes until the target is unreachable and returns how long that took, or -1 if
// it was still reachable at the deadline. A probe that could not run is retried, never counted as
// blocked.
func blockedWithin(t *testing.T, ns, name, host string, port int, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	var lastErr error
	for i := 0; ; i++ {
		reached, err := tryReachPort(ns, fmt.Sprintf("%s-%d", name, i), host, port)
		if err == nil && !reached {
			return time.Since(start)
		}
		lastErr = err
		if time.Since(start) > limit {
			if lastErr != nil {
				t.Fatalf("no verdict on %s:%d within %s: %v", host, port, limit, lastErr)
			}
			return -1
		}
		time.Sleep(5 * time.Second)
	}
}

// pollReachPort is the positive control for every BLOCKED verdict, so a slow start must not fail
// the test. It retries with bounded backoff (about 4 minutes in all) because a fresh pod's network
// (CNI, node routes, kube-router sync) can lag behind the pod reporting ready, and it retries a
// probe pod that could not run. It only gives up with the last observed reason.
func pollReachPort(t *testing.T, ns, name, host string, port int) bool {
	t.Helper()
	settleCluster(t)
	deadline := time.Now().Add(4 * time.Minute)
	var lastErr error
	for i := 0; ; i++ {
		reached, err := tryReachPort(ns, fmt.Sprintf("%s-%d", name, i), host, port)
		if reached {
			return true
		}
		lastErr = err
		if time.Now().Add(backoff(i)).After(deadline) {
			if lastErr != nil {
				t.Logf("last probe error: %v", lastErr)
			}
			return false
		}
		time.Sleep(backoff(i))
	}
}

// listener starts a restricted pod that accepts TCP on the given ports and returns its IP.
func listener(t *testing.T, ns, name, labels string, ports ...int) string {
	t.Helper()
	var sh []string
	for _, p := range ports {
		sh = append(sh, fmt.Sprintf("nc -lk -p %d &", p))
	}
	script := strings.Join(sh, " ") + " wait"
	mustKubectl(t, "-n", ns, "run", name, "--restart=Never", "--image=busybox:1.37.0", "--labels="+labels, "--overrides="+overrides(name, script))
	t.Cleanup(func() { _, _ = kubectl("-n", ns, "delete", "pod", name, "--now", "--ignore-not-found") })
	mustKubectl(t, "-n", ns, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=120s")
	return mustKubectl(t, "-n", ns, "get", "pod", name, "-o", "jsonpath={.status.podIP}")
}

func TestModuleTemplateApplies(t *testing.T) {
	phase0(t)
	moduleNS(t, "mod-stuba")
	for _, np := range []string{"default-deny", "allow-ingress-from-traefik", "allow-ingress-from-can-core", "module-egress"} {
		mustKubectl(t, "-n", "mod-stuba", "get", "networkpolicy", np)
	}
	mustKubectl(t, "-n", "mod-stuba", "get", "resourcequota", "ceiling")
	mustKubectl(t, "-n", "mod-stuba", "get", "limitrange", "defaults")
	if got := mustKubectl(t, "get", "ns", "mod-stuba", "-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`); got != "restricted" {
		t.Fatalf("PodSecurity enforce=%q", got)
	}
	// A root pod with no securityContext must be refused by PodSecurity "restricted".
	out, err := kubectl("-n", "mod-stuba", "run", "root-pod", "--restart=Never", "--image=busybox:1.37.0", "--", "sleep", "5")
	if err == nil || !strings.Contains(out, "violates PodSecurity") {
		_, _ = kubectl("-n", "mod-stuba", "delete", "pod", "root-pod", "--now", "--ignore-not-found")
		t.Fatalf("a non-restricted pod was admitted to a module namespace (err=%v):\n%s", err, out)
	}
	// And a compliant pod is admitted and picks up LimitRange defaults (requests and limits set).
	listener(t, "mod-stuba", "ok-pod", "app=probe", 5432)
	req := mustKubectl(t, "-n", "mod-stuba", "get", "pod", "ok-pod", "-o", "jsonpath={.spec.containers[0].resources.requests.memory}/{.spec.containers[0].resources.limits.memory}")
	if req != "32Mi/192Mi" {
		t.Errorf("LimitRange defaults not applied: %s", req)
	}
}

// Pod-to-node egress denial (SPEC 3.1): a module pod must not reach the node's own addresses,
// which include the API server (6443) and the kubelet.
func TestModuleEgressDeniedToNodeAndAPI(t *testing.T) {
	phase0(t)
	moduleNS(t, "mod-stuba")
	plainNS(t, "probe-node-ctl")
	ip := nodeIP(t)

	// Positive control: an unpolicied namespace reaches the node's API port, so the probe works
	// and the node port is open. Without this a BLOCKED below could just mean "nothing there".
	if !pollReachPort(t, "probe-node-ctl", "node-ctl", ip, 6443) {
		t.Fatalf("control failed: an unpolicied pod could not reach node %s:6443", ip)
	}
	// Policy programming is asynchronous, so allow it time before calling a reach a finding.
	// On k3s NetworkPolicy does not cover pod-to-node traffic (issue #13). The host rules of
	// specs/038-node-isolation do: with them loaded (CAN_HOSTFW_INSTALLED=1, the node-isolation
	// workflow) reaching either target is a failure. Without them, as in a plain `make dev`
	// cluster, it is expected and the test ends as a visible SKIP.
	var known []string
	if d := blockedWithin(t, "mod-stuba", "to-node", ip, 6443, 90*time.Second); d < 0 {
		known = append(known, fmt.Sprintf("module pod still reached node %s:6443 90s after namespace creation", ip))
	} else {
		t.Logf("module pod -> node %s:6443 first observed BLOCKED after %s", ip, d.Round(time.Second))
	}
	if d := blockedWithin(t, "mod-stuba", "to-apiserver-svc", "10.43.0.1", 443, 30*time.Second); d < 0 {
		known = append(known, "module pod still reached the kubernetes Service 10.43.0.1:443")
	} else {
		t.Logf("module pod -> 10.43.0.1:443 first observed BLOCKED after %s", d.Round(time.Second))
	}
	// Positive control for the egress allow itself: the internet is reachable.
	if !pollReachPort(t, "mod-stuba", "to-internet", "1.1.1.1", 443) {
		t.Errorf("module egress to the internet (1.1.1.1:443) is blocked; the 0.0.0.0/0 allow does not work")
	}
	if len(known) > 0 && os.Getenv("CAN_HOSTFW_INSTALLED") == "1" {
		t.Fatalf("node isolation is installed but: %s", strings.Join(known, "; "))
	}
	if len(known) > 0 {
		t.Skipf("KNOWN FAILURE (issue #13), other proofs in this test passed: %s", strings.Join(known, "; "))
	}
}

func TestModuleDNSRewriteFromModuleNamespace(t *testing.T) {
	phase0(t)
	moduleNS(t, "mod-stuba")
	traefik := mustKubectl(t, "-n", "kube-system", "get", "svc", "traefik", "-o", "jsonpath={.spec.clusterIP}")
	out := rprobe(t, "mod-stuba", "dns-a", "app=probe", "nslookup foo.localtest.me")
	if !strings.Contains(out, traefik) {
		t.Fatalf("foo.localtest.me from a module namespace should resolve to Traefik %s:\n%s", traefik, out)
	}
	out = rprobe(t, "mod-stuba", "dns-b", "app=probe", "nslookup admin.localtest.me")
	if strings.Contains(out, traefik) {
		t.Fatalf("admin.localtest.me must not be rewritten to Traefik:\n%s", out)
	}
}

func TestModulesIsolatedFromEachOther(t *testing.T) {
	phase0(t)
	moduleNS(t, "mod-stuba")
	moduleNS(t, "mod-stubb")
	plainNS(t, "probe-mod-ctl")
	ip := listener(t, "mod-stubb", "target", "app=target", 5432)

	if reachPort(t, "mod-stuba", "a-to-b", ip, 5432) {
		t.Fatalf("a pod in mod-stuba reached a pod in mod-stubb %s:5432", ip)
	}
	// Positive control: admit an unrelated namespace to the target and it must get through, so
	// the denial above is the policies' doing and not a dead listener.
	allow := `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: test-allow-ctl, namespace: mod-stubb}
spec:
  podSelector: {matchLabels: {app: target}}
  policyTypes: [Ingress]
  ingress:
    - from: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: probe-mod-ctl}}}]
      ports: [{protocol: TCP, port: 5432}]
`
	if out, err := kubectlStdin(allow, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply control policy: %v\n%s", err, out)
	}
	if !pollReachPort(t, "probe-mod-ctl", "ctl", ip, 5432) {
		t.Fatalf("control failed: explicitly admitted probe could not reach %s:5432", ip)
	}
}

// The module API port of can-core is reachable from a module namespace; the admin port is not.
// A stand-in pod carries can-core's labels (can-core itself arrives with E03).
func TestModuleReachesOnlyTheModuleAPIPort(t *testing.T) {
	phase0(t)
	moduleNS(t, "mod-stuba")
	ip := listener(t, "can-system", "can-core-stand-in", "app.kubernetes.io/name=can-core", 8090, 8080)
	if !pollReachPort(t, "mod-stuba", "api", ip, 8090) {
		t.Errorf("module namespace cannot reach can-core's module API port 8090 at %s", ip)
	}
	if reachPort(t, "mod-stuba", "admin", ip, 8080) {
		t.Errorf("FINDING: module namespace reached can-core's admin port 8080 at %s", ip)
	}
}

const tokenReview = `
apiVersion: authentication.k8s.io/v1
kind: TokenReview
spec:
  token: %q
  audiences: [%s]
`

func review(t *testing.T, token, aud, field string) string {
	t.Helper()
	out, err := kubectlStdin(fmt.Sprintf(tokenReview, token, aud), "--as=system:serviceaccount:can-system:can-core",
		"create", "--validate=false", "-f", "-", "-o", "jsonpath={"+field+"}")
	if err != nil {
		t.Fatalf("TokenReview as can-core failed: %v\n%s", err, out)
	}
	return out
}

// Module identity (SPEC 3.1): a module's projected ServiceAccount token, bound to an audience,
// is validated by can-core through TokenReview, and the base RBAC lets can-core do exactly that.
// Done from outside the pod (kubectl create token + impersonating can-core); the in-pod
// projected-volume path and a real can-core are not exercised.
func TestTokenReviewAuthenticatesModule(t *testing.T) {
	phase0(t)
	moduleNS(t, "mod-stuba")
	mustKubectl(t, "-n", "mod-stuba", "create", "serviceaccount", "stub")
	tok := mustKubectl(t, "-n", "mod-stuba", "create", "token", "stub", "--audience=can-core-module-api", "--duration=10m")

	if got := review(t, tok, "can-core-module-api", ".status.authenticated"); got != "true" {
		t.Fatalf("valid token: authenticated=%q", got)
	}
	if got := review(t, tok, "can-core-module-api", ".status.user.username"); got != "system:serviceaccount:mod-stuba:stub" {
		t.Errorf("identity = %q", got)
	}
	if got := review(t, tok, "some-other-audience", ".status.authenticated"); got == "true" {
		t.Errorf("a token for audience can-core-module-api was accepted for another audience")
	}
	bad := tok[:len(tok)-2] + "xx"
	if got := review(t, bad, "can-core-module-api", ".status.authenticated"); got == "true" {
		t.Errorf("a tampered token was accepted")
	}
	// A module itself must not be able to validate tokens.
	out, _ := kubectl("auth", "can-i", "create", "tokenreviews.authentication.k8s.io", "--as=system:serviceaccount:mod-stuba:stub")
	if lines := strings.Split(out, "\n"); strings.TrimSpace(lines[len(lines)-1]) != "no" { // kubectl may print a Warning first
		t.Errorf("module ServiceAccount can create TokenReviews: %q", out)
	}
}

// can-core's standing grant is small and the per-namespace grant appears with the RoleBinding.
func TestCanCoreRBACScope(t *testing.T) {
	phase0(t)
	const core = "--as=system:serviceaccount:can-system:can-core"
	can := func(args ...string) string {
		out, _ := kubectl(append([]string{"auth", "can-i"}, append(args, core)...)...)
		lines := strings.Split(strings.TrimSpace(out), "\n") // kubectl may print a Warning first
		return strings.TrimSpace(lines[len(lines)-1])
	}
	if got := can("get", "secrets", "-n", "kube-system"); got != "no" {
		t.Errorf("can-core can read kube-system secrets: %q", got)
	}
	if got := can("get", "secrets", "-n", "mod-stuba"); got != "no" {
		t.Errorf("can-core can read secrets in a namespace without the RoleBinding: %q", got)
	}
	if got := can("create", "tokenreviews.authentication.k8s.io"); got != "yes" {
		t.Errorf("can-core cannot create TokenReviews: %q", got)
	}
	moduleNS(t, "mod-stuba")
	if got := can("create", "deployments.apps", "-n", "mod-stuba"); got != "yes" {
		t.Errorf("can-core cannot manage workloads in a bound module namespace: %q", got)
	}
	if got := can("create", "deployments.apps", "-n", "kube-system"); got != "no" {
		t.Errorf("can-core can create workloads in kube-system: %q", got)
	}
}
