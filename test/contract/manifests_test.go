package contract

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/provider"
)

// Offline validation of deploy/base and dev/k3d.yaml (epic E02). Nothing here talks to a
// cluster; the cluster-backed assertions live in test/integration (build tag `integration`).

type obj map[string]any

func (o obj) str(path ...string) string {
	v, _ := o.get(path...).(string)
	return v
}

func (o obj) get(path ...string) any {
	var cur any = map[string]any(o)
	for _, p := range path {
		switch m := cur.(type) {
		case map[string]any:
			cur = m[p]
		case obj:
			cur = m[p]
		default:
			return nil
		}
	}
	return cur
}

func (o obj) kind() string { return o.str("kind") }
func (o obj) name() string { return o.str("metadata", "name") }
func (o obj) ns() string   { return o.str("metadata", "namespace") }

func loadDocs(t *testing.T, rel string) []obj {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var docs []obj
	for {
		var m map[string]any
		err := dec.Decode(&m)
		o := obj(m)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if o != nil {
			docs = append(docs, o)
		}
	}
}

// baseDocs loads every resource listed by deploy/base/kustomization.yaml.
func baseDocs(t *testing.T) []obj {
	t.Helper()
	k := loadDocs(t, "deploy/base/kustomization.yaml")
	if len(k) != 1 || k[0].kind() != "Kustomization" {
		t.Fatal("deploy/base/kustomization.yaml must hold one Kustomization")
	}
	res, _ := k[0].get("resources").([]any)
	if len(res) == 0 {
		t.Fatal("kustomization lists no resources")
	}
	var docs []obj
	for _, r := range res {
		docs = append(docs, loadDocs(t, "deploy/base/"+r.(string))...)
	}
	return docs
}

func ofKind(docs []obj, kind string) []obj {
	var out []obj
	for _, d := range docs {
		if d.kind() == kind {
			out = append(out, d)
		}
	}
	return out
}

var platformNamespaces = []string{"can-system", "can-data", "can-portal"}

func TestKustomizationListsEveryManifestFile(t *testing.T) {
	k := loadDocs(t, "deploy/base/kustomization.yaml")[0]
	listed := map[string]bool{}
	for _, r := range k.get("resources").([]any) {
		listed[r.(string)] = true
	}
	ents, _ := os.ReadDir(filepath.Join(root, "deploy/base"))
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".yaml") && n != "kustomization.yaml" && !listed[n] {
			t.Errorf("%s is not listed in kustomization.yaml", n)
		}
	}
}

func TestNamespaces(t *testing.T) {
	have := map[string]obj{}
	for _, d := range ofKind(baseDocs(t), "Namespace") {
		have[d.name()] = d
	}
	for _, n := range platformNamespaces {
		d, ok := have[n]
		if !ok {
			t.Errorf("namespace %s missing", n)
			continue
		}
		enforce := d.str("metadata", "labels", "pod-security.kubernetes.io/enforce")
		if enforce == "" {
			t.Errorf("%s: no pod-security enforce label", n)
		}
		// SPEC 3.1: can-system is exempt from `restricted`; can-portal must be restricted.
		if n == "can-portal" && enforce != "restricted" {
			t.Errorf("can-portal must enforce restricted, got %q", enforce)
		}
		if n == "can-system" && enforce == "restricted" {
			t.Errorf("can-system needs a hostPath mount and cannot be restricted")
		}
	}
	if _, ok := have["kube-system"]; ok {
		t.Error("kube-system is owned by k3s and must not be declared")
	}
}

