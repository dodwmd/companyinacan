//go:build integration

package integration

// Epic E07b (specs/041-wrapped-modules-live): the wrapped apps installed through can-core on a real
// cluster, from the pinned upstream images. For each of analytics, support and newsletter: enable it
// from company.yaml, wait for the rollout, load its sign-in page through Traefik on the module's own
// host, sign in with the password can-core generated, make the app write to its database and read the
// row back from the database itself, let it idle and record its memory, then disable it (data kept)
// and delete the namespace.
//
// What it does NOT cover: a real domain or certificate (Traefik answers with its default one), mail
// delivery (the relay host does not resolve; only that support's settings were written is checked),
// inbound mail, backups, upgrades, or the capability adapters (not built).
//
// Runs only with CAN_IT_CANCORE=1 and CAN_IT_WRAPPED=1 (.github/workflows/wrapped-modules.yml). It
// deletes the namespaces mod-analytics, mod-support and mod-newsletter: use a throw-away cluster.

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/routing"
)

const (
	wrappedDomain = "wrapped.example"
	relayPassword = "ci relay $ecret-not-real"
	browserUA     = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"
)

func wrappedCompany(id string, enabled bool) string {
	y := fmt.Sprintf("schema: 1\nname: Wrapped Co\ndomain: %s\nprovider:\n  type: local\nmodules:\n  %s:\n    enabled: %v\n", wrappedDomain, id, enabled)
	if id == "support" {
		y += supportMail(true)
	}
	return y
}

// supportMail is the opt-in without which support is given no relay credentials.
func supportMail(on bool) string {
	return fmt.Sprintf("email:\n  modules:\n    support: {enabled: %v}\n", on)
}

// browser is a minimal client that carries cookies by hand (they are Secure and the port-forward is
// plain HTTP), sends the module's public Host and never follows redirects.
type browser struct {
	base, host string
	cookies    map[string]string
}

func newBrowser(base, host string) *browser {
	return &browser{base: base, host: host, cookies: map[string]string{}}
}

type page struct {
	status   int
	location string
	body     string
}

func (b *browser) do(t *testing.T, method, path, contentType, body string, headers map[string]string) page {
	t.Helper()
	req, err := http.NewRequest(method, b.base+path, strings.NewReader(body)) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	req.Host = b.host
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("X-Forwarded-Proto", "https")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	var jar []string
	for k, v := range b.cookies {
		jar = append(jar, k+"="+v)
	}
	if len(jar) > 0 {
		req.Header.Set("Cookie", strings.Join(jar, "; "))
	}
	cl := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req) //nolint:gosec // loopback
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	for _, c := range resp.Cookies() {
		b.cookies[c.Name] = c.Value
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return page{status: resp.StatusCode, location: resp.Header.Get("Location"), body: string(raw)}
}

func (b *browser) form(t *testing.T, path string, v url.Values) page {
	t.Helper()
	return b.do(t, "POST", path, "application/x-www-form-urlencoded", v.Encode(), nil)
}

// throughTraefik loads path on the module's public host through the cluster's ingress (k3d maps
// 443 to the host). The certificate is Traefik's default one: there is no real domain here.
func throughTraefik(t *testing.T, host, path, want string) {
	t.Helper()
	cl := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: host}, //nolint:gosec // default certificate on a throw-away cluster
	}}
	eventually(t, 90*time.Second, "Traefik to serve https://"+host+path, func() (bool, string) {
		req, err := http.NewRequest("GET", "https://127.0.0.1"+path, nil) //nolint:noctx // test helper
		if err != nil {
			return false, err.Error()
		}
		req.Host = host
		resp, err := cl.Do(req) //nolint:gosec // loopback
		if err != nil {
			return false, err.Error()
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode == 200 && strings.Contains(string(raw), want), fmt.Sprintf("%d, %d bytes", resp.StatusCode, len(raw))
	})
}

