package launch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/routing"
)

// Public access (specs/045-public-access): the redirect and the edge limits of `can up`'s own routes.

func TestRedirectIsTemporaryAndOnlyForPageLoads(t *testing.T) {
	b, err := RedirectManifests()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"redirectScheme", "permanent: false", "scheme: https", "- web\n", "Method(`GET`) || Method(`HEAD`)", "priority: 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	// it must not touch TLS or certificate issuance
	for _, never := range []string{"websecure", "certResolver", "tls:"} {
		if strings.Contains(s, never) {
			t.Errorf("%q in the redirect:\n%s", never, s)
		}
	}
}

func TestRedirectFollowsTheMode(t *testing.T) {
	for _, tls := range []bool{true, false} {
		k := &fakeKube{}
		r := &runner{o: Options{Routes: RouteOptions{TLS: tls, CatchAll: true}}, d: Deps{Kube: k}, res: &Result{}}
		if err := r.redirect(context.Background()); err != nil {
			t.Fatal(err)
		}
		applied, deleted := len(k.called("apply")), len(k.called("delete"))
		if tls && (applied != 1 || deleted != 0) {
			t.Errorf("TLS: applied %d deleted %d", applied, deleted)
		}
		// the plain-HTTP fallback serves the site on `web`: a redirect left from an earlier run goes
		if !tls && (applied != 0 || deleted != 2 || len(k.called("delete ingressroutes.traefik.io https-redirect --ignore-not-found")) != 1) {
			t.Errorf("plain HTTP: applied %d deleted %d", applied, deleted)
		}
	}
}

func TestSiteRoutesLimitSignInAndThePublicSite(t *testing.T) {
	o := RouteOptions{SiteHosts: []string{"acme.example"}, AdminHosts: []string{"admin.acme.example"}, TLS: true}
	b, err := SiteRoutes(o)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := parseDocs(b)
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string][]string{}
	for _, d := range docs {
		if d["kind"] != "IngressRoute" {
			continue
		}
		for _, r := range d["spec"].(map[string]any)["routes"].([]any) {
			rm := r.(map[string]any)
			var names []string
			mws, _ := rm["middlewares"].([]any)
			for _, m := range mws {
				names = append(names, m.(map[string]any)["name"].(string))
			}
			rules[rm["match"].(string)] = names
		}
	}
	login := "Host(`admin.acme.example`) && Path(`/login`)"
	if got := strings.Join(rules[login], ","); got != "can-login-rate,can-login-inflight" {
		t.Errorf("%s: %q", login, got)
	}
	if len(rules["Host(`admin.acme.example`) && Path(`/api/v1/login`)"]) != 2 {
		t.Errorf("the API login is not limited: %v", rules)
	}
	if got := rules["Host(`admin.acme.example`)"]; len(got) != 0 {
		t.Errorf("the signed-in admin UI and API must not be limited: %v", got)
	}
	if got := strings.Join(rules["Host(`acme.example`)"], ","); got != "can-public-rate,can-public-inflight" {
		t.Errorf("site: %q", got)
	}
	// `can apply` still reads the routing facts off the site route
	for _, d := range docs {
		if md, _ := d["metadata"].(map[string]any); d["kind"] == "IngressRoute" && md["name"] == "site" {
			site, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if c, err := routing.FromSiteRoute("acme.example", site); err != nil || !c.TLS || len(c.SiteHosts) != 1 {
				t.Errorf("%+v %v", c, err)
			}
		}
	}

	o.NoEdgeLimit = true
	off, err := SiteRoutes(o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(off), "rateLimit") || strings.Contains(string(off), "middlewares") || strings.Contains(string(off), "/login") {
		t.Errorf("limits left on:\n%s", off)
	}
}