func TestDefaultDenyInEveryPlatformNamespace(t *testing.T) {
	for _, ns := range platformNamespaces {
		found := false
		for _, p := range ofKind(baseDocs(t), "NetworkPolicy") {
			if p.ns() != ns {
				continue
			}
			sel, _ := p.get("spec", "podSelector").(map[string]any)
			types, _ := p.get("spec", "policyTypes").([]any)
			_, hasIn := p.get("spec", "ingress").([]any)
			_, hasEg := p.get("spec", "egress").([]any)
			if sel != nil && len(sel) == 0 && len(types) == 2 && !hasIn && !hasEg {
				found = true
			}
		}
		if !found {
			t.Errorf("%s has no default-deny (ingress+egress, empty podSelector, no rules) NetworkPolicy", ns)
		}
	}
	for _, p := range ofKind(baseDocs(t), "NetworkPolicy") {
		if p.ns() == "kube-system" {
			t.Errorf("%s: default-deny must not apply to kube-system", p.name())
		}
		if p.ns() == "" {
			t.Errorf("%s: namespace not set", p.name())
		}
	}
}

func policiesIn(t *testing.T, ns string) map[string]obj {
	m := map[string]obj{}
	for _, p := range ofKind(baseDocs(t), "NetworkPolicy") {
		if p.ns() == ns {
			m[p.name()] = p
		}
	}
	return m
}

func TestExplicitAllows(t *testing.T) {
	for _, ns := range platformNamespaces {
		if _, ok := policiesIn(t, ns)["allow-dns"]; !ok {
			t.Errorf("%s: missing allow-dns egress policy", ns)
		}
	}
	// DNS allow must target kube-dns in kube-system on port 53.
	dns := policiesIn(t, "can-system")["allow-dns"]
	flat := mustJSON(dns)
	for _, want := range []string{"kube-system", "kube-dns", "53"} {
		if !strings.Contains(flat, want) {
			t.Errorf("allow-dns does not mention %q: %s", want, flat)
		}
	}
	// Traefik (kube-system) ingress to can-system and can-portal.
	for _, ns := range []string{"can-system", "can-portal"} {
		p, ok := policiesIn(t, ns)["allow-ingress-from-traefik"]
		if !ok {
			t.Errorf("%s: missing allow-ingress-from-traefik", ns)
			continue
		}
		if f := mustJSON(p); !strings.Contains(f, "kube-system") || !strings.Contains(f, "traefik") {
			t.Errorf("%s: allow-ingress-from-traefik must select traefik in kube-system: %s", ns, f)
		}
	}
	// can-core module API reachable from module namespaces only.
	api, ok := policiesIn(t, "can-system")["allow-module-api-from-modules"]
	if !ok {
		t.Fatal("can-system: missing allow-module-api-from-modules")
	}
	if f := mustJSON(api); !strings.Contains(f, "can.dev/namespace-kind") || !strings.Contains(f, "module") || !strings.Contains(f, "can-core") {
		t.Errorf("module API allow must select module namespaces and can-core pods: %s", f)
	}
	// Databases accept connections from can-core; each database module is admitted by a policy of its
	// own that can-core writes (specs/041).
	db, ok := policiesIn(t, "can-data")["allow-db-from-modules-and-core"]
	if !ok {
		t.Fatal("can-data: missing allow-db-from-modules-and-core")
	}
	if f := mustJSON(db); !strings.Contains(f, "5432") || !strings.Contains(f, "3306") || !strings.Contains(f, "can-system") {
		t.Errorf("db allow must cover 5432, 3306 and can-system: %s", f)
	}
	if strings.Contains(mustJSON(db), "namespace-kind") {
		t.Errorf("the base policy must not admit every module namespace to the databases: %s", mustJSON(db))
	}
	// can-core egress: modules, databases, Kubernetes API.
	eg, ok := policiesIn(t, "can-system")["allow-can-core-egress"]
	if !ok {
		t.Fatal("can-system: missing allow-can-core-egress")
	}
	if f := mustJSON(eg); !strings.Contains(f, "can-data") || !strings.Contains(f, "6443") {
		t.Errorf("can-core egress must reach can-data and the API server: %s", f)
	}
	// No policy may allow everything (an empty rule or 0.0.0.0/0 without except) except can-core egress.
	for _, p := range ofKind(baseDocs(t), "NetworkPolicy") {
		if p.name() == "allow-can-core-egress" {
			continue
		}
		if strings.Contains(mustJSON(p), "0.0.0.0/0") {
			t.Errorf("%s/%s opens egress to the whole internet", p.ns(), p.name())
		}
	}
}

