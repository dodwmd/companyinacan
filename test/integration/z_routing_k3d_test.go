//go:build integration

package integration

// Epic E03e (specs/040-module-routing) on a real cluster.
//
// TestAdmissionPolicyRoutes needs only deploy/base (and Traefik's CRDs): with server-side dry runs
// as can-core's ServiceAccount it shows which routes the API server refuses.
//
// TestModuleRoutingThroughTheIngress (CAN_IT_ROUTING=1, set by the dev-cluster workflow, which
// builds can-work and can-forms) enables work with its UI and forms from company.yaml and then uses
// only the ingress, on port 443 with Traefik's default certificate: the work UI, an admin login, the
// Apps launcher, a launch ticket redeemed into a work session, a public form post that becomes a
// lead, and the paths and hosts that must not answer. The admin and site routes are the ones
// `can up` would apply.
//
// NOT covered: a real certificate (ACME), a real browser, people, jobs and finance.

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/launch"
	"github.com/dodwmd/companyinacan/core/routing"
	"github.com/dodwmd/companyinacan/core/server/auth"
)

const routeDomain = "route.example"

func moduleNamespaceYAML(id string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: mod-%[1]s
  labels:
    can.dev/namespace-kind: module
    can.dev/module: %[1]s
    app.kubernetes.io/managed-by: can-core
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/enforce-version: v1.31
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: can-core-module
  namespace: mod-%[1]s
  labels: {app.kubernetes.io/managed-by: can-core, can.dev/module: %[1]s}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: can-core-module}
subjects: [{kind: ServiceAccount, name: can-core, namespace: can-system}]
`, id)
}

func routeYAML(ns, match, extra string) string {
	return fmt.Sprintf(`apiVersion: traefik.io/v1alpha1
kind: IngressRoute
metadata: {name: vap-probe, namespace: %s}
spec:
  entryPoints: [websecure]
  routes:
    - match: %q
      kind: Rule
      services: [{name: x, port: 8080}]
