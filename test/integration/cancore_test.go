//go:build integration

package integration

// Epic E03 part 2: can-core against a real cluster. TestAdmissionPolicy* need only `make dev`.
// TestCanCoreLifecycle needs can-core deployed from a locally built image (the dev-cluster
// workflow builds it, `k3d image import`s it and applies deploy/can-core); it is skipped unless
// CAN_IT_CANCORE=1. TestPostgresProvisioner starts a throw-away Postgres pod in can-data.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/dodwmd/companyinacan/core/server/dbprov"
)

const canCoreUser = "system:serviceaccount:can-system:can-core"

// eventually polls f until it returns ok or the timeout passes; the last detail is reported.
func eventually(t *testing.T, timeout time.Duration, what string, f func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var detail string
	for {
		ok, d := f()
		if ok {
			return
		}
		detail = d
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s; last state:\n%s", timeout, what, detail)
		}
		time.Sleep(2 * time.Second)
	}
}

func asCanCore(args ...string) (string, error) {
	return kubectl(append([]string{"--as", canCoreUser}, args...)...)
}

func asCanCoreStdin(stdin string, args ...string) (string, error) {
	return kubectlStdin(stdin, append([]string{"--as", canCoreUser}, args...)...)
}

func nsYAML(name string, labels map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", name)
	if len(labels) > 0 {
		b.WriteString("  labels:\n")
		for k, v := range labels {
			fmt.Fprintf(&b, "    %s: %s\n", k, v)
		}
	}
	return b.String()
}

// waitForPolicy waits until the admission policy is enforcing: a binding is effective a moment
// after it is created, so the first probe may still be allowed.
func waitForPolicy(t *testing.T) {
	t.Helper()
	mustKubectl(t, "get", "validatingadmissionpolicy", "can-core-rolebindings", "can-core-namespaces")
	mustKubectl(t, "get", "validatingadmissionpolicybinding", "can-core-rolebindings", "can-core-namespaces")
	eventually(t, 60*time.Second, "admission policy to be enforced", func() (bool, string) {
		out, err := asCanCoreStdin(nsYAML("not-a-module", nil), "create", "-f", "-")
		if err == nil {
			_, _ = kubectl("delete", "namespace", "not-a-module", "--ignore-not-found")
			return false, "namespace creation outside mod-* was allowed"
		}
		return strings.Contains(out, "can-core may only create module namespaces"), out
	})
}

func TestAdmissionPolicyDeniesBindIntoKubeSystem(t *testing.T) {
	waitForPolicy(t)
	out, err := asCanCore("create", "rolebinding", "evil", "-n", "kube-system",
		"--clusterrole=can-core-module", "--serviceaccount=can-system:can-core")
	if err == nil {
		_, _ = kubectl("delete", "rolebinding", "evil", "-n", "kube-system", "--ignore-not-found")
		t.Fatalf("can-core bound can-core-module into kube-system: %s", out)
	}
	if !strings.Contains(out, "can-core may only create RoleBindings in module namespaces") {
		t.Fatalf("denied, but not by the admission policy (RBAC alone allows this): %s", out)
	}
	// The same request from a cluster administrator is not affected by the policy.
	mustKubectl(t, "create", "rolebinding", "can-it-admin-ok", "-n", "default", "--clusterrole=view", "--user=can-it-nobody")
	mustKubectl(t, "delete", "rolebinding", "can-it-admin-ok", "-n", "default")
}

