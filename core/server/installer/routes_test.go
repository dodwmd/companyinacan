package installer

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/routing"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// Routing (specs/040-module-routing) against the real charts, manifests and renderer, with the fake
// API. What the API server's admission policy refuses is test/integration/z_routing_k3d_test.go.

func routed(t *testing.T, id string, x config.Expose) (*Installer, *kube.Fake, reconcile.Spec) {
	t.Helper()
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir = "../../../modules"
	spec.ID, spec.Namespace, spec.ServiceAccount = id, "mod-"+id, id
	spec.Manifest = loadManifest(t, "../../../modules/"+id+"/module.yaml")
	spec.Manifest.Requires.Database = "none"
	spec.Domain, spec.Expose = "acme.example", x
	return in, api, spec
}

func enable(t *testing.T, in *Installer, spec reconcile.Spec) {
	t.Helper()
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
}

func exists(api *kube.Fake, kind, ns, name string) bool {
	_, err := api.Get(context.Background(), kind, ns, name)
	return err == nil
}

func traefikPorts(t *testing.T, api *kube.Fake, ns string) string {
	t.Helper()
	return toJSON(dig(get(t, api, "NetworkPolicy", ns, policyTraefik), "spec", "ingress"))
}

// The policy is there, closed, from the first moment, and an upgrade does not shut a running route.
func TestTraefikPolicyExistsClosedBeforeTheRollout(t *testing.T) {
	in, api, spec := routed(t, "work", config.Expose{UI: true})
	api.NotReady["work"] = true
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("the rollout should have failed")
	}
	if got := traefikPorts(t, api, spec.Namespace); got != "[]" {
		t.Errorf("a module that never became ready is open: %s", got)
	}
	delete(api.NotReady, "work")
	enable(t, in, spec)
	api.NotReady["work"] = true
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("the upgrade should have failed")
	}
	if got := traefikPorts(t, api, spec.Namespace); !strings.Contains(got, `"port":8080`) {
		t.Errorf("a failed upgrade shut the running release off: %s", got)
	}
}

func TestNothingIsRoutedByDefault(t *testing.T) {
	for _, id := range []string{"work", "people", "finance"} {
		in, api, spec := routed(t, id, config.Expose{})
		enable(t, in, spec)
		for k := range api.Objects {
			if strings.HasPrefix(k, "IngressRoute/") || strings.HasPrefix(k, "Middleware/") {
				t.Errorf("%s: %s without expose", id, k)
			}
		}
		if got := traefikPorts(t, api, spec.Namespace); got != "[]" {
			t.Errorf("%s: the ingress controller is admitted without a route: %s", id, got)
		}
	}
}