// adminPassword reads the generated password the way an operator does. It is never logged.
func adminPassword(t *testing.T, module, key string) string {
	t.Helper()
	enc := mustKubectl(t, "-n", "mod-"+module, "get", "secret", "can-module", "-o", "jsonpath={.data."+key+"}")
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(b) < 32 {
		t.Fatalf("no generated %s", key)
	}
	return string(b)
}

// moduleSQL runs a query in a module's database inside the shared Postgres.
func moduleSQL(t *testing.T, db, query string) string {
	t.Helper()
	out, err := kubectl("-n", "can-data", "exec", "deploy/postgres", "--", "psql", "-U", "postgres", "-d", db, "-tAc", query)
	if err != nil {
		t.Fatalf("query in %s: %v\n%s", db, err, out)
	}
	return out
}

func enableWrapped(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, args := range [][]string{
			{"-n", "can-system", "logs", "deploy/can-core", "--tail=120"},
			{"-n", "mod-" + id, "get", "all,pvc,networkpolicy,resourcequota", "-o", "wide"},
			{"-n", "mod-" + id, "describe", "pod"},
			{"-n", "mod-" + id, "logs", "deploy/" + id, "--all-containers", "--tail=150"},
			{"-n", "can-data", "get", "all,pvc", "-o", "wide"},
			{"-n", "can-data", "logs", "deploy/postgres", "--tail=60"},
		} {
			out, _ := kubectl(args...)
			t.Logf("kubectl %v:\n%s", args, out)
		}
	})
	start := time.Now()
	canApplyModules(t, wrappedCompany(id, true))
	// 12 minutes: image pulls, the first database server and, for support, the first-boot migrations.
	// A rollout that exceeds the installer's 5 minutes is removed and retried by the reconciler.
	eventually(t, 12*time.Minute, id+" to be rolled out", func() (bool, string) {
		o, err := kubectl("-n", "mod-"+id, "rollout", "status", "deploy/"+id, "--timeout=5s")
		return err == nil, o
	})
	measureNote(t, fmt.Sprintf("%s: rolled out %s after can apply (image pull and first boot included)", id, time.Since(start).Round(time.Second)))
	if out := mustKubectl(t, "-n", "mod-"+id, "get", "pod", "-l", "app.kubernetes.io/name="+id, "-o", "jsonpath={.items[0].status.containerStatuses[*].restartCount}"); out != "0" {
		t.Errorf("%s restarted: %s", id, out)
	}
}

// removeWrapped disables the module (can-core keeps its data) and then deletes the namespace, which
// is the manual purge.
func removeWrapped(t *testing.T, id string) {
	t.Helper()
	canApplyModules(t, wrappedCompany(id, false))
	eventually(t, 150*time.Second, id+" to scale to zero", func() (bool, string) {
		o, _ := kubectl("-n", "mod-"+id, "get", "deploy", id, "-o", "jsonpath={.spec.replicas}/{.status.replicas}")
		return o == "0/" || o == "0/0", o
	})
	mustKubectl(t, "-n", "mod-"+id, "get", "pvc", id+"-data")
	mustKubectl(t, "-n", "mod-"+id, "get", "secret", "can-module")
	mustKubectl(t, "delete", "namespace", "mod-"+id, "--wait=true", "--timeout=180s")
}

func measureNote(t *testing.T, line string) {
	t.Helper()
	t.Log("MEASURE " + line)
	if p := os.Getenv("CAN_IT_MEASURE_FILE"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // test output path
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		_, _ = fmt.Fprintf(f, "- %s\n", line)
	}
}