%s`, ns, match, extra)
}

func applyRoutingFacts(t *testing.T, c routing.Config) {
	t.Helper()
	m, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := kubectlStdin(string(m), "apply", "-f", "-"); err != nil {
		t.Fatalf("can-routing: %v\n%s", err, out)
	}
}

func TestAdmissionPolicyRoutes(t *testing.T) {
	mustKubectl(t, "get", "validatingadmissionpolicy", "can-core-routes", "can-core-routing-config")
	mustKubectl(t, "get", "validatingadmissionpolicybinding", "can-core-routes", "can-core-routing-config")
	for _, id := range []string{"work", "forms", "jobs"} {
		if out, err := kubectlStdin(moduleNamespaceYAML(id), "apply", "-f", "-"); err != nil {
			t.Fatalf("namespace mod-%s: %v\n%s", id, err, out)
		}
	}
	applyRoutingFacts(t, routing.Default(routeDomain))

	create := func(yaml string) (string, error) {
		return asCanCoreStdin(yaml, "create", "--dry-run=server", "-f", "-")
	}
	host := func(h string) string { return "Host(`" + h + "`)" }
	// the policy takes a moment to be enforced
	eventually(t, 180*time.Second, "the route policy to be enforced", func() (bool, string) {
		out, err := create(routeYAML("mod-work", host("admin."+routeDomain), ""))
		return err != nil && strings.Contains(out, "only its own host"), out
	})

	allowed := []struct{ name, ns, match string }{
		{"work's own host", "mod-work", host("work."+routeDomain) + " && PathPrefix(`/ui/`)"},
		{"work's root", "mod-work", host("work."+routeDomain) + " && Path(`/`)"},
		{"jobs' alias", "mod-jobs", host("careers."+routeDomain) + " && PathPrefix(`/careers`)"},
		{"jobs' own host", "mod-jobs", host("jobs."+routeDomain) + " && PathPrefix(`/login`)"},
		{"forms on the site", "mod-forms", host(routeDomain) + " && PathPrefix(`/_forms`)"},
	}
	for _, c := range allowed {
		eventually(t, 30*time.Second, c.name+" to be allowed", func() (bool, string) {
			out, err := create(routeYAML(c.ns, c.match, ""))
			return err == nil, out
		})
	}

	denied := []struct{ name, ns, match, extra string }{
		{"the admin host", "mod-work", host("admin." + routeDomain), ""},
		{"another module's host", "mod-work", host("finance." + routeDomain), ""},
		{"another module's alias", "mod-work", host("careers." + routeDomain), ""},
		{"the apex", "mod-work", host(routeDomain), ""},
		{"the apex forms path from another module", "mod-work", host(routeDomain) + " && PathPrefix(`/_forms`)", ""},
		{"the apex outside /_forms", "mod-forms", host(routeDomain) + " && PathPrefix(`/`)", ""},
		{"a catch-all", "mod-forms", "PathPrefix(`/_forms`)", ""},
		{"no host", "mod-work", "PathPrefix(`/ui/`)", ""},
		{"a disjunction", "mod-work", host("work."+routeDomain) + " || " + host("admin."+routeDomain), ""},
		{"two hosts", "mod-work", host("work."+routeDomain) + " && " + host("admin."+routeDomain), ""},
		{"a host regexp", "mod-work", "HostRegexp(`.*`)", ""},
		{"another domain", "mod-work", host("work.attacker.example"), ""},
		{"a chosen certificate", "mod-work", host("work." + routeDomain), "  tls: {secretName: other}\n"},
		{"certificates for other names", "mod-work", host("work." + routeDomain), "  tls: {certResolver: letsencrypt, domains: [{main: admin.route.example}]}\n"},
		{"outside a module namespace", "can-system", host("work." + routeDomain), ""},
	}
	for _, c := range denied {
		out, err := create(routeYAML(c.ns, c.match, c.extra))
		if err == nil || !strings.Contains(out, "ValidatingAdmissionPolicy") {
			t.Errorf("%s: want a denial by the admission policy, got err=%v %s", c.name, err, out)
		}
	}
	other := strings.Replace(routeYAML("mod-work", host("work."+routeDomain), ""), "{name: x, port: 8080}", "{name: can-core, namespace: can-system, port: 8080}", 1)
	if out, err := create(other); err == nil {
		t.Errorf("a route to another namespace's Service was allowed: %s", out)
	}

	// Without the routing facts only module routes are held back; the rest of the cluster (k3s's
	// own Traefik chart creates a route) is not.
	mustKubectl(t, "-n", "can-system", "delete", "configmap", "can-routing")
	eventually(t, 60*time.Second, "a module route to be refused without can-routing", func() (bool, string) {
		out, err := create(routeYAML("mod-work", host("work."+routeDomain), ""))
		return err != nil, out
	})
	if out, err := kubectlStdin(routeYAML("kube-system", host("anything.example"), ""), "create", "--dry-run=server", "-f", "-"); err != nil {
		t.Errorf("a route outside module namespaces is blocked while can-routing is missing: %s", out)
	}
	applyRoutingFacts(t, routing.Default(routeDomain))

	// can-core cannot widen its own bounds.
	m, err := routing.Default("attacker.example").Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := asCanCoreStdin(string(m), "apply", "--dry-run=server", "-f", "-"); err == nil || !strings.Contains(out, "may not write ConfigMap") {
		t.Errorf("can-core may rewrite can-routing: err=%v %s", err, out)
	}
	if out, err := asCanCore("-n", "can-system", "delete", "configmap", "can-routing", "--dry-run=server"); err == nil {
		t.Errorf("can-core may delete can-routing: %s", out)
	}
	if out, err := asCanCore("-n", "mod-work", "create", "ingress", "vap-probe", "--rule=admin."+routeDomain+"/*=x:80", "--dry-run=server"); err == nil || !strings.Contains(out, "plain Ingress") {
		t.Errorf("can-core may create a plain Ingress: err=%v %s", err, out)
	}
}

// ---- through the ingress ------------------------------------------------------------------------

// ingressClient talks to Traefik's websecure entry point whatever the host name, like a browser
// whose DNS points at the server. The certificate is Traefik's self-signed default: there is no
// public name to get a real one for.
func ingressClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar:           jar,
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // default certificate of a throwaway cluster
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+port)
			},
		},
	}
}

func fetch(c *http.Client, method, u string, hdr map[string]string, body string) (int, string, http.Header, error) {
	req, err := http.NewRequest(method, u, strings.NewReader(body)) //nolint:noctx // test helper
	if err != nil {
		return 0, "", nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req) //nolint:gosec // loopback
	if err != nil {
		return 0, "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b), resp.Header, nil
}

func mustFetch(t *testing.T, c *http.Client, method, u string, hdr map[string]string, body string) (int, string, http.Header) {
	t.Helper()
	st, b, h, err := fetch(c, method, u, hdr, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	return st, b, h
}

func routingCompany(workUI bool) string {
	return fmt.Sprintf("schema: 1\nname: Route Co\ndomain: %s\nprovider:\n  type: local\nmodules:\n  work:\n    expose: {ui: %v}\n  forms: {}\n", routeDomain, workUI)
}

var hiddenInput = regexp.MustCompile(`name="(csrf|ticket)" value="([^"]+)"`)