func TestExposedUIRoutesOnlyUIPaths(t *testing.T) {
	loaders := map[string]func([]byte) error{}
	for _, m := range inhouseModules() {
		loaders[m.id] = m.parseConf
	}
	for _, c := range []struct {
		id, port, want string
	}{
		{"work", "8080", "PathPrefix(`/ui/`)"},
		{"people", "8081", "PathPrefix(`/people/`)"},
		{"jobs", "8080", "PathPrefix(`/applications/`)"},
		{"finance", "8080", "PathPrefix(`/invoices/`)"},
	} {
		t.Run(c.id, func(t *testing.T) {
			in, api, spec := routed(t, c.id, config.Expose{UI: true})
			enable(t, in, spec)
			r := get(t, api, "IngressRoute", spec.Namespace, routeWeb)
			js := toJSON(r["spec"])
			if !strings.Contains(js, "Host(`"+c.id+".acme.example`) \\u0026\\u0026 "+c.want) {
				t.Errorf("route: %s", js)
			}
			for _, never := range []string{"/v1", "/healthz", "/readyz", "/metrics", "/public", "PathPrefix(`/`)"} {
				if strings.Contains(js, never) {
					t.Errorf("%s is routed: %s", never, js)
				}
			}
			if toJSON(dig(r, "spec", "entryPoints")) != `["websecure"]` || dig(r, "spec", "tls", "certResolver") != "letsencrypt" {
				t.Errorf("not TLS like the rest of the stack: %s", js)
			}
			if !strings.Contains(js, middlewareWeb) {
				t.Errorf("no security headers: %s", js)
			}
			h := toJSON(get(t, api, "Middleware", spec.Namespace, middlewareWeb)["spec"])
			for _, want := range []string{"stsSeconds", "contentTypeNosniff", "frameDeny"} {
				if !strings.Contains(h, want) {
					t.Errorf("headers lack %s: %s", want, h)
				}
			}
			np := traefikPorts(t, api, spec.Namespace)
			if !strings.Contains(np, `"port":`+c.port) || strings.Count(np, `"port":`) != 1 || !strings.Contains(np, `"app.kubernetes.io/name":"traefik"`) {
				t.Errorf("NetworkPolicy: %s", np)
			}
			// the module runs with the UI on, without the paste-a-token form, and its own loader agrees
			raw, _ := dig(get(t, api, "ConfigMap", spec.Namespace, c.id+"-config"), "data", c.id+".yaml").(string)
			if !strings.Contains(raw, "token_login: false") || (c.id != "work" && !strings.Contains(raw, "enabled: true")) {
				t.Errorf("config: %s", raw)
			}
			if err := loaders[c.id]([]byte(raw)); err != nil {
				t.Errorf("the module refuses the config: %v\n%s", err, raw)
			}
			d := get(t, api, "Deployment", spec.Namespace, c.id)
			if dig(d, "spec", "template", "metadata", "annotations", annotationRoute) == nil {
				t.Error("the pods would not restart when exposure changes")
			}

			// switched off again: route, headers and the ingress controller's way in go
			spec.Expose = config.Expose{}
			enable(t, in, spec)
			if exists(api, "IngressRoute", spec.Namespace, routeWeb) || exists(api, "Middleware", spec.Namespace, middlewareWeb) {
				t.Error("route left behind")
			}
			// jobs keeps its careers route, so its port stays open
			if got := traefikPorts(t, api, spec.Namespace); (got != "[]") != (c.id == "jobs") {
				t.Errorf("NetworkPolicy: %s", got)
			}
			d = get(t, api, "Deployment", spec.Namespace, c.id)
			// jobs keeps its public_config while the careers pages are routed
			if c.id != "jobs" && dig(d, "spec", "template", "metadata", "annotations", annotationRoute) != nil {
				t.Error("exposure annotation left behind")
			}
		})
	}
}

func TestAPIIsRoutedOnlyWhenAsked(t *testing.T) {
	in, api, spec := routed(t, "work", config.Expose{API: true})
	enable(t, in, spec)
	js := toJSON(get(t, api, "IngressRoute", spec.Namespace, routeWeb)["spec"])
	if !strings.Contains(js, "PathPrefix(`/v1/`)") || strings.Contains(js, "/ui") {
		t.Errorf("api only: %s", js)
	}
}

func TestTokenLoginCanBeKept(t *testing.T) {
	in, api, spec := routed(t, "work", config.Expose{UI: true, TokenLogin: true})
	enable(t, in, spec)
	raw, _ := dig(get(t, api, "ConfigMap", spec.Namespace, "work-config"), "data", "work.yaml").(string)
	if !strings.Contains(raw, "token_login: true") {
		t.Error(raw)
	}
}

func TestDisableClosesTheRoutes(t *testing.T) {
	in, api, spec := routed(t, "work", config.Expose{UI: true})
	enable(t, in, spec)
	if err := in.Disable(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}
	if exists(api, "IngressRoute", spec.Namespace, routeWeb) || traefikPorts(t, api, spec.Namespace) != "[]" {
		t.Error("a disabled module is still routed")
	}
}

