//go:build integration

package integration

// Module-to-module connections on a real cluster (specs/057-module-capability-wiring): provisioning
// reads the people directory, equity looks callers and persons up in it, nothing else reaches
// people, the admission policy refuses any other connection, and disabling a side removes it.
//
// Runs only with CAN_IT_CANCORE=1 and CAN_IT_WIRING=1 (.github/workflows/modules-k3d.yml), after
// TestEveryInHouseModuleOnK3d in the same cluster.

import (
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/auth"
)

const peopleHealth = "http://people-api.mod-people.svc.cluster.local:8081/healthz"

func wiringCompany(without ...string) func(t *testing.T) string {
	return func(t *testing.T) string {
		company := "schema: 1\nname: Every Module Co\ndomain: modules.example\nprovider:\n  type: local\nmodules:\n"
		for _, id := range inHouseModules(t) {
			company += fmt.Sprintf("  %s: {enabled: %v}\n", id, !slices.Contains(without, id))
		}
		return company
	}
}

// moduleProbe asks peopleHealth from a short-lived pod in mod-<module> that is labelled as the app
// name, and reports whether it got an answer. The pod satisfies the namespace's restricted standard
// and quota; a pod that does not run fails the test (it must never read as "blocked").
func moduleProbe(t *testing.T, module, app, name string) bool {
	t.Helper()
	ns := "mod-" + module
	pod := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s, namespace: %s, labels: {app.kubernetes.io/name: %s}}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: probe
      image: busybox:1.37.0
      # kube-router needs a moment to put a new pod under its policies
      command: [sh, -c, "sleep 10; if wget -q -T 5 -O /dev/null %s; then echo REACHED; else echo BLOCKED; fi"]
      resources: {requests: {cpu: 5m, memory: 8Mi}, limits: {memory: 16Mi}}
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
`, name, ns, app, peopleHealth)
	if out, err := kubectlStdin(pod, "apply", "-f", "-"); err != nil {
		t.Fatalf("probe %s in %s: %v\n%s", name, ns, err, out)
	}
	defer func() { _, _ = kubectl("-n", ns, "delete", "pod", name, "--now", "--ignore-not-found") }()
	eventually(t, 2*time.Minute, "probe "+name+" to finish", func() (bool, string) {
		phase, err := kubectl("-n", ns, "get", "pod", name, "-o", "jsonpath={.status.phase}")
		return err == nil && phase == "Succeeded", phase
	})
	switch out := mustKubectl(t, "-n", ns, "logs", name); {
	case strings.Contains(out, "REACHED"):
		return true
	case strings.Contains(out, "BLOCKED"):
		return false
	default:
		t.Fatalf("probe %s in %s gave no verdict: %s", name, ns, out)
		return false
	}
}

func secretOf(t *testing.T, module, key string) string {
	t.Helper()
	out := mustKubectl(t, "-n", "mod-"+module, "get", "secret", "can-module", "-o", "jsonpath={.data."+key+"}")
	b, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatalf("%s of %s is not base64", key, module)
	}
	return string(b)
}

func hasPolicy(ns, name string) bool {
	_, err := kubectl("-n", ns, "get", "networkpolicy", name)
	return err == nil
}

func rolledOut(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		eventually(t, 5*time.Minute, id+" to be rolled out", func() (bool, string) {
			out, err := kubectl("-n", "mod-"+id, "rollout", "status", "deploy/"+id, "--timeout=5s")
			return err == nil, out
		})
	}
}

// adminPAT creates an admin account and a personal access token for equity that carries the
// module's admin permissions.
func adminPAT(t *testing.T) (user, token string) {
	t.Helper()
	const password = "ci-Passw0rd-not-a-secret-9f31"
	user = fmt.Sprintf("wire%d", time.Now().Unix())
	out, err := kubectlStdin(password+"\n", "-n", "can-system", "exec", "-i", "deploy/can-core", "--", "/can-core", "admin", "bootstrap", "--username", user)
	if err != nil {
		t.Fatalf("admin bootstrap: %v\n%s", err, out)
	}
	m := regexp.MustCompile(`TOTP secret \(shown once\): ([A-Z2-7]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatal("no TOTP secret in the bootstrap output")
	}
	key, err := auth.DecodeTOTPSecret(m[1])
	if err != nil {
		t.Fatal(err)
	}
	admin := portForward(t, "can-system", "can-core", 8080)
	st, body, cookies := doJSON(t, "POST", admin+"/api/v1/login", nil, map[string]string{"username": user, "password": password, "totp": auth.Code(key, auth.SHA1, 6, 30, time.Now())})
	if st != 200 {
		t.Fatalf("login: %d %v", st, body)
	}
	var session string
	for _, c := range cookies {
		if c.Name == auth.CookieName {
			session = c.Value
		}
	}
	csrf, _ := body["csrf_token"].(string)
	st, body, _ = doJSON(t, "POST", admin+"/api/v1/tokens", map[string]string{"Cookie": auth.CookieName + "=" + session, "X-CSRF-Token": csrf}, map[string]any{
		"module": "equity", "ttl_hours": 1, "name": "ci",
		"permissions": []string{"equity.read", "equity.write", "equity.approve", "equity.sensitive-read", "equity.admin"},
	})
	token, _ = body["token"].(string)
	if (st != 200 && st != 201) || token == "" {
		t.Fatalf("issue token: %d %v", st, body["message"])
	}
	return user, token
}

