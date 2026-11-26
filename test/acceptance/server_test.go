//go:build acceptance

package acceptance

// What is done on the server that is not the product: the stand-ins for a public certificate and a
// mail relay, and the measurements.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/test/acceptance/report"
)

const manifestsDir = "/var/lib/rancher/k3s/server/manifests"

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// seedCertificate leaves the test certificate where k3s applies it once it is installed, as
// Traefik's default one. It stands in for the certificate Let's Encrypt would issue.
func (e *env) seedCertificate(t *testing.T) {
	t.Helper()
	crt := e.read(t, filepath.Join(e.standins, "tls.crt"))
	key := e.read(t, filepath.Join(e.standins, "tls.key"))
	secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata: {name: can-acceptance-tls, namespace: kube-system}\ntype: kubernetes.io/tls\ndata: {tls.crt: %s, tls.key: %s}\n", b64(crt), b64(key))
	store := "apiVersion: traefik.io/v1alpha1\nkind: TLSStore\nmetadata: {name: default, namespace: kube-system}\nspec: {defaultCertificate: {secretName: can-acceptance-tls}}\n"
	e.mustSSH(t, time.Minute, fmt.Sprintf("install -d -m 0755 %[1]s\necho %[2]s | base64 -d >%[1]s/zz-acceptance-secret.yaml\necho %[3]s | base64 -d >%[1]s/zz-acceptance-tlsstore.yaml",
		manifestsDir, b64([]byte(secret)), b64([]byte(store))))
}

// deploySink runs the SMTP sink in the cluster (STARTTLS and AUTH, as a hosted relay) and lets
// can-core use it: a private address is refused by default, and the sink's certificate is signed by
// the test CA. These two settings of can-core are the stand-in; the relay code is the product's.
func (e *env) deploySink(t *testing.T) {
	t.Helper()
	crt := e.read(t, filepath.Join(e.standins, "tls.crt"))
	key := e.read(t, filepath.Join(e.standins, "tls.key"))
	ca := e.read(t, filepath.Join(e.standins, "ca.crt"))
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata: {name: acceptance-sink}
---
apiVersion: v1
kind: Secret
metadata: {name: sink-tls, namespace: acceptance-sink}
type: kubernetes.io/tls
data: {tls.crt: %s, tls.key: %s}
---
apiVersion: v1
kind: Secret
metadata: {name: sink-auth, namespace: acceptance-sink}
stringData: {SINK_USERNAME: %s, SINK_PASSWORD: %s}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: smtp-sink, namespace: acceptance-sink}
spec:
  selector: {matchLabels: {app: smtp-sink}}
  template:
    metadata: {labels: {app: smtp-sink}}
    spec:
      containers:
        - name: sink
          image: docker.io/library/can-acceptance-sink:test
          imagePullPolicy: Never
          args: ["-smtp", ":2525", "-http", ":8025", "-tls-cert", /tls/tls.crt, "-tls-key", /tls/tls.key]
          envFrom: [{secretRef: {name: sink-auth}}]
          readinessProbe: {httpGet: {path: /healthz, port: 8025}, periodSeconds: 2}
          volumeMounts: [{name: tls, mountPath: /tls, readOnly: true}]
      volumes: [{name: tls, secret: {secretName: sink-tls}}]
---
apiVersion: v1
kind: Service
metadata: {name: smtp-sink, namespace: acceptance-sink}
spec:
  selector: {app: smtp-sink}
  ports: [{name: smtp, port: 2525}, {name: http, port: 8025}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: acceptance-ca, namespace: can-system}
binaryData: {ca.crt: %s}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: acceptance-smtp-sink, namespace: can-system}
spec:
  podSelector: {matchLabels: {app.kubernetes.io/name: can-core}}
  policyTypes: [Egress]
  egress:
    - to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: acceptance-sink}}}]
      ports: [{protocol: TCP, port: 2525}]
`, b64(crt), b64(key), smtpUser, e.smtpPassword, b64(ca))
	patch := `{"spec":{"template":{"spec":{"containers":[{"name":"can-core","env":[{"name":"CAN_SMTP_ALLOW_PRIVATE","value":"true"},{"name":"SSL_CERT_DIR","value":"/etc/can-acceptance-ca"}],"volumeMounts":[{"name":"acceptance-ca","mountPath":"/etc/can-acceptance-ca","readOnly":true}]}],"volumes":[{"name":"acceptance-ca","configMap":{"name":"acceptance-ca"}}]}}}}`
	e.mustSSH(t, 4*time.Minute, fmt.Sprintf(`k3s ctr -n k8s.io images import %s >/dev/null