func TestModuleRoutingThroughTheIngress(t *testing.T) {
	if os.Getenv("CAN_IT_CANCORE") != "1" || os.Getenv("CAN_IT_ROUTING") != "1" {
		t.Skip("CAN_IT_CANCORE=1 and CAN_IT_ROUTING=1 are not both set: can-core, can-work and can-forms are not in this cluster")
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, args := range [][]string{
			{"-n", "can-system", "logs", "deploy/can-core", "--tail=120"},
			{"get", "ingressroutes.traefik.io,middlewares.traefik.io,networkpolicy", "-A", "-o", "yaml"},
			{"-n", "mod-work", "logs", "deploy/work", "--tail=60"},
			{"-n", "mod-forms", "logs", "deploy/forms", "--tail=60"},
			{"-n", "mod-forms", "get", "all", "-o", "wide"},
			{"-n", "kube-system", "logs", "deploy/traefik", "--tail=60"},
		} {
			out, _ := kubectl(args...)
			t.Logf("kubectl %v:\n%s", args, out)
		}
	})

	// The launch cookie and the Apps addresses need the company domain; `can up` sets it.
	mustKubectl(t, "-n", "can-system", "set", "env", "deploy/can-core", "CAN_COMPANY_DOMAIN="+routeDomain)
	t.Cleanup(func() { _, _ = kubectl("-n", "can-system", "set", "env", "deploy/can-core", "CAN_COMPANY_DOMAIN-") })
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")
	admin, err := launch.SiteRoutes(launch.RouteOptions{SiteHosts: []string{routeDomain}, AdminHosts: []string{"admin." + routeDomain}, TLS: true})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := kubectlStdin(string(admin), "apply", "-f", "-"); err != nil {
		t.Fatalf("admin route: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl("-n", "can-system", "delete", "ingressroutes.traefik.io", "site", "admin", "--ignore-not-found")
	})

	const password = "ci-Passw0rd-not-a-secret-9f31"
	user := fmt.Sprintf("route%d", time.Now().Unix())
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

	// 1. enable work with its UI, and forms. `can apply` also writes the routing facts.
	canApplyModules(t, routingCompany(true))
	for _, mod := range []string{"work", "forms"} {
		eventually(t, 300*time.Second, mod+" to be rolled out", func() (bool, string) {
			o, err := kubectl("-n", "mod-"+mod, "rollout", "status", "deploy/"+mod, "--timeout=5s")
			return err == nil, o
		})
	}
	eventually(t, 120*time.Second, "can-core to create the routes", func() (bool, string) {
		a, err1 := kubectl("-n", "mod-work", "get", "ingressroutes.traefik.io", "can-web", "-o", "name")
		b, err2 := kubectl("-n", "mod-forms", "get", "ingressroutes.traefik.io", "forms-public", "-o", "name")
		return err1 == nil && err2 == nil, a + "\n" + b
	})
	if got := mustKubectl(t, "-n", "mod-work", "get", "networkpolicy", "allow-ingress-from-traefik", "-o", "jsonpath={.spec.ingress[*].ports[*].port}"); got != "8080" {
		t.Errorf("the ingress controller may reach ports %q of work, want 8080", got)
	}

	// 2. the UI answers on work.<domain>, over TLS only, with security headers; the API does not.
	browser := ingressClient(t)
	work := "https://work." + routeDomain
	eventually(t, 120*time.Second, "the work UI through the ingress", func() (bool, string) {
		st, b, _, err := fetch(browser, "GET", work+"/ui/login", nil, "")
		return err == nil && st == 200, fmt.Sprint(st, err, b)
	})
	_, body, h := mustFetch(t, browser, "GET", work+"/ui/login", nil, "")
	if strings.Contains(body, `name="token"`) {
		t.Error("a routed UI must not offer the paste-a-token form unless expose.token_login is set")
	}
	for k, want := range map[string]string{"Strict-Transport-Security": "max-age=31536000", "X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY"} {
		if got := h.Get(k); !strings.Contains(got, want) {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	for _, p := range []string{"/v1/projects", "/v1/me", "/healthz", "/readyz", "/metrics"} {
		if st, b, _ := mustFetch(t, browser, "GET", work+p, nil, ""); st != 404 {
			t.Errorf("%s must not be routed: %d %s", p, st, b)
		}
	}
	if st, _, _, err := fetch(edgeClient(), "GET", "http://work."+routeDomain+"/ui/login", nil, ""); err == nil && st == 200 {
		t.Error("the UI answers over plain HTTP")
	}

	// 3. the admin UI: log in, find work under Apps (nothing was set by hand), launch.
	base := "https://admin." + routeDomain
	code := auth.Code(key, auth.SHA1, 6, 30, time.Now())
	var st int
	var csrf string
	eventually(t, 60*time.Second, "the admin login through the ingress", func() (bool, string) {
		var resp string
		st, resp, _, err = fetch(browser, "POST", base+"/api/v1/login", map[string]string{"Content-Type": "application/json"},
			fmt.Sprintf(`{"username":%q,"password":%q,"totp":%q}`, user, password, code))
		if mm := regexp.MustCompile(`"csrf_token":"([^"]+)"`).FindStringSubmatch(resp); mm != nil {
			csrf = mm[1]
		}
		// only a route that is not there yet is worth retrying: the code can be used once
		return err == nil && st != 404, fmt.Sprint(st, err)
	})
	if st != 200 || csrf == "" {
		t.Fatalf("login: %d", st)
	}
	hdr := map[string]string{"X-CSRF-Token": csrf}
	st, body, _ = mustFetch(t, browser, "GET", base+"/api/v1/apps", hdr, "")
	if st != 200 || !strings.Contains(body, `"id":"work"`) {
		t.Fatalf("Apps does not list work: %d %s", st, body)
	}
	st, body, _ = mustFetch(t, browser, "POST", base+"/api/v1/apps/work/launch", hdr, "")
	mm := regexp.MustCompile(`"url":"([^"]+)"`).FindStringSubmatch(body)
	if st != 200 && st != 201 || mm == nil {
		t.Fatalf("launch: %d", st)
	}
	ticketURL := strings.ReplaceAll(mm[1], `\u0026`, "&")
	if !strings.HasPrefix(ticketURL, work+"/ui/login/ticket?t=cant_") {
		t.Fatalf("the launch address was not derived from the route (the ticket is not logged); host %q", strings.SplitN(ticketURL, "?", 2)[0])
	}

	// 4. redeem the ticket in the work UI, as the browser would: GET, then the confirmation POST.
	st, body, _ = mustFetch(t, browser, "GET", ticketURL, nil, "")
	form := url.Values{}
	for _, f := range hiddenInput.FindAllStringSubmatch(body, -1) {
		form.Set(f[1], f[2])
	}
	if st != 200 || form.Get("csrf") == "" || form.Get("ticket") == "" {
		t.Fatalf("ticket page: %d", st)
	}
	post := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": work, "Sec-Fetch-Site": "same-origin"}
	st, body, h = mustFetch(t, browser, "POST", work+"/ui/login/ticket", post, form.Encode())
	if st != 303 || !strings.HasPrefix(h.Get("Location"), "/ui/") {
		t.Fatalf("redeeming the ticket: %d %s", st, body)
	}
	st, body, _ = mustFetch(t, browser, "GET", work+"/ui/", nil, "")
	if st != 200 || !strings.Contains(body, `action="/ui/logout"`) {
		t.Fatalf("no signed-in work page: %d %s", st, body)
	}

	// 5. a public form post on the site host becomes a lead.
	site := "https://" + routeDomain
	var token string
	eventually(t, 120*time.Second, "/_forms through the ingress", func() (bool, string) {
		st, b, _, err := fetch(browser, "GET", site+"/_forms/contact/token", nil, "")
		if mm := regexp.MustCompile(`"token":"([^"]+)"`).FindStringSubmatch(b); mm != nil {
			token = mm[1]
		}
		return err == nil && st == 200 && token != "", fmt.Sprint(st, err, b)
	})
	time.Sleep(4 * time.Second) // the gateway's minimum fill time
	lead := url.Values{"name": {"Ingress Visitor"}, "email": {"visitor@visitor.example"}, "message": {"through /_forms"}, "_t": {token}}
	st, body, _ = mustFetch(t, browser, "POST", site+"/_forms/contact",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", "Origin": site}, lead.Encode())
	if st != 201 {
		t.Fatalf("form post: %d %s", st, body)
	}
	read := mustKubectl(t, "-n", "mod-forms", "get", "secret", "can-module", "-o", `go-template={{index .data "CAN_FORMS_API_READ_TOKEN" | base64decode}}`)
	api := portForward(t, "mod-forms", "forms-api", 8081)
	st, body, _ = mustFetch(t, &http.Client{Timeout: 15 * time.Second}, "GET", api+"/v1/leads", map[string]string{"Authorization": "Bearer " + read}, "")
	if st != 200 || !strings.Contains(body, "through /_forms") {
		t.Fatalf("the lead was not stored: %d", st)
	}
	if st, _, _ := mustFetch(t, browser, "GET", site+"/v1/leads", nil, ""); st == 200 {
		t.Error("the leads API answers on the site host")
	}

	// 6. switching the UI off removes the route; the module keeps running and leaves Apps.
	canApplyModules(t, routingCompany(false))
	eventually(t, 180*time.Second, "the work route to be removed", func() (bool, string) {
		st, _, _, err := fetch(browser, "GET", work+"/ui/login", nil, "")
		return err == nil && st == 404, fmt.Sprint(st, err)
	})
	eventually(t, 180*time.Second, "work to leave Apps", func() (bool, string) {
		st, b, _, err := fetch(browser, "GET", base+"/api/v1/apps", hdr, "")
		return err == nil && st == 200 && !strings.Contains(b, `"id":"work"`), fmt.Sprint(st, err, b)
	})
	if got := mustKubectl(t, "-n", "mod-work", "get", "networkpolicy", "allow-ingress-from-traefik", "-o", "jsonpath={.spec.ingress}"); got != "" && got != "[]" {
		t.Errorf("an unrouted module still admits the ingress controller: %s", got)
	}
	canApplyModules(t, fmt.Sprintf("schema: 1\nname: Route Co\ndomain: %s\nprovider:\n  type: local\nmodules:\n  work: {enabled: false}\n  forms: {enabled: false}\n", routeDomain))
}