func TestAdmissionPolicyNamespaceAndBindingRules(t *testing.T) {
	waitForPolicy(t)
	good := map[string]string{"can.dev/namespace-kind": "module", "pod-security.kubernetes.io/enforce": "restricted"}
	t.Cleanup(func() {
		_, _ = kubectl("delete", "namespace", "mod-vaptest", "mod-vapbad", "--ignore-not-found", "--wait=false")
	})

	denied := []struct {
		name, yaml, want string
	}{
		{"name outside mod-*", nsYAML("evil-ns", good), "only create module namespaces"},
		{"relaxed pod security", nsYAML("mod-vapbad", map[string]string{"can.dev/namespace-kind": "module", "pod-security.kubernetes.io/enforce": "privileged"}), "restricted pod security"},
		{"no labels", nsYAML("mod-vapbad", nil), "restricted pod security"},
		{"pod security exemption", nsYAML("mod-vapbad", map[string]string{"can.dev/namespace-kind": "module", "pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/exempt": "true"}), "cannot opt out"},
	}
	for _, d := range denied {
		out, err := asCanCoreStdin(d.yaml, "create", "-f", "-")
		if err == nil || !strings.Contains(out, d.want) {
			_, _ = kubectl("delete", "namespace", "evil-ns", "mod-vapbad", "--ignore-not-found", "--wait=false")
			t.Errorf("%s: want denial containing %q, got err=%v out=%s", d.name, d.want, err, out)
		}
	}
	if out, err := asCanCoreStdin(nsYAML("mod-vaptest", good), "create", "-f", "-"); err != nil {
		t.Fatalf("a correctly labelled module namespace must be allowed: %s", out)
	}
	if out, err := asCanCore("create", "rolebinding", "can-core-module", "-n", "mod-vaptest",
		"--clusterrole=can-core-module", "--serviceaccount=can-system:can-core"); err != nil {
		t.Fatalf("the canonical binding into a module namespace must be allowed: %s", out)
	}
	if out, err := asCanCore("create", "rolebinding", "other", "-n", "mod-vaptest",
		"--clusterrole=can-core-module", "--user=somebody-else"); err == nil || !strings.Contains(out, "only subject") {
		t.Fatalf("a binding to another subject must be denied by policy: err=%v %s", err, out)
	}
}

// ---- the stub module lifecycle (the epic's "done when") -------------------------------------

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "..", "..")
}

// canApply runs the real `can apply` CLI, which syncs company.yaml into the cluster.
func canApply(t *testing.T, companyYAML string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "company.yaml"), []byte(companyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./cmd/can", "apply", dir, "--modules-dir", "test/integration/testdata/modules", "--context", kubeContext()) //nolint:gosec // test helper
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("can apply: %v\n%s", err, out)
	}
	t.Logf("can apply: %s", strings.TrimSpace(string(out)))
}

func company(greeting string, enabled bool) string {
	return fmt.Sprintf("schema: 1\nname: Stub Co\ndomain: stub.example\nprovider:\n  type: local\nmodules:\n  stub:\n    enabled: %v\n    settings:\n      greeting: %s\n", enabled, greeting)
}

func stubLogs() string {
	out, _ := kubectl("-n", "mod-stub", "logs", "deploy/stub", "--tail=60")
	return out
}