func TestModuleNamespaceTemplate(t *testing.T) {
	docs := loadDocs(t, "deploy/templates/module-namespace.yaml")
	if len(ofKind(docs, "Namespace")) != 1 {
		t.Fatal("template needs exactly one Namespace")
	}
	ns := ofKind(docs, "Namespace")[0]
	if ns.str("metadata", "labels", "can.dev/namespace-kind") != "module" {
		t.Error("module namespace must be labelled can.dev/namespace-kind=module")
	}
	if ns.str("metadata", "labels", "pod-security.kubernetes.io/enforce") == "" {
		t.Error("module namespace needs a pod-security enforce label")
	}
	var deny, egress, ingressTraefik, ingressCore bool
	for _, p := range ofKind(docs, "NetworkPolicy") {
		f := mustJSON(p)
		switch p.name() {
		case "allow-ingress-from-traefik":
			if !strings.Contains(f, "Ingress") || !strings.Contains(f, "kube-system") || !strings.Contains(f, "traefik") {
				t.Errorf("allow-ingress-from-traefik must admit Traefik in kube-system: %s", f)
			}
			ingressTraefik = true
		case "allow-ingress-from-can-core":
			if !strings.Contains(f, "Ingress") || !strings.Contains(f, "can-system") || !strings.Contains(f, "can-core") {
				t.Errorf("allow-ingress-from-can-core must admit can-core pods in can-system: %s", f)
			}
			ingressCore = true
		}
		if p.name() == "default-deny" {
			deny = true
		}
		if p.name() == "module-egress" {
			egress = true
			for _, want := range []string{"169.254.0.0/16", "10.42.0.0/16", "10.43.0.0/16", "${NODE_IP}/32", "can-core", "53"} {
				if !strings.Contains(f, want) {
					t.Errorf("module-egress must mention %q", want)
				}
			}
		}
	}
	if !ingressTraefik || !ingressCore {
		t.Errorf("template needs ingress allows from Traefik (%v) and can-core (%v): default-deny would otherwise block module UIs and webhooks", ingressTraefik, ingressCore)
	}
	if !deny || !egress {
		t.Errorf("template needs default-deny (%v) and module-egress (%v)", deny, egress)
	}
	if len(ofKind(docs, "ResourceQuota")) != 1 || len(ofKind(docs, "LimitRange")) != 1 {
		t.Error("template needs a ResourceQuota and a LimitRange")
	}
}

func TestQuotaAndLimitRangeInEveryNamespace(t *testing.T) {
	docs := baseDocs(t)
	for _, ns := range platformNamespaces {
		var q, l bool
		for _, d := range ofKind(docs, "ResourceQuota") {
			q = q || d.ns() == ns
		}
		for _, d := range ofKind(docs, "LimitRange") {
			if d.ns() != ns {
				continue
			}
			l = true
			f := mustJSON(d)
			for _, want := range []string{"defaultRequest", "default", "memory", "cpu"} {
				if !strings.Contains(f, want) {
					t.Errorf("%s LimitRange missing %q", ns, want)
				}
			}
		}
		if !q || !l {
			t.Errorf("%s: ResourceQuota=%v LimitRange=%v", ns, q, l)
		}
	}
}

func TestTraefikHelmChartConfig(t *testing.T) {
	cs := ofKind(baseDocs(t), "HelmChartConfig")
	if len(cs) != 1 {
		t.Fatalf("want one HelmChartConfig, got %d", len(cs))
	}
	c := cs[0]
	if c.name() != "traefik" || c.ns() != "kube-system" || c.str("apiVersion") != "helm.cattle.io/v1" {
		t.Fatalf("must be helm.cattle.io/v1 traefik in kube-system: %s %s %s", c.str("apiVersion"), c.name(), c.ns())
	}
	var vm map[string]any
	if err := yaml.Unmarshal([]byte(c.str("spec", "valuesContent")), &vm); err != nil {
		t.Fatalf("valuesContent is not valid YAML: %v", err)
	}
	v := obj(vm)
	if v.get("persistence", "enabled") != true {
		t.Error("persistence must be enabled so acme.json survives restarts")
	}
	path := v.str("persistence", "path")
	storage := v.str("certificatesResolvers", "letsencrypt", "acme", "storage")
	if path == "" || !strings.HasPrefix(storage, path+"/") {
		t.Errorf("ACME storage %q must live under the persistent path %q", storage, path)
	}
	if v.str("service", "spec", "externalTrafficPolicy") != "Local" {
		t.Error("service.spec.externalTrafficPolicy must be Local (SPEC 3.1)")
	}
}