echo %s | base64 -d | k3s kubectl apply -f - >/dev/null
k3s kubectl -n acceptance-sink rollout status deploy/smtp-sink --timeout=120s
k3s kubectl -n can-system patch deployment can-core -p '%s'
k3s kubectl -n can-system rollout status deploy/can-core --timeout=180s`, filepath.Join(e.standins, "sink.tar"), b64([]byte(manifest)), patch))
}

type mail struct {
	From string   `json:"from"`
	To   []string `json:"to"`
	Data string   `json:"data"`
	TLS  bool     `json:"tls"`
	Auth string   `json:"auth"`
}

func (e *env) mails() ([]mail, error) {
	out, err := e.ssh(time.Minute, "k3s kubectl get --raw /api/v1/namespaces/acceptance-sink/services/smtp-sink:8025/proxy/messages")
	if err != nil {
		return nil, err
	}
	var m []mail
	return m, json.Unmarshal([]byte(out), &m)
}

// waitMail waits for the notification of one form submission.
func (e *env) waitMail(t *testing.T, visitor string) {
	t.Helper()
	// The body is quoted-printable: the local part has nothing that gets encoded or folded early.
	local, _, _ := strings.Cut(visitor, "@")
	eventually(t, 3*time.Minute, "the notification email for "+visitor+" at the relay", func() (bool, string) {
		ms, err := e.mails()
		if err != nil {
			return false, err.Error()
		}
		for _, m := range ms {
			if strings.Contains(strings.ReplaceAll(m.Data, "=\r\n", ""), local) {
				if !m.TLS || m.Auth != smtpUser || len(m.To) != 1 || m.To[0] != salesEmail {
					t.Fatalf("the notification arrived wrongly: to %v, TLS %v, signed in as %q", m.To, m.TLS, m.Auth)
				}
				return true, ""
			}
		}
		out, _ := e.ssh(time.Minute, "k3s kubectl -n can-system logs deploy/can-core --tail=200 | grep -i -E 'mail|smtp' | tail -15 || true")
		return false, fmt.Sprintf("%d messages at the sink\n%s", len(ms), out)
	})
}

const memoryScript = `ws() {
  [ -f "$1/memory.current" ] || { echo 0; return; }
  c=$(cat "$1/memory.current"); i=$(awk '/^inactive_file /{print $2}' "$1/memory.stat")
  echo $(( (c - i) / 1048576 ))
}
awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END{printf "total=%d\nused=%d\n", t/1024, (t-a)/1024}' /proc/meminfo
echo "k3s=$(ws /sys/fs/cgroup/system.slice/k3s.service)"
if [ -d /sys/fs/cgroup/kubepods.slice ]; then echo "pods=$(ws /sys/fs/cgroup/kubepods.slice)"; else echo "pods=$(ws /sys/fs/cgroup/kubepods)"; fi
if command -v k3s >/dev/null; then echo ---; k3s crictl stats 2>/dev/null || true; fi`

// memory records the node's memory after letting it settle.
func (e *env) memory(t *testing.T, label string, settle time.Duration, budgetMiB int) {
	t.Helper()
	time.Sleep(settle)
	out := e.mustSSH(t, time.Minute, memoryScript)
	head, detail, _ := strings.Cut(out, "---\n")
	v := map[string]int{}
	for _, l := range strings.Split(head, "\n") {
		if k, n, ok := strings.Cut(l, "="); ok {
			v[k], _ = strconv.Atoi(n)
		}
	}
	m := report.Memory{Label: label, K3s: v["k3s"], Pods: v["pods"], Used: v["used"], Total: v["total"], Budget: budgetMiB, Detail: detail}
	// Without both cgroups the budget would pass by measuring nothing.
	if budgetMiB > 0 && (m.K3s == 0 || m.Pods == 0) {
		t.Fatalf("the memory of k3s or of the pods could not be read:\n%s", head)
	}
	e.rep.Memory = append(e.rep.Memory, m)
	t.Logf("memory %s: k3s %d + pods %d = %d MiB; machine %d/%d MiB", label, m.K3s, m.Pods, m.Stack(), m.Used, m.Total)
}

// diagnostics is what is saved when the journey fails.
func (e *env) diagnostics() {
	var b strings.Builder
	for _, c := range []string{
		"k3s kubectl get pods,svc,pvc,ingressroute -A -o wide",
		"k3s kubectl get events -A --sort-by=.lastTimestamp | tail -60",
		"k3s kubectl -n can-system logs deploy/can-core --tail=300",
		"k3s kubectl -n kube-system logs deploy/traefik --tail=60",
		"k3s kubectl -n mod-forms logs deploy/forms --tail=60",
		"k3s kubectl -n mod-work logs deploy/work --tail=60",
		"k3s kubectl -n mod-support logs deploy/support --all-containers --tail=60",
		"k3s kubectl -n can-data logs deploy/postgres --tail=40",
		"journalctl -u can-backup --no-pager | tail -60",
		"journalctl -u k3s --no-pager | tail -60",
		"cat /var/lib/can/restore-state.json",
		"df -h /var/lib",
		"free -m",
		"nft list table inet can_acceptance_noacme",
	} {
		out, err := e.ssh(time.Minute, c)
		fmt.Fprintf(&b, "\n===== %s\n%s\n", c, out)
		if err != nil {
			fmt.Fprintf(&b, "(%v)\n", err)
		}
	}
	e.save("diagnostics", b.String())
}
