package installer

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

func renderOne(t *testing.T, tpl string) error {
	t.Helper()
	dir := writeChart(t, map[string]string{"templates/a.yaml": tpl})
	_, err := RenderChart(dir, nil, ReleaseInfo{Name: "x", Namespace: "n"})
	return err
}

func TestRendererSurvivesHostileTemplates(t *testing.T) {
	cases := map[string]string{
		"include recursion":     `{{- define "r" -}}{{ include "r" . }}{{- end -}}{{ include "r" . }}`,
		"template recursion":    `{{- define "r" -}}{{ template "r" . }}{{- end -}}{{ template "r" . }}`,
		"nindent huge":          `{{ "x" | nindent 1000000000 }}`,
		"indent huge":           `{{ indent 1073741824 "x" }}`,
		"indent negative":       `{{ indent -5 "x" }}`,
		"trunc negative":        `{{ trunc -1 "abc" }}`,
		"replace amplification": `{{ $s := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }}{{ $s = replace "a" $s $s }}{{ $s = replace "a" $s $s }}{{ $s = replace "a" $s $s }}{{ $s = replace "a" $s $s }}{{ $s = replace "a" $s $s }}{{ $s = replace "a" $s $s }}{{ $s = replace "a" $s $s }}`,
		"exponential include": `{{- define "a" -}}x{{- end -}}{{- define "b" -}}{{ include "a" . }}{{ include "a" . }}{{- end -}}` +
			`{{- define "c" -}}{{ include "b" . }}{{ include "b" . }}{{- end -}}` +
			`{{- define "d" -}}{{ include "c" . }}{{ include "c" . }}{{- end -}}` + `{{ include "d" . }}`,
	}
	for name, body := range cases {
		files := map[string]string{"templates/a.yaml": body}
		dir := writeChart(t, files)
		done := make(chan error, 1)
		go func() {
			_, err := RenderChart(dir, nil, ReleaseInfo{Name: "x", Namespace: "n"})
			done <- err
		}()
		select {
		case err := <-done:
			if name != "exponential include" && err == nil {
				t.Errorf("%s: accepted", name)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("%s: render did not finish", name)
		}
	}
}

func TestRendererOutputCaps(t *testing.T) {
	// 1 MiB per template file: a chart that renders a long comment-ish string is refused.
	big := strings.Repeat("a", 600000)
	body := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: a}\ndata: {x: {{ .Values.big }}, y: {{ .Values.big }}}\n"
	dir := writeChart(t, map[string]string{"templates/a.yaml": body})
	if _, err := RenderChart(dir, map[string]any{"big": big}, ReleaseInfo{Name: "x", Namespace: "n"}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("%v", err)
	}
	// and a template file that is itself huge is refused before parsing
	if err := renderOne(t, "# "+strings.Repeat("a", 2<<20)+"\nkind: ConfigMap\n"); err == nil {
		t.Fatal("2 MiB template accepted")
	}
}

func TestRendererDoesNotEchoFileContents(t *testing.T) {
	dir := writeChart(t, map[string]string{
		"values.yaml":      "secret-token-value: [unclosed\n",
		"templates/a.yaml": "kind: ConfigMap\napiVersion: v1\nmetadata: {name: a}\n",
	})
	_, err := RenderChart(dir, nil, ReleaseInfo{Name: "x", Namespace: "n"})
	if err == nil || strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("%v", err)
	}
}

func TestRendererRefusesSymlinkedValuesAndTemplatesDir(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "values.yaml"), []byte("a: b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := writeChart(t, map[string]string{"templates/a.yaml": "kind: ConfigMap\napiVersion: v1\nmetadata: {name: a}\n"})
	if err := os.Symlink(filepath.Join(outside, "values.yaml"), filepath.Join(dir, "values.yaml")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := RenderChart(dir, nil, ReleaseInfo{Name: "x", Namespace: "n"}); err == nil {
		t.Error("symlinked values.yaml read")
	}
	dir2 := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir2, "templates")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := RenderChart(dir2, nil, ReleaseInfo{Name: "x", Namespace: "n"}); err == nil {
		t.Error("symlinked templates directory read")
	}
}

// ---- object-level guard rails ----

func enableWith(t *testing.T, domain string, body string) error {
	t.Helper()
	in, _, _, spec := fixture(t, map[string]string{"stub/chart/templates/evil.yaml": body})
	in.cfg.Domain = domain
	spec.Manifest.Runtime.Subdomain = "stub"
	return in.Enable(context.Background(), spec)
}