func TestFormsPublicRoute(t *testing.T) {
	in, api, spec := routed(t, "forms", config.Expose{})
	enable(t, in, spec)
	r := toJSON(get(t, api, "IngressRoute", spec.Namespace, "forms-public")["spec"])
	if !strings.Contains(r, "Host(`acme.example`) \\u0026\\u0026 PathPrefix(`/_forms`)") || !strings.Contains(r, `"priority":100`) || strings.Contains(r, "forms-api") {
		t.Errorf("route: %s", r)
	}
	if s := toJSON(get(t, api, "Middleware", spec.Namespace, "forms-strip")["spec"]); !strings.Contains(s, "/_forms") {
		t.Errorf("strip: %s", s)
	}
	if np := traefikPorts(t, api, spec.Namespace); !strings.Contains(np, `"port":8080`) || strings.Contains(np, "8081") {
		t.Errorf("only the public port: %s", np)
	}

	// the routing facts decide the hosts (sslip.io names, the plain-HTTP fallback)
	facts := routing.Config{Domain: "acme.example", CatchAll: true}
	cm := kube.Object{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": routing.ConfigMapName, "namespace": routing.Namespace}}
	data := map[string]any{}
	for k, v := range facts.Data() {
		data[k] = v
	}
	cm["data"] = data
	if err := api.Apply(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	enable(t, in, spec)
	r = toJSON(get(t, api, "IngressRoute", spec.Namespace, "forms-public")["spec"])
	if !strings.Contains(r, `"match":"PathPrefix(`+"`/_forms`"+`)"`) || !strings.Contains(r, `"entryPoints":["web"]`) || strings.Contains(r, "tls") {
		t.Errorf("plain HTTP catch-all: %s", r)
	}

	off := false
	spec.Expose = config.Expose{Public: &off}
	enable(t, in, spec)
	if exists(api, "IngressRoute", spec.Namespace, "forms-public") || traefikPorts(t, api, spec.Namespace) != "[]" {
		t.Error("public: false must remove the route")
	}
}

func TestPublicOffRemovesTheChartRoute(t *testing.T) {
	in, api, spec := routed(t, "jobs", config.Expose{})
	enable(t, in, spec)
	if !exists(api, "IngressRoute", spec.Namespace, "jobs") || !strings.Contains(traefikPorts(t, api, spec.Namespace), `"port":8080`) {
		t.Fatal("the careers route is on by default and reachable")
	}
	off := false
	spec.Expose = config.Expose{Public: &off}
	enable(t, in, spec)
	if exists(api, "IngressRoute", spec.Namespace, "jobs") {
		t.Error("careers route left behind")
	}
}

func TestPlainHTTPStackRoutesNoUI(t *testing.T) {
	in, api, spec := routed(t, "work", config.Expose{UI: true, API: true})
	if err := api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": routing.ConfigMapName, "namespace": routing.Namespace},
		"data":     map[string]any{"domain": "acme.example", "tls": "false", "siteHosts": "", "catchAll": "true"}}); err != nil {
		t.Fatal(err)
	}
	enable(t, in, spec)
	if exists(api, "IngressRoute", spec.Namespace, routeWeb) {
		t.Error("a UI must not be served over plain HTTP")
	}
	rt, err := in.Routing(context.Background(), spec)
	if err != nil || rt.UIHost != "" {
		t.Errorf("no Apps address either: %q %v", rt.UIHost, err)
	}
}

func TestRoutingRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*reconcile.Spec, *kube.Fake)
		want   string
	}{
		{"the API as a UI path", func(s *reconcile.Spec, _ *kube.Fake) { s.Manifest.Runtime.PublicUI.Prefixes = []string{"/v1"} }, "not a UI path"},
		{"everything as a UI path", func(s *reconcile.Spec, _ *kube.Fake) { s.Manifest.Runtime.PublicUI.Prefixes = []string{"/"} }, "not a UI path"},
		{"a service the chart lacks", func(s *reconcile.Spec, _ *kube.Fake) { s.Manifest.Runtime.PublicUI.Service = "can-core" }, "which the chart does not define"},
		{"a site path that is not its own", func(s *reconcile.Spec, _ *kube.Fake) {
			s.Manifest.Runtime.SiteRoute = &config.SiteRoute{Path: "/_forms", Service: "work", Port: 8080}
		}, "may not claim /_forms"},
		{"stale routing facts", func(_ *reconcile.Spec, api *kube.Fake) {
			_ = api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": routing.ConfigMapName, "namespace": routing.Namespace},
				"data":     map[string]any{"domain": "other.example", "tls": "true"}})
		}, "run `can apply`"},
	} {
		in, api, spec := routed(t, "work", config.Expose{UI: true})
		c.mutate(&spec, api)
		if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
		if exists(api, "IngressRoute", spec.Namespace, routeWeb) {
			t.Errorf("%s: a route was created", c.name)
		}
	}
}

func TestChartMayNotClaimAnotherHost(t *testing.T) {
	for _, sub := range []string{"admin", "work", "careers", "www"} {
		in, _, spec := routed(t, "jobs", config.Expose{})
		spec.Manifest.Runtime.Subdomain = sub
		want := sub == "careers" // jobs' own alias
		if got := in.hostFor(spec) != ""; got != want {
			t.Errorf("jobs claiming %s.<domain>: allowed=%v", sub, got)
		}
	}
}