func TestCanCoreLifecycle(t *testing.T) {
	if os.Getenv("CAN_IT_CANCORE") != "1" {
		t.Skip("CAN_IT_CANCORE=1 not set: can-core is not deployed in this cluster")
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := kubectl("-n", "can-system", "logs", "deploy/can-core", "--tail=80")
			t.Logf("can-core log:\n%s", out)
			out, _ = kubectl("get", "pods,deploy,networkpolicy,resourcequota", "-n", "mod-stub", "-o", "wide")
			t.Logf("mod-stub:\n%s", out)
		}
	})
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")

	// 1. enable: `can apply` -> ConfigMap -> can-core reconciles -> namespace, policies, workload.
	canApply(t, company("first", true))
	eventually(t, 180*time.Second, "stub deployment to be rolled out", func() (bool, string) {
		out, err := kubectl("-n", "mod-stub", "rollout", "status", "deploy/stub", "--timeout=5s")
		return err == nil, out
	})

	// namespace, quota, policies and RBAC from deploy/templates
	if out := mustKubectl(t, "get", "ns", "mod-stub", "-o", "jsonpath={.metadata.labels}"); !strings.Contains(out, `"pod-security.kubernetes.io/enforce":"restricted"`) || !strings.Contains(out, `"can.dev/namespace-kind":"module"`) {
		t.Errorf("namespace labels: %s", out)
	}
	for _, np := range []string{"default-deny", "allow-ingress-from-traefik", "allow-ingress-from-can-core", "module-egress"} {
		mustKubectl(t, "-n", "mod-stub", "get", "networkpolicy", np)
	}
	if out := mustKubectl(t, "-n", "mod-stub", "get", "resourcequota", "ceiling", "-o", "jsonpath={.spec.hard}"); !strings.Contains(out, "62Mi") {
		t.Errorf("quota should be derived from ram_mb=20: %s", out)
	}
	mustKubectl(t, "-n", "mod-stub", "get", "rolebinding", "can-core-module")

	// workload hardening: own SA, no automounted API token, only the projected `can` token
	if out := mustKubectl(t, "-n", "mod-stub", "get", "pod", "-l", "app=stub", "-o", "jsonpath={.items[0].spec.automountServiceAccountToken}"); out != "false" {
		t.Errorf("automountServiceAccountToken=%q", out)
	}
	vols := mustKubectl(t, "-n", "mod-stub", "get", "pod", "-l", "app=stub", "-o", "jsonpath={.items[0].spec.volumes}")
	if !strings.Contains(vols, `"audience":"can"`) || strings.Contains(vols, "kube-api-access") {
		t.Errorf("pod volumes: %s", vols)
	}

	// 2. injected config and a module API call with the real projected token, reviewed by the real API server
	eventually(t, 90*time.Second, "the stub to authenticate against the module API with its projected token", func() (bool, string) {
		l := stubLogs()
		return strings.Contains(l, `"id":"stub"`) && strings.Contains(l, `"namespace":"mod-stub"`), l
	})
	l := stubLogs()
	for _, want := range []string{"GREETING=first", "CAN_API_URL=http://can-core.can-system.svc.cluster.local:8090", "CAN_WEBHOOK_SECRET_SET=yes", "ADMIN blocked"} {
		if !strings.Contains(l, want) {
			t.Errorf("stub log lacks %q:\n%s", want, l)
		}
	}
	if !strings.Contains(l, "NOAUTH wget: server returned error: HTTP/1.1 401") {
		t.Errorf("an unauthenticated call must be 401:\n%s", l)
	}
	// a token for another audience (the default API-server audience) must be refused
	wrong := mustKubectl(t, "-n", "mod-stub", "create", "token", "stub", "--duration=10m")
	out, _ := kubectl("-n", "mod-stub", "exec", "deploy/stub", "--", "sh", "-c",
		fmt.Sprintf(`wget -T 3 -qO- --header "Authorization: Bearer %s" "$CAN_API_URL/v1/self" 2>&1 | head -c 120`, wrong))
	if !strings.Contains(out, "401") {
		t.Errorf("a token without audience `can` was accepted: %s", out)
	}

	// 3. upgrade: a settings change in company.yaml reaches the pod
	canApply(t, company("second", true))
	eventually(t, 180*time.Second, "the new settings to roll out", func() (bool, string) {
		l := stubLogs()
		return strings.Contains(l, "GREETING=second"), l
	})

	// 4. disable: workloads scale to zero, data and Secrets stay
	canApply(t, company("second", false))
	eventually(t, 120*time.Second, "the stub to scale to zero", func() (bool, string) {
		out, _ := kubectl("-n", "mod-stub", "get", "deploy", "stub", "-o", "jsonpath={.spec.replicas}/{.status.replicas}")
		return out == "0/" || out == "0/0", out
	})
	mustKubectl(t, "-n", "mod-stub", "get", "pvc", "data")
	mustKubectl(t, "-n", "mod-stub", "get", "secret", "can-module")
	mustKubectl(t, "get", "ns", "mod-stub")

	// 5. re-enable
	canApply(t, company("third", true))
	eventually(t, 180*time.Second, "the stub to come back", func() (bool, string) {
		l := stubLogs()
		return strings.Contains(l, "GREETING=third") && strings.Contains(l, `"id":"stub"`), l
	})

	// can-core's own footprint on the cluster
	if out, err := kubectl("-n", "can-system", "get", "pod", "-l", "app.kubernetes.io/name=can-core", "-o", "jsonpath={.items[0].status.containerStatuses[0].restartCount}"); err != nil || out != "0" {
		t.Errorf("can-core restarted: %q %v", out, err)
	}
}

