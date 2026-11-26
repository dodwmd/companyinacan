//go:build integration

package integration

// Epic E03f (specs/045-public-access) on a real cluster, run by TestModuleRoutingThroughTheIngress
// with its signed-in admin and work sessions: the Middlewares as Traefik applies them (a spec it
// rejects takes the router down), the HTTPS redirect `can up` applies, the sign-in limit with
// spoofed forwarding headers, no limit on signed-in traffic and finance's signed-link path.
//
// NOT covered: the headers on the careers pages (jobs is not installed by this test; the installer
// tests cover the route), ACME with the redirect in place (no public name), the client address as ServiceLB
// delivers it on a real server (k3d's load balancer hides it), a real invoice link.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/launch"
)

func publicCompany() string {
	return fmt.Sprintf("schema: 1\nname: Route Co\ndomain: %s\nprovider:\n  type: local\nmodules:\n  work:\n    expose: {ui: true}\n  forms: {}\n  finance:\n    expose: {public: true}\n", routeDomain)
}

func publicAccessThroughTheIngress(t *testing.T, browser *http.Client, admin string, adminHdr map[string]string) {
	t.Helper()
	work, site := "https://work."+routeDomain, "https://"+routeDomain

	// 1. plain HTTP: page loads go to HTTPS, anything carrying a body does not.
	redirect, err := launch.RedirectManifests()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := kubectlStdin(string(redirect), "apply", "-f", "-"); err != nil {
		t.Fatalf("redirect: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl("-n", "can-system", "delete", "ingressroutes.traefik.io", "https-redirect", "--ignore-not-found")
		_, _ = kubectl("-n", "can-system", "delete", "middlewares.traefik.io", "can-https-redirect", "--ignore-not-found")
	})
	for _, host := range []string{routeDomain, "work." + routeDomain, "admin." + routeDomain} {
		eventually(t, 60*time.Second, "http://"+host+" to redirect", func() (bool, string) {
			st, _, h, err := fetch(browser, "GET", "http://"+host+"/some/page?q=1", nil, "")
			return err == nil && (st == 302 || st == 307) && h.Get("Location") == "https://"+host+"/some/page?q=1",
				fmt.Sprint(st, err, h.Get("Location"))
		})
	}
	if st, _, h := mustFetch(t, browser, "POST", "http://admin."+routeDomain+"/api/v1/login", nil, "{}"); st != 404 {
		t.Errorf("a POST over plain HTTP must fail, not be redirected: %d %s", st, h.Get("Location"))
	}
	if st, _, _ := mustFetch(t, browser, "GET", site+"/", nil, ""); st != 200 && st != 404 {
		t.Errorf("https is affected by the redirect: %d", st)
	}

	// 2. finance's signed links.
	canApplyModules(t, publicCompany())
	eventually(t, 300*time.Second, "finance to be rolled out", func() (bool, string) {
		o, err := kubectl("-n", "mod-finance", "rollout", "status", "deploy/finance", "--timeout=5s")
		return err == nil, o
	})
	finance := "https://finance." + routeDomain
	var h http.Header
	eventually(t, 120*time.Second, "the signed-link path through the ingress", func() (bool, string) {
		st, b, hd, err := fetch(browser, "GET", finance+"/public/invoices/not-a-link", nil, "")
		h = hd
		// finance's own JSON 404, not Traefik's: the request reached the module
		return err == nil && st == 404 && strings.Contains(b, "not_found"), fmt.Sprint(st, err, b)
	})
	for k, want := range map[string]string{"X-Robots-Tag": "noindex", "Strict-Transport-Security": "max-age=31536000", "X-Content-Type-Options": "nosniff",
		"X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
		if got := h.Get(k); !strings.Contains(got, want) {
			t.Errorf("link header %s = %q, want %q", k, got, want)
		}
	}
	for _, c := range [][2]string{{"GET", "/"}, {"GET", "/v1/invoices"}, {"GET", "/healthz"}, {"GET", "/login"}, {"POST", "/public/invoices/not-a-link"},
		{"GET", "/public/invoices/../../v1/me"}} {
		st, b, _ := mustFetch(t, browser, c[0], finance+c[1], nil, "")
		if st == 200 || strings.Contains(b, `"code"`) {
			t.Errorf("%s %s reached finance: %d %s", c[0], c[1], st, b)
		}
	}
	// 3. signed-in traffic is not limited at the edge, however busy.
	for i := range 80 {
		if st, b, _ := mustFetch(t, browser, "GET", work+"/ui/", nil, ""); st != 200 {
			t.Fatalf("signed-in page %d: %d %s", i, st, b)
		}
	}
	for i := range 40 {
		if st, b, _ := mustFetch(t, browser, "GET", admin+"/api/v1/apps", adminHdr, ""); st != 200 {
			t.Fatalf("admin API request %d: %d %s", i, st, b)
		}
	}

	// 4. sign-in is limited, and a client cannot buy a new allowance with forwarding headers.
	for _, u := range []string{work + "/ui/login", admin + "/login"} {
		limited := 0
		for i := range 90 {
			spoof := fmt.Sprintf("203.0.113.%d", i+1)
			st, _, _ := mustFetch(t, browser, "GET", u, map[string]string{"X-Forwarded-For": spoof, "X-Real-Ip": spoof}, "")
			if st == http.StatusTooManyRequests {
				limited++
			}
		}
		if limited < 20 {
			t.Errorf("%s: only %d of 90 requests with spoofed addresses were limited", u, limited)
		}
	}
	if st, _, _ := mustFetch(t, browser, "GET", work+"/ui/", nil, ""); st != 200 {
		t.Errorf("a signed-in session is caught by the sign-in limit: %d", st)
	}
}