func TestCoreDNSCustom(t *testing.T) {
	var cm obj
	for _, d := range ofKind(baseDocs(t), "ConfigMap") {
		if d.name() == "coredns-custom" {
			cm = d
		}
	}
	if cm == nil {
		t.Fatal("coredns-custom ConfigMap missing")
	}
	if cm.ns() != "kube-system" {
		t.Errorf("coredns-custom must be in kube-system, got %q", cm.ns())
	}
	data, _ := cm.get("data").(map[string]any)
	var override string
	for k, v := range data {
		if !strings.HasSuffix(k, ".override") && !strings.HasSuffix(k, ".server") {
			t.Errorf("key %q is not imported by k3s CoreDNS (*.override or *.server)", k)
		}
		override += v.(string) + "\n"
	}
	if !strings.Contains(override, `localtest\.me`) || !strings.Contains(override, "traefik.kube-system.svc.cluster.local") {
		t.Errorf("must rewrite *.localtest.me to the Traefik Service:\n%s", override)
	}
	if !strings.Contains(override, "can-core-api.can-system.svc.cluster.local") {
		t.Error("must alias the in-cluster module API hostname")
	}
	// admin.* must never be rewritten (SPEC 3.1): an exact no-op rule has to precede the wildcard.
	adminIdx := strings.Index(override, "admin.localtest.me admin.localtest.me")
	wildIdx := strings.Index(override, `localtest\.me`)
	if adminIdx < 0 || adminIdx > wildIdx {
		t.Error("admin.localtest.me must be pinned (no-op stop rule) before the wildcard rewrite")
	}
	if !strings.Contains(override, "answer auto") {
		t.Error("rewrite must restore the question name in answers (answer auto)")
	}
}