// measureIdle lets the app settle and records the working set of its container's cgroup (current
// minus inactive file pages, what `kubectl top` reports), read from inside the container.
func measureIdle(t *testing.T, label, ns, deploy, container string) {
	t.Helper()
	settle := 180 * time.Second
	if v, err := strconv.Atoi(os.Getenv("CAN_IT_SETTLE_SECONDS")); err == nil {
		settle = time.Duration(v) * time.Second
	}
	time.Sleep(settle)
	var samples []int
	for range 6 {
		// -c: without it kubectl prints "Defaulted container ..." into the output of a pod with init containers
		out := mustKubectl(t, "-n", ns, "exec", "deploy/"+deploy, "-c", container, "--", "sh", "-c",
			"cat /sys/fs/cgroup/memory.current; grep -E '^inactive_file ' /sys/fs/cgroup/memory.stat")
		f := strings.Fields(out)
		if len(f) != 3 {
			t.Fatalf("unexpected cgroup output %q", out)
		}
		cur, err1 := strconv.Atoi(f[0])
		inactive, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil {
			t.Fatalf("unexpected cgroup output %q", out)
		}
		samples = append(samples, (cur-inactive)>>20)
		time.Sleep(10 * time.Second)
	}
	lo, hi := samples[0], samples[0]
	for _, s := range samples {
		lo, hi = min(lo, s), max(hi, s)
	}
	measureNote(t, fmt.Sprintf("%s: idle working set %d to %d MiB (6 samples over 1 minute, after %s idle)", label, lo, hi, settle))
}

func analyticsChecks(t *testing.T) {
	host := "stats." + wrappedDomain
	pw := adminPassword(t, "analytics", "CAN_ANALYTICS_ADMIN_PASSWORD")
	throughTraefik(t, host, "/user/new", `name="password"`)
	b := newBrowser(portForward(t, "mod-analytics", "analytics", 8080), host)
	login := func(password string) page {
		return b.form(t, "/user/requestlogin", url.Values{"email": {"admin@" + wrappedDomain}, "password": {password}})
	}
	if p := login("not-the-password-at-all"); p.location == "/" {
		t.Error("a wrong password signed in")
	}
	if p := login(pw); p.status != 303 || p.location != "/" {
		t.Fatalf("signing in with the generated password: %d to %q", p.status, p.location)
	}
	// A page view, which GoatCounter writes to its SQLite database within its store interval.
	if p := b.do(t, "GET", "/count?p=/ci-page&t=ci", "", "", map[string]string{"X-Forwarded-For": "8.8.8.8"}); p.status != 200 {
		t.Fatalf("count: %d", p.status)
	}
	eventually(t, 60*time.Second, "the page view to be in the database", func() (bool, string) {
		o, err := kubectl("-n", "mod-analytics", "exec", "deploy/analytics", "-c", "app", "--",
			"goatcounter", "db", "query", "select path from paths where path = '/ci-page'", "-db", "sqlite+/data/goatcounter.sqlite3")
		return err == nil && strings.Contains(o, "/ci-page"), o
	})
}

var csrfRE = regexp.MustCompile(`name="_token" value="([^"]+)"`)