func TestModuleWiringOnK3d(t *testing.T) {
	if os.Getenv("CAN_IT_CANCORE") != "1" || os.Getenv("CAN_IT_WIRING") != "1" {
		t.Skip("CAN_IT_CANCORE=1 and CAN_IT_WIRING=1 not set (needs the module images in the cluster)")
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		dumpState(t)
		for _, id := range []string{"people", "equity", "provisioning"} {
			out, _ := kubectl("-n", "mod-"+id, "logs", "deploy/"+id, "--tail=40")
			t.Logf("mod-%s log:\n%s", id, out)
			out, _ = kubectl("-n", "mod-"+id, "get", "networkpolicy,configmap", "-o", "yaml")
			t.Logf("mod-%s:\n%s", id, out)
		}
	})
	prepareDumpsDir(t)
	canApplyModules(t, wiringCompany()(t))
	rolledOut(t, "people", "equity", "provisioning", "work")
	for _, p := range [][2]string{{"mod-equity", "can-wire-to-people"}, {"mod-people", "can-wire-from-equity"}, {"mod-provisioning", "can-wire-to-people"}, {"mod-people", "can-wire-from-provisioning"}} {
		eventually(t, 3*time.Minute, p[0]+"/"+p[1], func() (bool, string) { return hasPolicy(p[0], p[1]), "missing" })
	}

	t.Run("network", func(t *testing.T) {
		// Positive controls first (kube-router programs policies asynchronously), so that
		// "blocked" below means refused and not "nothing works yet".
		for i, id := range []string{"provisioning", "equity"} {
			n := 0
			eventually(t, 2*time.Minute, id+" to reach people", func() (bool, string) {
				n++
				return moduleProbe(t, id, id, fmt.Sprintf("probe-ok-%d-%d", i, n)), "blocked"
			})
		}
		for name, p := range map[string][2]string{
			"probe-unrelated":  {"work", "work"},         // a module with no connection
			"probe-borrowed":   {"work", "equity"},       // the right label in the wrong namespace
			"probe-other-pods": {"equity", "equity-job"}, // another pod of a connected module
		} {
			if moduleProbe(t, p[0], p[1], name) {
				t.Errorf("a pod labelled %s in mod-%s reached people", p[1], p[0])
			}
		}
	})

	t.Run("admission", func(t *testing.T) {
		if got := mustKubectl(t, "get", "validatingadmissionpolicy", "can-core-networkpolicies", "-o", "jsonpath={.status.typeChecking.expressionWarnings}"); got != "" {
			t.Errorf("the policy does not type-check: %s", got)
		}
		wire := func(ns, name, owner, peer string, port int) string {
			return fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: %s, namespace: %s}
spec:
  podSelector: {matchLabels: {app.kubernetes.io/name: %s}}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: mod-%s}}
          podSelector: {matchLabels: {app.kubernetes.io/name: %s}}
      ports: [{protocol: TCP, port: %d}]
`, name, ns, owner, peer, peer, port)
		}
		if out, err := asCanCoreStdin(wire("mod-people", "can-wire-from-probe", "people", "equity", 8081), "create", "--dry-run=server", "-f", "-"); err != nil {
			t.Errorf("a granted connection was refused: %v\n%s", err, out)
		}
		anyModule := `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: open-to-modules, namespace: mod-people}