// rulesOf returns the rules of a Role or ClusterRole as maps.
func rulesOf(o obj) []map[string]any {
	var out []map[string]any
	for _, r := range o.get("rules").([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

func clusterRole(t *testing.T, docs []obj, name string) obj {
	t.Helper()
	for _, r := range ofKind(docs, "ClusterRole") {
		if r.name() == name {
			return r
		}
	}
	t.Fatalf("ClusterRole %s not found", name)
	return nil
}

var workloadResources = map[string]bool{"pods": true, "deployments": true, "statefulsets": true, "daemonsets": true,
	"replicasets": true, "jobs": true, "cronjobs": true}

func TestRBACLeastPrivilege(t *testing.T) {
	docs := baseDocs(t)
	var sa obj
	for _, d := range ofKind(docs, "ServiceAccount") {
		if d.name() == "can-core" && d.ns() == "can-system" {
			sa = d
		}
	}
	if sa == nil {
		t.Fatal("ServiceAccount can-system/can-core missing")
	}
	if sa.get("automountServiceAccountToken") == false {
		t.Error("can-core needs its token (it calls the Kubernetes API)")
	}
	rules := 0
	for _, kind := range []string{"ClusterRole", "Role"} {
		for _, r := range ofKind(docs, kind) {
			for _, rm := range rulesOf(r) {
				rules++
				for _, field := range []string{"apiGroups", "resources", "verbs", "resourceNames"} {
					for _, x := range asStrings(rm[field]) {
						if x == "*" {
							t.Errorf("%s %s: wildcard in %s", kind, r.name(), field)
						}
					}
				}
				for _, v := range asStrings(rm["verbs"]) {
					if v == "escalate" || v == "impersonate" {
						t.Errorf("%s %s grants %q", kind, r.name(), v)
					}
					// `bind` is only acceptable when pinned to named roles.
					if v == "bind" && len(asStrings(rm["resourceNames"])) == 0 {
						t.Errorf("%s %s grants unrestricted bind", kind, r.name())
					}
				}
			}
		}
	}
	if rules == 0 {
		t.Fatal("no RBAC rules found")
	}

	// Cluster scope: every role reachable through a ClusterRoleBinding must be tiny. No
	// secrets, no pods/exec, no workload writes, no namespace mutation beyond create.
	cluster := map[string]bool{}
	for _, b := range ofKind(docs, "ClusterRoleBinding") {
		if b.str("roleRef", "kind") != "ClusterRole" {
			t.Errorf("ClusterRoleBinding %s must reference a ClusterRole", b.name())
		}
		cluster[b.str("roleRef", "name")] = true
	}
	if len(cluster) == 0 {
		t.Fatal("no ClusterRoleBinding found")
	}
	seen := map[string]bool{}
	for name := range cluster {
		for _, rm := range rulesOf(clusterRole(t, docs, name)) {
			for _, res := range asStrings(rm["resources"]) {
				verbs := asStrings(rm["verbs"])
				switch {
				case res == "secrets" || strings.HasPrefix(res, "secrets/"):
					t.Errorf("cluster-scope role %s grants secrets", name)
				case res == "pods/exec" || res == "pods/attach" || res == "pods/portforward":
					t.Errorf("cluster-scope role %s grants %s", name, res)
				case workloadResources[res]:
					t.Errorf("cluster-scope role %s grants workload resource %s (%v)", name, res, verbs)
				case res == "namespaces":
					for _, v := range verbs {
						if v == "update" || v == "patch" || v == "delete" || v == "deletecollection" {
							t.Errorf("cluster-scope role %s may %s namespaces (would relax pod-security labels)", name, v)
						}
					}
				case res == "clusterroles" || res == "clusterrolebindings":
					for _, v := range verbs {
						if v != "bind" || len(asStrings(rm["resourceNames"])) == 0 {
							t.Errorf("cluster-scope role %s: %s on %s must be bind on named roles only", name, v, res)
						}
					}
				}
				seen[res] = true
			}
		}
	}
	for _, want := range []string{"tokenreviews", "namespaces", "rolebindings"} {
		if !seen[want] {
			t.Errorf("can-core cluster role lacks %s (SPEC 3.1)", want)
		}
	}

	// The per-namespace role exists, is bound only by RoleBindings, and carries the workload
	// permissions that used to be cluster-wide.
	mod := clusterRole(t, docs, "can-core-module")
	if cluster["can-core-module"] {
		t.Error("can-core-module must never be bound with a ClusterRoleBinding")
	}
	if f := mustJSON(mod); !strings.Contains(f, "networkpolicies") || !strings.Contains(f, "deployments") || !strings.Contains(f, "secrets") {
		t.Errorf("can-core-module should hold the namespaced permissions: %s", f)
	}
	for _, rm := range rulesOf(mod) {
		if strings.Contains(mustJSON(rm["resources"]), "roles") {
			t.Error("can-core-module must not manage RBAC (no escalation inside module namespaces)")
		}
	}
	bound := map[string]bool{}
	for _, b := range ofKind(docs, "RoleBinding") {
		if b.ns() != "kube-system" && b.str("roleRef", "name") == "can-core-module" {
			bound[b.ns()] = true
		}
	}
	for _, ns := range []string{"can-system", "can-data"} {
		if !bound[ns] {
			t.Errorf("can-core-module is not bound in %s", ns)
		}
	}

	// Every binding must point at can-core, never cluster-admin.
	for _, kind := range []string{"ClusterRoleBinding", "RoleBinding"} {
		for _, b := range ofKind(docs, kind) {
			if b.str("roleRef", "name") == "cluster-admin" {
				t.Errorf("%s %s binds cluster-admin", kind, b.name())
			}
			subs, _ := b.get("subjects").([]any)
			for _, s := range subs {
				if sm := s.(map[string]any); sm["name"] != "can-core" || sm["namespace"] != "can-system" {
					t.Errorf("%s %s binds an unexpected subject %v", kind, b.name(), sm)
				}
			}
		}
	}
}

func TestModuleRBACTemplate(t *testing.T) {
	docs := loadDocs(t, "deploy/templates/module-rbac.yaml")
	bs := ofKind(docs, "RoleBinding")
	if len(bs) != 1 || len(docs) != 1 {
		t.Fatalf("template must be exactly one RoleBinding, got %d docs", len(docs))
	}
	b := bs[0]
	if b.ns() != "${MODULE_NAMESPACE}" || b.str("roleRef", "kind") != "ClusterRole" || b.str("roleRef", "name") != "can-core-module" {
		t.Errorf("RoleBinding must bind ClusterRole can-core-module in ${MODULE_NAMESPACE}: %s", mustJSON(b))
	}
	if f := mustJSON(b.get("subjects")); !strings.Contains(f, "can-core") || !strings.Contains(f, "can-system") {
		t.Errorf("subject must be can-system/can-core: %s", f)
	}
}

func TestNoWorkloadsInBase(t *testing.T) {
	// Base manifests carry policy and configuration only; workloads arrive with charts (E03+).
	// Any workload that does appear must pin images and set requests, limits and a security context.
	for _, d := range baseDocs(t) {
		switch d.kind() {
		case "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod":
			f := mustJSON(d)
			for _, want := range []string{"requests", "limits", "securityContext", "@sha256:"} {
				if !strings.Contains(f, want) {
					t.Errorf("%s %s: missing %q", d.kind(), d.name(), want)
				}
			}
		}
	}
}

func TestK3dConfig(t *testing.T) {
	c := loadDocs(t, "dev/k3d.yaml")[0]
	if c.str("apiVersion") != "k3d.io/v1alpha5" || c.kind() != "Simple" {
		t.Fatalf("unexpected k3d config header: %s %s", c.str("apiVersion"), c.kind())
	}
	if c.str("metadata", "name") != "companyinacan-dev" {
		t.Errorf("cluster name must match cluster.DefaultConfig, got %q", c.str("metadata", "name"))
	}
	img := c.str("image")
	if !strings.HasPrefix(img, "rancher/k3s:v") || strings.Contains(img, "latest") {
		t.Errorf("image must be pinned to an explicit k3s tag, got %q", img)
	}
	if c.get("servers") != 1 || c.get("agents") != 0 {
		t.Errorf("single-node cluster expected, got servers=%v agents=%v", c.get("servers"), c.get("agents"))
	}
	ports := mustJSON(c.get("ports"))
	for _, want := range []string{"80:80", "443:443", "loadbalancer"} {
		if !strings.Contains(ports, want) {
			t.Errorf("ports missing %q: %s", want, ports)
		}
	}
	args := mustJSON(c.get("options", "k3s", "extraArgs"))
	if !strings.Contains(args, "--disable=metrics-server") {
		t.Errorf("metrics-server must be disabled (SPEC 3.2): %s", args)
	}
	if !strings.Contains(args, "--secrets-encryption") {
		t.Errorf("secrets encryption must be on (SPEC 3.1): %s", args)
	}
	if strings.Contains(args, "traefik") {
		t.Error("traefik must stay enabled")
	}
	if c.get("options", "k3d", "wait") != true {
		t.Error("k3d should wait for the cluster")
	}
}

func mustJSON(v any) string {
	// Cheap, stable flattening for substring assertions.
	b, err := yaml.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func asStrings(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Development and CI must run the k3s servers get: an older one bundles another Traefik chart.
func TestK3dImagesMatchTheServerK3sVersion(t *testing.T) {
	want := "image: rancher/k3s:" + strings.ReplaceAll(provider.DefaultK3sVersion, "+", "-")
	paths, _ := filepath.Glob("../../dev/footprint/k3d-*.yaml")
	for _, p := range append(paths, "../../dev/k3d.yaml") {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s: want %q", p, want)
		}
	}
	if len(paths) < 3 {
		t.Fatalf("footprint configs not found: %v", paths)
	}
}