// ---- database provisioner against a throw-away Postgres -----------------------------------------

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func TestPostgresProvisioner(t *testing.T) {
	const pw = "adminPassw0rdForTestsOnly"
	t.Cleanup(func() {
		_, _ = kubectl("-n", "can-data", "delete", "pod", "pg-test", "--ignore-not-found", "--wait=false")
	})
	mustKubectl(t, "-n", "can-data", "run", "pg-test", "--image=postgres:16-alpine", "--restart=Never",
		"--env=POSTGRES_PASSWORD="+pw, "--labels=app.kubernetes.io/name=pg-test")
	mustKubectl(t, "-n", "can-data", "wait", "--for=condition=Ready", "pod/pg-test", "--timeout=180s")

	port := freePort(t)
	pf := exec.Command("kubectl", "--context", kubeContext(), "-n", "can-data", "port-forward", "pod/pg-test", fmt.Sprintf("%d:5432", port)) //nolint:gosec // test helper
	if err := pf.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pf.Process.Kill(); _ = pf.Wait() })

	admin := fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%d/postgres?sslmode=disable", pw, port)
	prov := &dbprov.Postgres{AdminURL: admin, Host: "postgres.can-data.svc.cluster.local", Port: 5432}
	var creds dbprov.Credentials
	eventually(t, 90*time.Second, "postgres to accept connections", func() (bool, string) {
		c, err := prov.Ensure(context.Background(), "postgres", "crm", "modulePassw0rdAAAAAAAAAAAA")
		creds = c
		return err == nil, fmt.Sprint(err)
	})
	if creds.Database != "mod_crm" || creds.User != "mod_crm" {
		t.Fatalf("%+v", creds)
	}
	connect := func(user, password, db string) (string, error) {
		d, err := sql.Open("postgres", fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", user, password, port, db))
		if err != nil {
			return "", err
		}
		defer func() { _ = d.Close() }()
		var cur string
		return cur, d.QueryRow("SELECT current_database()").Scan(&cur)
	}
	if cur, err := connect("mod_crm", "modulePassw0rdAAAAAAAAAAAA", "mod_crm"); err != nil || cur != "mod_crm" {
		t.Fatalf("the module cannot use its own database: %q %v", cur, err)
	}
	// idempotent, and re-asserting the password changes it
	if _, err := prov.Ensure(context.Background(), "postgres", "crm", "rotatedPassw0rdBBBBBBBBBBB"); err != nil {
		t.Fatal(err)
	}
	if _, err := connect("mod_crm", "modulePassw0rdAAAAAAAAAAAA", "mod_crm"); err == nil {
		t.Error("old password still works after re-ensure")
	}
	if _, err := connect("mod_crm", "rotatedPassw0rdBBBBBBBBBBB", "mod_crm"); err != nil {
		t.Errorf("new password does not work: %v", err)
	}
	// a second module has its own database and cannot connect to the first one
	if _, err := prov.Ensure(context.Background(), "postgres", "hr-portal", "hrPassw0rdCCCCCCCCCCCCCCCC"); err != nil {
		t.Fatal(err)
	}
	if _, err := connect("mod_hr_portal", "hrPassw0rdCCCCCCCCCCCCCCCC", "mod_hr_portal"); err != nil {
		t.Fatal(err)
	}
	if _, err := connect("mod_hr_portal", "hrPassw0rdCCCCCCCCCCCCCCCC", "mod_crm"); err == nil {
		t.Error("module hr-portal could connect to module crm's database")
	}
}
