package installer

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

// Public access (specs/045-public-access): edge limits, the signed-link route and the headers on
// chart-made routes, against the real charts and manifests. Traefik's behaviour is checked by
// test/integration/z_routing_k3d_test.go.

// routeRules returns the rules of an IngressRoute as match -> middleware names (comma separated)
// and match -> priority.
func routeRules(t *testing.T, api *kube.Fake, ns, name string) (map[string]string, map[string]string) {
	t.Helper()
	mws, prio := map[string]string{}, map[string]string{}
	for _, r := range asList(dig(get(t, api, "IngressRoute", ns, name), "spec", "routes")) {
		rm := asMap(r)
		match, _ := rm["match"].(string)
		if _, dup := mws[match]; dup {
			t.Errorf("%s: rule %s twice", name, match)
		}
		var names []string
		for _, m := range asList(rm["middlewares"]) {
			n, _ := asMap(m)["name"].(string)
			names = append(names, n)
		}
		mws[match] = strings.Join(names, ",")
		prio[match] = toJSON(rm["priority"])
	}
	return mws, prio
}

const loginLimits = "can-web-headers,can-login-rate,can-login-inflight"

func TestSignInPathsAreLimitedAndNothingElse(t *testing.T) {
	for id, login := range map[string]string{"work": "/ui/login", "people": "/login", "jobs": "/login", "finance": "/login"} {
		t.Run(id, func(t *testing.T) {
			in, api, spec := routed(t, id, config.Expose{UI: true, API: true})
			enable(t, in, spec)
			mws, prio := routeRules(t, api, spec.Namespace, routeWeb)
			h := "Host(`" + id + ".acme.example`) && "
			for _, m := range []string{h + "Path(`" + login + "`)", h + "PathPrefix(`" + login + "/`)"} {
				if mws[m] != loginLimits || prio[m] != "1000" {
					t.Errorf("%s: middlewares %q priority %s", m, mws[m], prio[m])
				}
			}
			for m, got := range mws {
				if !strings.Contains(m, "`"+login+"`") && !strings.Contains(m, "`"+login+"/`") && got != "can-web-headers" {
					t.Errorf("%s is limited at the edge (authenticated traffic must not be): %s", m, got)
				}
			}
			for _, n := range []string{"can-login-rate", "can-login-inflight"} {
				s := toJSON(get(t, api, "Middleware", spec.Namespace, n)["spec"])
				// by source address; never a header the client writes, never one bucket per Host
				if !strings.Contains(s, `"sourceCriterion":{"ipStrategy":{"depth":0}}`) || strings.Contains(s, "requestHeaderName") || strings.Contains(s, "requestHost") {
					t.Errorf("%s: %s", n, s)
				}
			}
		})
	}
}

func TestEdgeLimitsCanBeSwitchedOff(t *testing.T) {
	off := false
	for name, setup := range map[string]func(*testing.T) (*Installer, *kube.Fake, string){
		"expose.edge_limit: false": func(t *testing.T) (*Installer, *kube.Fake, string) {
			in, api, spec := routed(t, "work", config.Expose{UI: true, EdgeLimit: &off})
			enable(t, in, spec)
			return in, api, spec.Namespace
		},
		"a proxy in front (routing facts)": func(t *testing.T) (*Installer, *kube.Fake, string) {
			in, api, spec := routed(t, "work", config.Expose{UI: true})
			enable(t, in, spec) // with limits first: switching off must remove them
			facts(t, api, map[string]any{"domain": "acme.example", "tls": "true", "siteHosts": "acme.example", "edgeLimit": "off"})
			enable(t, in, spec)
			return in, api, spec.Namespace
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, api, ns := setup(t)
			if s := toJSON(get(t, api, "IngressRoute", ns, routeWeb)["spec"]); strings.Contains(s, "rate") || strings.Contains(s, "inflight") {
				t.Errorf("still limited: %s", s)
			}
			if exists(api, "Middleware", ns, "can-login-rate") || exists(api, "Middleware", ns, "can-login-inflight") {
				t.Error("limit left behind")
			}
		})
	}
}

func TestFormsRouteIsLimited(t *testing.T) {
	in, api, spec := routed(t, "forms", config.Expose{})
	enable(t, in, spec)
	mws, _ := routeRules(t, api, spec.Namespace, "forms-public")
	for m, got := range mws {
		if got != "forms-strip,can-site-headers,can-public-rate,can-public-inflight" {
			t.Errorf("%s: %s", m, got)
		}
	}
}