func TestChartNetworkPolicies(t *testing.T) {
	np := func(spec string) string {
		return "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata: {name: extra}\nspec:\n" + spec
	}
	bad := map[string]string{
		"allow all ingress":      np("  podSelector: {matchLabels: {a: b}}\n  ingress: [{}]\n"),
		"allow all egress":       np("  podSelector: {matchLabels: {a: b}}\n  policyTypes: [Egress]\n  egress: [{}]\n"),
		"select all pods":        np("  podSelector: {}\n  ingress: [{from: [{podSelector: {matchLabels: {a: b}}}]}]\n"),
		"ipBlock":                np("  podSelector: {matchLabels: {a: b}}\n  egress: [{to: [{ipBlock: {cidr: 0.0.0.0/0}}]}]\n"),
		"namespaceSelector":      np("  podSelector: {matchLabels: {a: b}}\n  ingress: [{from: [{namespaceSelector: {}}]}]\n"),
		"namespace and pod peer": np("  podSelector: {matchLabels: {a: b}}\n  ingress: [{from: [{namespaceSelector: {}, podSelector: {matchLabels: {a: b}}}]}]\n"),
		"ports only":             np("  podSelector: {matchLabels: {a: b}}\n  ingress: [{ports: [{port: 80}]}]\n"),
		"empty peer list":        np("  podSelector: {matchLabels: {a: b}}\n  ingress: [{from: []}]\n"),
	}
	for name, body := range bad {
		if err := enableWith(t, "a.example", body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := np("  podSelector: {matchLabels: {app: stub}}\n  policyTypes: [Ingress, Egress]\n  ingress: [{from: [{podSelector: {matchLabels: {app: stub}}}]}]\n  egress: [{to: [{podSelector: {matchLabels: {app: stub}}}]}]\n")
	if err := enableWith(t, "a.example", ok); err != nil {
		t.Fatalf("the same-namespace shape used by the starter charts must be allowed: %v", err)
	}
}

func TestChartServices(t *testing.T) {
	svc := func(spec string) string {
		return "apiVersion: v1\nkind: Service\nmetadata: {name: pub}\nspec:\n  selector: {app: stub}\n  ports: [{port: 80" + spec
	}
	bad := map[string]string{
		"NodePort":       "apiVersion: v1\nkind: Service\nmetadata: {name: pub}\nspec:\n  type: NodePort\n  selector: {app: stub}\n  ports: [{port: 80}]\n",
		"LoadBalancer":   "apiVersion: v1\nkind: Service\nmetadata: {name: pub}\nspec:\n  type: LoadBalancer\n  selector: {app: stub}\n  ports: [{port: 80}]\n",
		"ExternalName":   "apiVersion: v1\nkind: Service\nmetadata: {name: pub}\nspec:\n  type: ExternalName\n  externalName: evil.example\n",
		"externalIPs":    "apiVersion: v1\nkind: Service\nmetadata: {name: pub}\nspec:\n  externalIPs: [1.2.3.4]\n  selector: {app: stub}\n  ports: [{port: 80}]\n",
		"nodePort field": svc(", nodePort: 30080}]\n"),
	}
	for name, body := range bad {
		if err := enableWith(t, "a.example", body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := enableWith(t, "a.example", svc("}]\n")); err != nil {
		t.Fatalf("plain ClusterIP service refused: %v", err)
	}
}

func TestChartRouting(t *testing.T) {
	route := func(match string) string {
		return "apiVersion: traefik.io/v1alpha1\nkind: IngressRoute\nmetadata: {name: r}\nspec:\n  routes:\n    - match: \"" + match + "\"\n      kind: Rule\n      services: [{name: stub, port: 80}]\n"
	}
	bad := []struct{ name, domain, body string }{
		{"foreign host", "a.example", route("Host(`admin.a.example`)")},
		{"second host", "a.example", route("Host(`stub.a.example`) || Host(`admin.a.example`)")},
		{"two args", "a.example", route("Host(`stub.a.example`, `admin.a.example`)")},
		{"regexp", "a.example", route("HostRegexp(`.+`)")},
		{"no host", "a.example", route("PathPrefix(`/`)")},
		{"no company domain", "", route("Host(`stub.a.example`)")},
		{"other namespace service", "a.example", "apiVersion: traefik.io/v1alpha1\nkind: IngressRoute\nmetadata: {name: r}\nspec:\n  routes:\n    - match: \"Host(`stub.a.example`)\"\n      kind: Rule\n      services: [{name: x, port: 80, namespace: can-system}]\n"},
		{"middleware kind", "a.example", "apiVersion: traefik.io/v1alpha1\nkind: Middleware\nmetadata: {name: m}\nspec: {}\n"},
		{"plain ingress", "a.example", "apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata: {name: i}\nspec: {}\n"},
	}
	for _, c := range bad {
		if err := enableWith(t, c.domain, c.body); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if err := enableWith(t, "a.example", route("Host(`stub.a.example`) && PathPrefix(`/x`)")); err != nil {
		t.Fatalf("the module's own host must be routable: %v", err)
	}
}

func TestChartSecretsAndNames(t *testing.T) {
	cases := map[string]string{
		"sa token secret":   "apiVersion: v1\nkind: Secret\ntype: kubernetes.io/service-account-token\nmetadata: {name: tok}\n",
		"sa annotation":     "apiVersion: v1\nkind: Secret\nmetadata: {name: tok, annotations: {kubernetes.io/service-account.name: default}}\n",
		"uppercase name":    "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: Bad_Name}\n",
		"clusterrole":       "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata: {name: x}\n",
		"extra serviceacct": "apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: other}\n",
		"admission policy":  "apiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingAdmissionPolicy\nmetadata: {name: x}\n",
	}
	for name, body := range cases {
		if err := enableWith(t, "a.example", body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// ---- deletion, adoption, release integrity ----

func TestPruneAndRollbackOnlyDeleteOwnedObjects(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	// Someone replaced the chart's Service with an object of the same name without our labels.
	api.Objects["Service/mod-stub/stub"] = kube.Object{"kind": "Service", "metadata": map[string]any{"name": "stub", "namespace": "mod-stub", "labels": map[string]any{"owner": "someone-else"}}}
	writeFiles(t, in.cfg.ModulesDir, map[string]string{"stub/chart/templates/svc.yaml": ""})
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Get(context.Background(), "Service", "mod-stub", "stub"); err != nil {
		t.Fatal("pruning deleted an object that does not carry the module's labels")
	}
}

func TestTamperedReleaseRecordIsRefused(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	rec := api.Objects["Secret/mod-stub/can-release"]
	data := rec["data"].(map[string]any)
	data["hmac"] = base64.StdEncoding.EncodeToString([]byte("0000"))
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "HMAC") {
		t.Fatalf("%v", err)
	}
}

func TestReleaseRecordWithForeignNamespaceObjectIsRefused(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	// A forged but self-consistent record that names an object in kube-system.
	forged := []kube.Object{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "coredns-custom", "namespace": "kube-system"}}}
	if err := in.saveReleaseObjects(context.Background(), spec, forged); err != nil {
		t.Fatal(err)
	}
	_ = api
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("a release record targeting kube-system was replayed")
	}
}

func TestExistingNamespaceAndBindingAreVerifiedBeforeAdoption(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	api.Objects["Namespace//mod-stub"] = kube.Object{"kind": "Namespace", "metadata": map[string]any{"name": "mod-stub", "labels": map[string]any{"pod-security.kubernetes.io/enforce": "privileged"}}}
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("a pre-existing relaxed namespace was adopted: %v", err)
	}

	in, api, _, spec = fixture(t, nil)
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	rb := api.Objects["RoleBinding/mod-stub/can-core-module"]
	rb["subjects"] = []any{map[string]any{"kind": "User", "name": "attacker"}}
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("a binding to another subject was adopted: %v", err)
	}
}

func TestEnableRefusedWithoutAdmissionPolicies(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	in.cfg.RequireAdmission = true
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "admission policy") {
		t.Fatalf("%v", err)
	}
	if _, e := api.Get(context.Background(), "Namespace", "", "mod-stub"); !kube.IsNotFound(e) {
		t.Fatal("namespace created without the admission policies")
	}
	for _, n := range []string{"can-core-rolebindings", "can-core-namespaces"} {
		api.Objects["ValidatingAdmissionPolicy//"+n] = kube.Object{"kind": "ValidatingAdmissionPolicy"}
		api.Objects["ValidatingAdmissionPolicyBinding//"+n] = kube.Object{"kind": "ValidatingAdmissionPolicyBinding", "spec": map[string]any{"validationActions": []any{"Warn"}}}
	}
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "Deny") {
		t.Fatalf("a Warn-only binding was accepted: %v", err)
	}
	for _, n := range []string{"can-core-rolebindings", "can-core-namespaces"} {
		api.Objects["ValidatingAdmissionPolicyBinding//"+n]["spec"] = map[string]any{"validationActions": []any{"Deny"}}
	}
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
}

func TestStarterModuleChartsStillInstall(t *testing.T) {
	for _, id := range []string{"support", "newsletter", "analytics", "wiki", "crm", "status"} {
		t.Run(id, func(t *testing.T) {
			in, _, _, spec := fixture(t, nil)
			in.cfg.ModulesDir = "../../../modules"
			in.cfg.Domain = "acme.example"
			spec.ID, spec.Namespace, spec.ServiceAccount = id, "mod-"+id, id
			spec.Manifest = loadManifest(t, "../../../modules/"+id+"/module.yaml")
			spec.Manifest.Requires.Database = "none"
			if err := in.Enable(context.Background(), spec); err != nil {
				t.Fatalf("%s chart no longer installs: %v", id, err)
			}
		})
	}
}