func supportChecks(t *testing.T) {
	host := "support." + wrappedDomain
	pw := adminPassword(t, "support", "CAN_SUPPORT_ADMIN_PASSWORD")
	throughTraefik(t, host, "/login", `name="password"`)
	b := newBrowser(portForward(t, "mod-support", "support", 8080), host)
	token := func(path string) string {
		p := b.do(t, "GET", path, "", "", nil)
		m := csrfRE.FindStringSubmatch(p.body)
		if p.status != 200 || m == nil {
			t.Fatalf("GET %s: %d, CSRF token found: %v", path, p.status, m != nil)
		}
		return m[1]
	}
	p := b.form(t, "/login", url.Values{"_token": {token("/login")}, "email": {"admin@" + wrappedDomain}, "password": {pw}})
	if p.status != 302 || strings.HasSuffix(p.location, "/login") {
		t.Fatalf("signing in with the generated password: %d to %q", p.status, p.location)
	}
	p = b.form(t, "/mailbox/new", url.Values{"_token": {token("/mailbox/new")}, "email": {"help@" + wrappedDomain}, "name": {"CI mailbox"}})
	if p.status != 302 {
		t.Fatalf("creating a mailbox: %d", p.status)
	}
	if got := moduleSQL(t, "mod_support", "select email from mailboxes where name = 'CI mailbox'"); got != "help@"+wrappedDomain {
		t.Errorf("the mailbox is not in the database: %q", got)
	}
	// The company's relay was written into FreeScout's system mail settings (no mail is sent).
	eventually(t, 3*time.Minute, "the SMTP relay settings to be applied", func() (bool, string) {
		o := moduleSQL(t, "mod_support", "select name || '=' || value from options where name in ('mail_driver','mail_host','mail_port','mail_username','mail_encryption','mail_from') order by name")
		return strings.Contains(o, "mail_driver=smtp") && strings.Contains(o, "mail_host=smtp.relay.invalid") && strings.Contains(o, "mail_from=no-reply@"+wrappedDomain) && strings.Contains(o, "mail_encryption=tls"), o
	})
	if got := moduleSQL(t, "mod_support", "select value from options where name = 'mail_password'"); got == "" || strings.Contains(got, "relay") {
		t.Error("the relay password is missing or stored in clear text")
	}
	// Opting out is an apply of its own that empties the module's Secret. What FreeScout stored stays.
	canApplyModules(t, strings.Replace(wrappedCompany("support", true), supportMail(true), supportMail(false), 1))
	eventually(t, 4*time.Minute, "can-smtp to be emptied", func() (bool, string) {
		o, err := kubectl("-n", "mod-support", "get", "secret", "can-smtp", "-o", "jsonpath={.data}")
		return err == nil && (o == "" || o == "{}"), fmt.Sprintf("%d bytes of data", len(o))
	})
	eventually(t, 8*time.Minute, "support to be back after the opt-out", func() (bool, string) {
		o, err := kubectl("-n", "mod-support", "rollout", "status", "deploy/support", "--timeout=5s")
		return err == nil, o
	})
}

func newsletterChecks(t *testing.T) {
	host := "newsletter." + wrappedDomain
	pw := adminPassword(t, "newsletter", "CAN_NEWSLETTER_ADMIN_PASSWORD")
	throughTraefik(t, host, "/admin/login", `name="password"`)
	b := newBrowser(portForward(t, "mod-newsletter", "newsletter", 9000), host)
	p := b.do(t, "GET", "/admin/login", "", "", nil)
	nonce := regexp.MustCompile(`name="nonce" value="([^"]+)"`).FindStringSubmatch(p.body)
	if p.status != 200 || nonce == nil {
		t.Fatalf("login page: %d", p.status)
	}
	p = b.form(t, "/admin/login", url.Values{"username": {"admin"}, "password": {pw}, "nonce": {nonce[1]}, "next": {"/admin"}})
	if p.status != 302 || p.location != "/admin" {
		t.Fatalf("signing in with the generated password: %d to %q", p.status, p.location)
	}
	body, _ := json.Marshal(map[string]string{"name": "ci-list", "type": "private", "optin": "single"})
	if p = b.do(t, "POST", "/api/lists", "application/json", string(body), nil); p.status != 200 {
		t.Fatalf("creating a list: %d %s", p.status, p.body)
	}
	if got := moduleSQL(t, "mod_newsletter", "select count(*) from lists where name = 'ci-list'"); got != "1" {
		t.Errorf("the list is not in the database: %q", got)
	}
}

