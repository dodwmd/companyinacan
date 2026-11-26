//go:build integration

package integration

// Node isolation (specs/038-node-isolation) against a real k3s in k3d, with its real kube-proxy,
// flannel and kube-router. The k3d node has no nft or systemd, so the shipped can-firewall script
// runs (apply, then its sync loop) in a sidecar that shares the node's network namespace; it can
// change nothing outside that namespace. Every denial is observed reachable first.
//
// Run by the node-isolation workflow: CAN_IT_HOSTFW=1, CAN_IT_HOSTFW_IMAGE=<image built from
// test/hostfw/Dockerfile>. CAN_IT_HOSTFW_KEEP=1 leaves the rules loaded afterwards so the rest of
// the integration tests can be run against an isolated node. Not covered: systemd, a reboot, ufw.

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	fwSidecar  = "can-hostfw-sidecar"
	fakeMeta   = "can-hostfw-fakemeta"
	metadataIP = "169.254.169.254"
	cniGateway = "10.42.0.1"
	clusterDNS = "10.43.0.10"
)

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec // test helper
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// probeMany runs ONE pod that first waits until control is reachable (so a pod whose network is
// not up yet cannot be read as "blocked"), then tries every target once.
func probeMany(t *testing.T, ns, name, labels, control string, targets ...string) map[string]bool {
	t.Helper()
	hp := func(s string) string { return strings.Replace(s, ":", " ", 1) }
	script := "for i in $(seq 40); do nc -z -w 3 " + hp(control) + " && break; sleep 2; done; " +
		"nc -z -w 3 " + hp(control) + " || { echo NOCONTROL; exit 0; }; "
	for _, tg := range targets {
		script += "if nc -z -w 3 " + hp(tg) + "; then echo " + tg + " REACHED; else echo " + tg + " BLOCKED; fi; "
	}
	out := rprobe(t, ns, name, labels, script)
	if strings.Contains(out, "NOCONTROL") {
		t.Fatalf("probe %s/%s never reached its control %s", ns, name, control)
	}
	res := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 && (f[1] == "REACHED" || f[1] == "BLOCKED") {
			res[f[0]] = f[1] == "REACHED"
		}
	}
	if len(res) != len(targets) {
		t.Fatalf("probe %s/%s: want %d verdicts:\n%s", ns, name, len(targets), out)
	}
	return res
}

func expectAll(t *testing.T, who string, got map[string]bool, want bool) {
	t.Helper()
	for target, reached := range got {
		if reached != want {
			t.Errorf("%s -> %s: reached=%v, want %v", who, target, reached, want)
		} else {
			t.Logf("%s -> %s: reached=%v", who, target, reached)
		}
	}
}