func TestRoutesNeedTheAdmissionPolicy(t *testing.T) {
	in, api, spec := routed(t, "work", config.Expose{UI: true})
	in.cfg.RequireAdmission = true
	facts(t, api, map[string]any{"domain": "acme.example", "tls": "true", "siteHosts": "acme.example"})
	for _, n := range []string{"can-core-rolebindings", "can-core-namespaces"} {
		for _, o := range []kube.Object{
			{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy", "metadata": map[string]any{"name": n}},
			{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicyBinding", "metadata": map[string]any{"name": n}, "spec": map[string]any{"validationActions": []any{"Deny"}}},
		} {
			if err := api.Apply(context.Background(), o); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "can-core-routes") {
		t.Fatalf("without the route policy: %v", err)
	}
	if slices.ContainsFunc(api.Log, func(l string) bool { return strings.Contains(l, "mod-work") }) {
		t.Errorf("something was created: %v", api.Log)
	}
	// a module with nothing to route is not held back by it
	spec.Expose = config.Expose{}
	enable(t, in, spec)
}

func facts(t *testing.T, api *kube.Fake, data map[string]any) {
	t.Helper()
	if err := api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": routing.ConfigMapName, "namespace": routing.Namespace}, "data": data}); err != nil {
		t.Fatal(err)
	}
}

func TestPublicOffHoldsWhenTheRolloutFails(t *testing.T) {
	in, api, spec := routed(t, "jobs", config.Expose{})
	enable(t, in, spec)
	off := false
	spec.Expose = config.Expose{Public: &off}
	api.NotReady["jobs"] = true
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("the rollout should have failed")
	}
	if exists(api, "IngressRoute", spec.Namespace, "jobs") {
		t.Error("the careers route is still there (or the rollback put it back)")
	}
}

func TestAPIOnlyTrustsTheProxy(t *testing.T) {
	for _, id := range []string{"work", "jobs", "finance"} {
		in, api, spec := routed(t, id, config.Expose{API: true})
		enable(t, in, spec)
		raw, _ := dig(get(t, api, "ConfigMap", spec.Namespace, id+"-config"), "data", id+".yaml").(string)
		if !strings.Contains(raw, "trust_proxy: true") || strings.Contains(raw, "token_login") {
			t.Errorf("%s: %s", id, raw)
		}
	}
}

func TestBrokenRoutingFactsFailOnlyWhatNeedsThem(t *testing.T) {
	in, api, spec := routed(t, "work", config.Expose{})
	facts(t, api, map[string]any{"domain": "not a name"})
	if rt, err := in.Routing(context.Background(), spec); err != nil || rt.UIHost != "" || rt.Fingerprint == "" {
		t.Fatalf("%+v %v", rt, err)
	}
	enable(t, in, spec) // nothing to route
	spec.Expose = config.Expose{UI: true}
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("a routed module needs readable facts")
	}
}

func TestInAClusterTheFactsMustExist(t *testing.T) {
	in, _, spec := routed(t, "work", config.Expose{UI: true})
	in.cfg.RequireAdmission = true
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "run `can up`") {
		t.Fatalf("%v", err)
	}
}

// A cluster set up without `can up` has no facts: modules still install, unrouted.
func TestWithoutFactsModulesInstallUnrouted(t *testing.T) {
	for _, id := range []string{"forms", "jobs", "files"} {
		in, api, spec := routed(t, id, config.Expose{})
		in.cfg.RequireAdmission = true
		for _, n := range []string{"can-core-rolebindings", "can-core-namespaces"} {
			for _, o := range []kube.Object{
				{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy", "metadata": map[string]any{"name": n}},
				{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicyBinding", "metadata": map[string]any{"name": n}, "spec": map[string]any{"validationActions": []any{"Deny"}}},
			} {
				if err := api.Apply(context.Background(), o); err != nil {
					t.Fatal(err)
				}
			}
		}
		enable(t, in, spec)
		for k := range api.Objects {
			if strings.HasPrefix(k, "IngressRoute/") {
				t.Errorf("%s: %s without routing facts", id, k)
			}
		}
		if got := traefikPorts(t, api, spec.Namespace); got != "[]" {
			t.Errorf("%s: %s", id, got)
		}
	}
}