func TestWrappedModulesThroughCanCore(t *testing.T) {
	if os.Getenv("CAN_IT_CANCORE") != "1" || os.Getenv("CAN_IT_WRAPPED") != "1" {
		t.Skip("CAN_IT_CANCORE=1 and CAN_IT_WRAPPED=1 are not both set")
	}
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")
	// The routing facts `can up` would write (specs/040): without them can-core leaves every module
	// unrouted, and the route policy refuses any route.
	applyRoutingFacts(t, routing.Default(wrappedDomain))
	// A relay that does not resolve: support is configured with it, nothing can be sent.
	secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata: {name: can-smtp-relay, namespace: can-system}\nstringData:\n  host: smtp.relay.invalid\n  username: ci-user\n  password: %q\n  from: no-reply@%s\n", relayPassword, wrappedDomain)
	if out, err := kubectlStdin(secret, "apply", "-f", "-"); err != nil {
		t.Fatalf("creating the relay Secret: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = kubectl("-n", "can-system", "delete", "secret", "can-smtp-relay", "--ignore-not-found") })
	// The kubelet takes up to a minute or two to show a new Secret in can-core's mounted volume, and
	// support is applied first: a restart mounts it at once.
	mustKubectl(t, "-n", "can-system", "rollout", "restart", "deploy/can-core")
	mustKubectl(t, "-n", "can-system", "rollout", "status", "deploy/can-core", "--timeout=180s")

	t.Run("support", func(t *testing.T) {
		enableWrapped(t, "support")
		supportChecks(t)
		measureIdle(t, "support (FreeScout)", "mod-support", "support", "app")
		measureIdle(t, "shared Postgres (with the support database)", "can-data", "postgres", "postgres")
		removeWrapped(t, "support")
	})
	// After support, so that the shared Postgres is running while a module without a database is.
	t.Run("analytics", func(t *testing.T) {
		enableWrapped(t, "analytics")
		analyticsChecks(t)
		// By address, so that a failed name lookup cannot pass for a blocked connection.
		ip := mustKubectl(t, "-n", "can-data", "get", "svc", "postgres", "-o", "jsonpath={.spec.clusterIP}")
		eventually(t, time.Minute, "a module without a database to be kept away from the shared Postgres", func() (bool, string) {
			o, err := kubectl("-n", "mod-analytics", "exec", "deploy/analytics", "-c", "app", "--", "sh", "-c",
				"if nc -w 3 "+ip+" 5432 </dev/null; then echo reached; else echo blocked; fi")
			return err == nil && strings.HasSuffix(o, "blocked"), o
		})
		measureIdle(t, "analytics (GoatCounter)", "mod-analytics", "analytics", "app")
		removeWrapped(t, "analytics")
	})
	t.Run("newsletter", func(t *testing.T) {
		enableWrapped(t, "newsletter")
		newsletterChecks(t)
		measureIdle(t, "newsletter (Listmonk)", "mod-newsletter", "newsletter", "app")
		// One database and one user per module: newsletter's credentials do not open support's database.
		pw, err := base64.StdEncoding.DecodeString(mustKubectl(t, "-n", "mod-newsletter", "get", "secret", "can-db", "-o", "jsonpath={.data.DB_PASSWORD}"))
		if err != nil {
			t.Fatal("no database password")
		}
		connect := func(db string) (string, error) {
			return kubectlStdin(string(pw)+"\n", "-n", "can-data", "exec", "-i", "deploy/postgres", "--", "sh", "-c",
				`read -r PGPASSWORD; export PGPASSWORD; psql -h "$(hostname -i)" -U mod_newsletter -d `+db+` -tAc 'select 1'`)
		}
		if out, err := connect("mod_newsletter"); err != nil || out != "1" {
			t.Fatalf("newsletter cannot open its own database: %v %s", err, out)
		}
		for _, db := range []string{"postgres", "template1"} {
			if out, err := connect(db); err == nil {
				t.Errorf("a module's role opened the maintenance database %s: %s", db, out)
			}
		}
		if got := moduleSQL(t, "postgres", "select rolconnlimit from pg_roles where rolname = 'mod_newsletter'"); got != "20" {
			t.Errorf("connection limit of the module role: %q", got)
		}
		if t.Failed() {
			return
		}
		if moduleSQL(t, "postgres", "select count(*) from pg_database where datname = 'mod_support'") == "1" {
			if out, err := connect("mod_support"); err == nil {
				t.Errorf("newsletter's user opened support's database: %s", out)
			}
		}
		removeWrapped(t, "newsletter")
	})
}