func TestInvoiceLinksAreOptIn(t *testing.T) {
	in, api, spec := routed(t, "finance", config.Expose{})
	enable(t, in, spec)
	if exists(api, "IngressRoute", spec.Namespace, routeLink) || traefikPorts(t, api, spec.Namespace) != "[]" {
		t.Fatal("finance publishes nothing unless company.yaml says public: true")
	}

	on, off := true, false
	spec.Expose = config.Expose{Public: &on}
	enable(t, in, spec)
	mws, _ := routeRules(t, api, spec.Namespace, routeLink)
	want := map[string]bool{}
	for _, method := range []string{"GET", "HEAD"} {
		want["Host(`finance.acme.example`) && PathPrefix(`/public/invoices/`) && Method(`"+method+"`)"] = true
	}
	if len(mws) != len(want) {
		t.Fatalf("rules: %v", mws)
	}
	for m, got := range mws {
		if !want[m] || got != "can-link-headers,can-public-rate,can-public-inflight" {
			t.Errorf("%s: %s", m, got)
		}
	}
	r := get(t, api, "IngressRoute", spec.Namespace, routeLink)
	if toJSON(dig(r, "spec", "entryPoints")) != `["websecure"]` || dig(r, "spec", "tls", "certResolver") != "letsencrypt" {
		t.Errorf("a signed link must only travel over TLS: %s", toJSON(r["spec"]))
	}
	h := toJSON(get(t, api, "Middleware", spec.Namespace, middlewareLink)["spec"])
	for _, w := range []string{"stsSeconds", "contentTypeNosniff", "frameDeny", "no-referrer", "noindex, nofollow", "no-store"} {
		if !strings.Contains(h, w) {
			t.Errorf("headers lack %s: %s", w, h)
		}
	}
	cfg, _ := dig(get(t, api, "ConfigMap", spec.Namespace, "finance-config"), "data", "finance.yaml").(string)
	for _, w := range []string{"public_base_url: https://finance.acme.example", "trust_proxy: true"} {
		if !strings.Contains(cfg, w) {
			t.Errorf("config lacks %q:\n%s", w, cfg)
		}
	}
	for _, m := range inhouseModules() {
		if m.id == "finance" {
			if err := m.parseConf([]byte(cfg)); err != nil {
				t.Errorf("can-finance does not accept the config: %v", err)
			}
		}
	}
	if !strings.Contains(traefikPorts(t, api, spec.Namespace), `"port":8080`) {
		t.Error("the ingress controller is not admitted")
	}

	spec.Expose = config.Expose{Public: &off}
	enable(t, in, spec)
	if exists(api, "IngressRoute", spec.Namespace, routeLink) || exists(api, "Middleware", spec.Namespace, middlewareLink) || traefikPorts(t, api, spec.Namespace) != "[]" {
		t.Error("public: false left the link route behind")
	}
}

func TestInvoiceLinksNeverOverPlainHTTP(t *testing.T) {
	on := true
	in, api, spec := routed(t, "finance", config.Expose{Public: &on})
	facts(t, api, map[string]any{"domain": "acme.example", "tls": "false", "catchAll": "true"})
	enable(t, in, spec)
	if exists(api, "IngressRoute", spec.Namespace, routeLink) {
		t.Error("an invoice link route over plain HTTP")
	}
}

func TestAModuleMayPublishOnlyItsListedLinkPath(t *testing.T) {
	on := true
	in, api, spec := routed(t, "finance", config.Expose{Public: &on})
	spec.Manifest.Runtime.PublicLink.Prefix = "/v1/"
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "may not publish") {
		t.Fatalf("%v", err)
	}
	if exists(api, "IngressRoute", spec.Namespace, routeLink) {
		t.Error("route created")
	}
}

func TestCareersRoutesGetHeadersLimitsAndTheClientAddress(t *testing.T) {
	in, api, spec := routed(t, "jobs", config.Expose{})
	enable(t, in, spec)
	mws, _ := routeRules(t, api, spec.Namespace, "jobs")
	if len(mws) == 0 {
		t.Fatal("no careers rules")
	}
	for m, got := range mws {
		if got != "can-chart-headers,can-public-rate,can-public-inflight" {
			t.Errorf("%s: %s", m, got)
		}
	}
	h := toJSON(get(t, api, "Middleware", spec.Namespace, middlewareChart)["spec"])
	for _, w := range []string{"stsSeconds", "contentTypeNosniff", "frameDeny"} {
		if !strings.Contains(h, w) {
			t.Errorf("headers lack %s: %s", w, h)
		}
	}
	cfg, _ := dig(get(t, api, "ConfigMap", spec.Namespace, "jobs-config"), "data", "jobs.yaml").(string)
	if !strings.Contains(cfg, "trust_proxy: true") {
		t.Errorf("all applicants would share one allowance:\n%s", cfg)
	}
	// the middlewares are there before the route that names them
	mw, route := -1, -1
	for i, l := range api.Log {
		if mw < 0 && strings.Contains(l, middlewareChart) {
			mw = i
		}
		if route < 0 && strings.Contains(l, "IngressRoute") && strings.HasSuffix(l, "/jobs") {
			route = i
		}
	}
	if mw < 0 || route < 0 || mw > route {
		t.Errorf("order: middleware at %d, route at %d: %v", mw, route, api.Log)
	}

	off := false
	spec.Expose = config.Expose{Public: &off}
	enable(t, in, spec)
	if exists(api, "Middleware", spec.Namespace, middlewareChart) || exists(api, "Middleware", spec.Namespace, "can-public-rate") {
		t.Error("middlewares left behind")
	}
}

func TestAFailedUpgradeKeepsTheMiddlewaresTheRunningRouteNames(t *testing.T) {
	in, api, spec := routed(t, "jobs", config.Expose{})
	enable(t, in, spec)
	off := false
	spec.Expose = config.Expose{EdgeLimit: &off}
	api.NotReady["jobs"] = true
	if err := in.Enable(context.Background(), spec); err == nil {
		t.Fatal("the rollout should have failed")
	}
	mws, _ := routeRules(t, api, spec.Namespace, "jobs")
	for _, names := range mws {
		for _, n := range strings.Split(names, ",") {
			if !exists(api, "Middleware", spec.Namespace, n) {
				t.Errorf("the restored route names %s, which is gone", n)
			}
		}
	}
}

func TestAuthenticatedChartEndpointsAreNotLimited(t *testing.T) {
	in, api, spec := routed(t, "provisioning", config.Expose{})
	enable(t, in, spec)
	mws, _ := routeRules(t, api, spec.Namespace, "provisioning")
	for m, got := range mws {
		if got != "can-chart-headers" {
			t.Errorf("%s: %s (SCIM is authenticated API traffic)", m, got)
		}
	}
}
