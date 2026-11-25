package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Offline checks of the E03 part 2 manifests: the admission policies and the can-core Deployment.
// The cluster-backed proof is test/integration/cancore_test.go.

func TestAdmissionPoliciesAreBoundAndDeny(t *testing.T) {
	docs := baseDocs(t)
	pol := map[string]obj{}
	for _, d := range ofKind(docs, "ValidatingAdmissionPolicy") {
		pol[d.name()] = d
	}
	bound := map[string]obj{}
	for _, d := range ofKind(docs, "ValidatingAdmissionPolicyBinding") {
		bound[d.str("spec", "policyName")] = d
	}
	for _, name := range []string{"can-core-rolebindings", "can-core-namespaces"} {
		p, ok := pol[name]
		if !ok {
			t.Errorf("policy %s missing", name)
			continue
		}
		if p.str("spec", "failurePolicy") != "Fail" {
			t.Errorf("%s: failurePolicy must be Fail", name)
		}
		b, ok := bound[name]
		if !ok {
			t.Errorf("policy %s has no binding (an unbound policy enforces nothing)", name)
			continue
		}
		acts, _ := b.get("spec", "validationActions").([]any)
		if len(acts) != 1 || acts[0] != "Deny" {
			t.Errorf("%s: binding must Deny, got %v", name, acts)
		}
		// Only can-core's ServiceAccount is constrained: administrators and k3s are untouched.
		mc, _ := p.get("spec", "matchConditions").([]any)
		if len(mc) == 0 || !strings.Contains(mc[0].(map[string]any)["expression"].(string), "system:serviceaccount:can-system:can-core") {
			t.Errorf("%s: must be limited to the can-core ServiceAccount", name)
		}
	}
	// The rules the RBAC residual risk needs: mod-* only, one ClusterRole, restricted namespaces.
	all := ""
	for _, p := range pol {
		vs, _ := p.get("spec", "validations").([]any)
		for _, v := range vs {
			all += v.(map[string]any)["expression"].(string) + "\n"
		}
	}
	for _, want := range []string{"startsWith('mod-')", "can-core-module", "restricted", "can.dev/namespace-kind"} {
		if !strings.Contains(all, want) {
			t.Errorf("no validation mentions %q", want)
		}
	}
}

func TestCanCorePolicyMatchesRBACGrant(t *testing.T) {
	// The policy is only meaningful while RBAC lets can-core create these objects at all.
	rbac := loadDocs(t, "deploy/base/rbac.yaml")
	var cr obj
	for _, d := range ofKind(rbac, "ClusterRole") {
		if d.name() == "can-core" {
			cr = d
		}
	}
	if cr == nil {
		t.Fatal("ClusterRole can-core missing")
	}
	text := toJSON(cr.get("rules"))
	for _, want := range []string{`"namespaces"`, `"rolebindings"`, `"can-core-module"`, `"tokenreviews"`} {
		if !strings.Contains(text, want) {
			t.Errorf("ClusterRole can-core lost %s: the admission policy was written against it", want)
		}
	}
	if strings.Contains(text, "admissionregistration") {
		t.Error("can-core must not be able to edit the admission policies that constrain it")
	}
}

func TestCanCoreDeployment(t *testing.T) {
	docs := loadDocs(t, "deploy/can-core/can-core.yaml")
	deps := ofKind(docs, "Deployment")
	if len(deps) != 1 {
		t.Fatalf("want one Deployment, got %d", len(deps))
	}
	d := deps[0]
	if d.ns() != "can-system" || d.str("spec", "strategy", "type") != "Recreate" {
		t.Errorf("namespace/strategy: %s %s (SQLite has a single writer)", d.ns(), d.str("spec", "strategy", "type"))
	}
	spec := d.get("spec", "template", "spec").(map[string]any)
	if spec["serviceAccountName"] != "can-core" {
		t.Error("must run as ServiceAccount can-core")
	}
	psc := spec["securityContext"].(map[string]any)
	if psc["runAsNonRoot"] != true {
		t.Error("runAsNonRoot")
	}
	c := spec["containers"].([]any)[0].(map[string]any)
	sc := c["securityContext"].(map[string]any)
	if sc["allowPrivilegeEscalation"] != false || sc["readOnlyRootFilesystem"] != true {
		t.Errorf("container securityContext %v", sc)
	}
	res := c["resources"].(map[string]any)
	req, lim := res["requests"].(map[string]any), res["limits"].(map[string]any)
	if req["memory"] != "48Mi" || lim["memory"] != "96Mi" {
		t.Errorf("resources must match the declared ram_mb 48 / ram_limit_mb 96: %v", res)
	}
	for _, p := range []string{"readinessProbe", "livenessProbe"} {
		if c[p] == nil {
			t.Errorf("%s missing", p)
		}
	}
	env := map[string]string{}
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		if v, ok := m["value"].(string); ok {
			env[m["name"].(string)] = v
		}
	}
	for k, v := range map[string]string{"CAN_TRUST_PROXY": "true", "CAN_TOKEN_REVIEW": "kubernetes", "CAN_INSTALLER": "kubernetes", "CAN_COOKIE_SECURE": "true", "CAN_MODULE_ADDR": ":8090", "CAN_ADMIN_ADDR": ":8080"} {
		if env[k] != v {
			t.Errorf("%s=%q, want %q (specs/005-can-core-mvp/deployment.md)", k, env[k], v)
		}
	}
	// The pod must match the selectors the NetworkPolicies use.
	if d.get("spec", "template", "metadata", "labels").(map[string]any)["app.kubernetes.io/name"] != "can-core" {
		t.Error("pod label app.kubernetes.io/name=can-core is what the NetworkPolicies select")
	}
	// Service ports are the ones the policies allow.
	svc := ofKind(docs, "Service")[0]
	ports := toJSON(svc.get("spec", "ports"))
	if !strings.Contains(ports, `"port":8080`) || !strings.Contains(ports, `"port":8090`) {
		t.Errorf("service ports %s", ports)
	}
}

func TestCanCoreDockerfileUsesNonRootAndBakesModules(t *testing.T) {
	b := readFile(t, "deploy/can-core/Dockerfile")
	for _, want := range []string{"USER 65532", "COPY modules /modules", "ENTRYPOINT [\"/can-core\"]", "distroless"} {
		if !strings.Contains(b, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func readFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
