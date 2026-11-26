package installer

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// The signing pages (specs/049-contract-esign) are routed only when asked for: one path, with POST.
func TestSigningRouteIsOffByDefault(t *testing.T) {
	on := true
	for name, x := range map[string]config.Expose{"nothing": {}, "ui": {UI: true}, "invoice links": {Public: &on}} {
		in, api, spec := routed(t, "finance", x)
		enable(t, in, spec)
		if exists(api, "IngressRoute", spec.Namespace, routeSign) {
			t.Errorf("%s: a signing route", name)
		}
	}
}

func TestSigningRoute(t *testing.T) {
	in, api, spec := routed(t, "finance", config.Expose{Signing: true})
	enable(t, in, spec)
	mws, _ := routeRules(t, api, spec.Namespace, routeSign)
	const h = "Host(`finance.acme.example`) && PathPrefix(`/public/sign/`) && Method(`"
	if len(mws) != 3 {
		t.Fatalf("%v", mws)
	}
	for _, m := range []string{"GET", "HEAD", "POST"} {
		if got := mws[h+m+"`)"]; got != "can-sign-headers,can-public-rate,can-public-inflight" {
			t.Errorf("%s: %q", m, got)
		}
	}
	hdr := toJSON(get(t, api, "Middleware", spec.Namespace, middlewareSign)["spec"])
	for _, w := range []string{`"referrerPolicy":"no-referrer"`, `"frameDeny":true`, "stsSeconds", "noindex"} {
		if !strings.Contains(hdr, w) {
			t.Errorf("headers lack %s: %s", w, hdr)
		}
	}
	if exists(api, "IngressRoute", spec.Namespace, routeWeb) || exists(api, "IngressRoute", spec.Namespace, routeLink) {
		t.Error("signing published more than the signing path")
	}
	cm := toJSON(get(t, api, "ConfigMap", spec.Namespace, "finance-config")["data"])
	for _, w := range []string{"sign_base_url: https://finance.acme.example", "trust_proxy: true"} {
		if !strings.Contains(cm, w) {
			t.Errorf("config lacks %q: %s", w, cm)
		}
	}
	// Switching it off takes the route and the setting away.
	spec.Expose = config.Expose{}
	enable(t, in, spec)
	if exists(api, "IngressRoute", spec.Namespace, routeSign) || exists(api, "Middleware", spec.Namespace, middlewareSign) {
		t.Error("the signing route stayed")
	}
	if cm := toJSON(get(t, api, "ConfigMap", spec.Namespace, "finance-config")["data"]); strings.Contains(cm, "sign_base_url") {
		t.Error("sign_base_url stayed")
	}
}

func TestSigningNeverOverPlainHTTP(t *testing.T) {
	in, api, spec := routed(t, "finance", config.Expose{Signing: true})
	facts(t, api, map[string]any{"domain": "acme.example", "tls": "false", "catchAll": "true"})
	enable(t, in, spec)
	if exists(api, "IngressRoute", spec.Namespace, routeSign) {
		t.Error("a signing route over plain HTTP")
	}
}