spec:
  podSelector: {}
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector: {matchLabels: {can.dev/namespace-kind: module}}
`
		for what, y := range map[string]string{
			"an ungranted consumer":               wire("mod-people", "can-wire-from-work", "people", "work", 8081),
			"another port":                        wire("mod-people", "can-wire-from-probe", "people", "equity", 8080),
			"the reverse direction":               wire("mod-equity", "can-wire-from-people", "equity", "people", 8081),
			"a cross-module rule by another name": wire("mod-people", "allow-equity", "people", "equity", 8081),
			"every module":                        anyModule,
		} {
			out, err := asCanCoreStdin(y, "create", "--dry-run=server", "-f", "-")
			if err == nil || !strings.Contains(out, "can-core-networkpolicies") {
				t.Errorf("%s was not refused by the policy: %v\n%s", what, err, out)
			}
		}
	})

	peopleAPI := portForward(t, "mod-people", "people-api", 8081)
	hr := map[string]string{"Authorization": "Bearer " + secretOf(t, "people", "CAN_PEOPLE_API_TOKEN")}
	newPerson := func(name, subject string) string {
		t.Helper()
		in := map[string]string{"kind": "employee", "status": "onboarding", "given_name": name, "family_name": "Example"}
		if subject != "" {
			in["sso_subject"] = subject
		}
		st, b, _ := doJSON(t, "POST", peopleAPI+"/v1/people", hr, in)
		id, _ := b["id"].(string)
		if st != 201 || id == "" {
			t.Fatalf("create person: %d %v", st, b)
		}
		return id
	}
	holder := newPerson("Holder", "")

	t.Run("provisioning reads the directory", func(t *testing.T) {
		p := portForward(t, "mod-provisioning", "provisioning", 8090)
		auth := map[string]string{"Authorization": "Bearer " + secretOf(t, "provisioning", "CAN_PROVISIONING_API_TOKEN")}
		// A leaver plan for someone who is joining is refused with the status the directory has for
		// them: only a module that read the record can say it. (A fresh install has no connectors, so
		// no plan of this person would have steps.)
		eventually(t, 2*time.Minute, "provisioning to read the person from the directory", func() (bool, string) {
			st, b, _ := doJSON(t, "POST", p+"/v1/provision/plans", auth, map[string]string{"kind": "leaver", "party_id": holder})
			return st == 409 && strings.Contains(fmt.Sprint(b["detail"]), "status onboarding"), fmt.Sprint(st, b)
		})
		// An answer that only the directory can give: this person is not in it.
		if st, b, _ := doJSON(t, "POST", p+"/v1/provision/plans", auth, map[string]string{"kind": "joiner", "party_id": "pers_0000000000000000000000ff"}); st != 404 {
			t.Errorf("unknown person: %d %v", st, b)
		}
	})

	user, pat := adminPAT(t)
	bearer := map[string]string{"Authorization": "Bearer " + pat}
	var entity string
	stakeholder := func(equityAPI, personID string) (int, map[string]any) {
		st, b, _ := doJSON(t, "POST", equityAPI+"/v1/entities/"+entity+"/stakeholders", bearer, map[string]string{"kind": "employee", "person_id": personID, "name": "Holder Example"})
		return st, b
	}

	t.Run("equity looks people up", func(t *testing.T) {
		eq := portForward(t, "mod-equity", "equity-api", 8081)
		eventually(t, 2*time.Minute, "an entity", func() (bool, string) {
			st, b, _ := doJSON(t, "POST", eq+"/v1/entities", bearer, map[string]string{"name": "Wiring Co", "currency": "EUR"})
			entity, _ = b["id"].(string)
			return st == 201, fmt.Sprint(st, b)
		})
		// Directory mode: the account has no directory record yet.
		if st, b := stakeholder(eq, holder); st != 403 || b["code"] != "identity_required" {
			t.Fatalf("an unlinked caller: %d %v", st, b)
		}
		newPerson("Admin", "user:"+user)
		// 201 needs both lookups: who the caller is, and that the holder's person exists.
		eventually(t, 2*time.Minute, "the linked caller to create a holder", func() (bool, string) {
			st, b := stakeholder(eq, holder)
			return st == 201, fmt.Sprint(st, b)
		})
		if st, b := stakeholder(eq, "pers_0000000000000000000000ff"); st != 400 {
			t.Errorf("a person id the directory does not have: %d %v", st, b)
		}

		t.Run("fails closed while the directory is down", func(t *testing.T) {
			mustKubectl(t, "-n", "mod-people", "scale", "deploy/people", "--replicas=0")
			defer func() {
				mustKubectl(t, "-n", "mod-people", "scale", "deploy/people", "--replicas=1")
				rolledOut(t, "people")
			}()
			eventually(t, 2*time.Minute, "503 directory_unavailable", func() (bool, string) {
				st, b := stakeholder(eq, holder)
				return st == 503 && b["code"] == "directory_unavailable", fmt.Sprint(st, b)
			})
		})
	})

	t.Run("disabling a side removes the connection", func(t *testing.T) {
		canApplyModules(t, wiringCompany("people", "provisioning")(t))
		eventually(t, 4*time.Minute, "every connection to be removed", func() (bool, string) {
			left, err := kubectl("get", "networkpolicy", "-A", "-l", "can.dev/wire", "-o", "name")
			return err == nil && left == "", left
		})
		eventually(t, 4*time.Minute, "equity to be applied without the directory", func() (bool, string) {
			conf, err := kubectl("-n", "mod-equity", "get", "configmap", "equity-config", "-o", "jsonpath={.data}")
			tok, terr := kubectl("-n", "mod-equity", "get", "secret", "can-module", "-o", "jsonpath={.data.CAN_EQUITY_DIRECTORY_TOKEN}")
			return err == nil && terr == nil && !strings.Contains(conf, "directory:") && tok == "", conf
		})
		rolledOut(t, "equity")
		// And back: enabling people again reconnects it.
		canApplyModules(t, wiringCompany("provisioning")(t))
		eventually(t, 5*time.Minute, "equity to be connected again", func() (bool, string) {
			conf, _ := kubectl("-n", "mod-equity", "get", "configmap", "equity-config", "-o", "jsonpath={.data}")
			return hasPolicy("mod-equity", "can-wire-to-people") && hasPolicy("mod-people", "can-wire-from-equity") && strings.Contains(conf, "directory:"), conf
		})
		rolledOut(t, "people", "equity")
		if hasPolicy("mod-people", "can-wire-from-provisioning") {
			t.Error("the disabled provisioning module is still admitted by people")
		}
	})
}