func ingressAnswers(t *testing.T, what string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	eventually(t, 2*time.Minute, "Traefik to answer on the published port "+what, func() (bool, string) {
		resp, err := client.Get("http://127.0.0.1:80/can-hostfw-probe")
		if err != nil {
			return false, err.Error()
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return resp.StatusCode == http.StatusNotFound && strings.Contains(string(b), "404 page not found"), resp.Status
	})
}

func TestNodeIsolationOnK3d(t *testing.T) {
	if os.Getenv("CAN_IT_HOSTFW") != "1" {
		t.Skip("set CAN_IT_HOSTFW=1 and CAN_IT_HOSTFW_IMAGE (needs `make dev` and docker)")
	}
	image := os.Getenv("CAN_IT_HOSTFW_IMAGE")
	if image == "" {
		t.Fatal("CAN_IT_HOSTFW_IMAGE must name the image built from test/hostfw/Dockerfile")
	}
	keep := os.Getenv("CAN_IT_HOSTFW_KEEP") == "1"
	node := "k3d-" + strings.TrimPrefix(kubeContext(), "k3d-") + "-server-0"
	ip := nodeIP(t)

	// A stand-in cloud metadata server on the cluster's docker network, routed from the node.
	network := docker(t, "inspect", node, "-f", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}")
	_ = exec.Command("docker", "rm", "-f", fakeMeta, fwSidecar).Run()
	docker(t, "run", "-d", "--name", fakeMeta, "--network", network, "--cap-add", "NET_ADMIN", image,
		"sh", "-c", "ip addr add "+metadataIP+"/32 dev eth0 && exec socat TCP-LISTEN:80,fork,reuseaddr SYSTEM:'echo ok'")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", fakeMeta).Run() })
	metaAddr := docker(t, "inspect", fakeMeta, "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}")

	kubectlPath, err := exec.LookPath("kubectl")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "can-hostfw")
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(dir, "k3s.yaml") // the node's own: https://127.0.0.1:6443
	if err := os.WriteFile(kubeconfig, []byte(docker(t, "exec", node, "cat", "/etc/rancher/k3s/k3s.yaml")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repoRoot(t), "core", "provider", "hostfw", "can-firewall.sh")
	script, err = filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	docker(t, "run", "-d", "--name", fwSidecar, "--network", "container:"+node, "--cap-add", "NET_ADMIN",
		// `check` and the sync loop build their reference rules in a new network namespace.
		"--cap-add", "SYS_ADMIN",
		"-v", kubectlPath+":/usr/local/bin/kubectl:ro", "-v", kubeconfig+":/kubeconfig:ro",
		"-v", script+":/usr/local/sbin/can-firewall:ro",
		"-e", "CAN_FW_KUBECTL=kubectl --kubeconfig /kubeconfig", "-e", "CAN_FW_INTERVAL=2",
		image, "sleep", "infinity")
	fw := func(args ...string) string {
		return docker(t, append([]string{"exec", fwSidecar}, args...)...)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "exec", fwSidecar, "nft", "list", "table", "inet", "can_isolation").CombinedOutput() //nolint:gosec // test helper
			t.Logf("ruleset at failure:\n%s", out)
		}
		if !keep {
			_ = exec.Command("docker", "exec", fwSidecar, "bash", "/usr/local/sbin/can-firewall", "disable").Run()
			_ = exec.Command("docker", "rm", "-f", fwSidecar).Run()
		}
	})
	fw("ip", "route", "replace", metadataIP+"/32", "via", metaAddr)

	moduleNS(t, "mod-fwstub")
	plainNS(t, "hostfw-plain")
	control := clusterDNS + ":53"
	nodeTargets := []string{ip + ":6443", ip + ":10250", "10.43.0.1:443"}
	plainTargets := append([]string{cniGateway + ":6443", cniGateway + ":10250", metadataIP + ":80"}, nodeTargets...)

	// ---- positive controls: without the rules all of it is reachable ----
	expectAll(t, "module pod (no rules)", probeMany(t, "mod-fwstub", "c-mod", "app=probe", control, nodeTargets...), true)
	expectAll(t, "plain pod (no rules)", probeMany(t, "hostfw-plain", "c-plain", "app=probe", control, plainTargets...), true)
	ingressAnswers(t, "before the rules")
	if t.Failed() {
		t.Fatal("controls failed: a BLOCKED below would mean nothing")
	}

	// ---- enforce, as the host service does: apply, then the sync loop ----
	fw("bash", "/usr/local/sbin/can-firewall", "apply")
	docker(t, "exec", "-d", fwSidecar, "sh", "-c", "bash /usr/local/sbin/can-firewall run >/tmp/can-firewall.log 2>&1")

	expectAll(t, "module pod", probeMany(t, "mod-fwstub", "e-mod", "app=probe", control, nodeTargets...), false)
	expectAll(t, "plain pod", probeMany(t, "hostfw-plain", "e-plain", "app=probe", control, plainTargets...), false)
	expectAll(t, "module pod (egress)", probeMany(t, "mod-fwstub", "e-egress", "app=probe", control, "1.1.1.1:443"), true)
	if out := rprobe(t, "mod-fwstub", "e-dns", "app=probe",
		"sleep 8; nslookup kubernetes.default.svc.cluster.local "+clusterDNS+" || true"); !strings.Contains(out, "10.43.0.1") {
		t.Errorf("DNS from a module pod does not work:\n%s", out)
	}

	// System pods are trusted for the API server within seconds of starting, and for nothing else.
	sys := probeMany(t, "kube-system", "e-sys", "app=hostfw-probe", "10.43.0.1:443", ip+":6443", ip+":10250", metadataIP+":80")
	if !sys[ip+":6443"] || sys[ip+":10250"] || sys[metadataIP+":80"] {
		t.Errorf("kube-system pod: %v, want only the API server", sys)
	}
	core := probeMany(t, "can-system", "e-core", "app.kubernetes.io/name=can-core", "10.43.0.1:443", ip+":6443")
	expectAll(t, "pod labelled can-core", core, true)

	// ---- restarts: new pod addresses must be picked up or the cluster is broken ----
	// CoreDNS and Traefik only become Ready once they have talked to the API server.
	mustKubectl(t, "-n", "kube-system", "delete", "pod", "-l", "k8s-app=kube-dns", "--wait=true")
	mustKubectl(t, "-n", "kube-system", "rollout", "status", "deploy/coredns", "--timeout=150s")
	mustKubectl(t, "-n", "kube-system", "delete", "pod", "-l", "app.kubernetes.io/name=traefik", "--wait=true")
	mustKubectl(t, "-n", "kube-system", "rollout", "status", "deploy/traefik", "--timeout=180s")
	mustKubectl(t, "-n", "kube-system", "delete", "pod", "-l", "app=local-path-provisioner", "--wait=true")
	mustKubectl(t, "-n", "kube-system", "rollout", "status", "deploy/local-path-provisioner", "--timeout=150s")
	ingressAnswers(t, "after restarting Traefik under the rules")
	expectAll(t, "module pod after restarts", probeMany(t, "mod-fwstub", "r-mod", "app=probe", control, nodeTargets...), false)

	// ---- probes (kubelet to pod) and kubectl logs still work ----
	ready := `
apiVersion: v1
kind: Pod
metadata: {name: probed, namespace: hostfw-plain}
spec:
  containers:
    - name: probed
      image: busybox:1.37.0
      command: [sh, -c, "echo hostfw-log-line; exec nc -lk -p 8080 -e cat"]
      readinessProbe: {tcpSocket: {port: 8080}, periodSeconds: 2}
`
	if out, err := kubectlStdin(ready, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply probed pod: %v\n%s", err, out)
	}
	mustKubectl(t, "-n", "hostfw-plain", "wait", "--for=condition=Ready", "pod/probed", "--timeout=120s")
	if out := mustKubectl(t, "-n", "hostfw-plain", "logs", "probed"); !strings.Contains(out, "hostfw-log-line") {
		t.Errorf("kubectl logs: %q", out)
	}

	eventually(t, time.Minute, "can-firewall check to pass", func() (bool, string) {
		out, err := exec.Command("docker", "exec", fwSidecar, "bash", "/usr/local/sbin/can-firewall", "check").CombinedOutput() //nolint:gosec // test helper
		return err == nil && strings.Contains(string(out), "status=ok"), string(out)
	})
	t.Logf("sync log:\n%s", fw("cat", "/tmp/can-firewall.log"))

	if keep {
		t.Log("CAN_IT_HOSTFW_KEEP=1: the rules and the sync loop stay")
		return
	}
	// ---- closing control: removing the rules makes it reachable again ----
	fw("sh", "-c", "pkill -f 'can-firewall[ ]run'; bash /usr/local/sbin/can-firewall disable")
	expectAll(t, "plain pod (rules removed)", probeMany(t, "hostfw-plain", "z-plain", "app=probe", control, plainTargets...), true)
}
