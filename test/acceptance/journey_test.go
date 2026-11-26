//go:build acceptance

package acceptance

// The journey of specs/058-acceptance-journey, in order. Every step is timed; a step that fails ends
// the journey, a budget that is exceeded fails it at the end. The product is driven the way a person
// drives it: the `can` binary, ssh to the server, the web pages. What stands in for the internet
// (certificate, DNS, mail relay, bucket) is listed in standIns and in the report.

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/test/acceptance/report"
	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

// Budgets are generous: they catch a step that stopped working at a usable speed on a shared CI
// runner, not a few percent. The product promise for the first `make up` is promiseUp.
const (
	promiseUp = 5 * time.Minute

	budgetInit    = 30 * time.Second
	budgetUp      = 10 * time.Minute
	budgetSite    = 2 * time.Minute
	budgetForm    = time.Minute
	budgetMail    = 3 * time.Minute
	budgetSignIn  = 90 * time.Second
	budgetModules = 15 * time.Minute
	budgetUse     = 3 * time.Minute
	budgetBackup  = 15 * time.Minute
	budgetDestroy = 5 * time.Minute
	budgetNewVM   = 8 * time.Minute
	budgetRestore = 20 * time.Minute
	budgetUpAgain = 10 * time.Minute

	// Working set of k3s, containerd and the pods (MiB). SPEC.md section 3.2 targets about 700 for
	// the platform on a 2 GB server.
	budgetIdleMiB    = 1400
	budgetModulesMiB = 2400
)

var standIns = []string{
	"The server is the CI runner VM itself, reached over ssh as a sudo user: the same bootstrap script and k3s as on a server, but not a fresh cloud image, and no VM creation time.",
	"The certificate is signed by a test CA and set as Traefik's default; Let's Encrypt is blocked. ACME issuance is not proven.",
	"DNS is /etc/hosts on the one machine; the domain is acceptance.test and `can up` runs with --domain-live.",
	"The mail relay is dev/compose/smtpsink in the cluster (STARTTLS, AUTH). can-core is told to allow its private address and to trust the test CA; deliverability is not proven.",
	"The backup bucket is an S3 gateway (versitygw) over a local directory on the same machine, which survives `can down`.",
	"The images are built from the checkout (`make images`), only those this company needs; upstream images (FreeScout, Postgres, Traefik, k3s) are pulled from their registries.",
	"'destroy' is `can down` (ssh provider): k3s and can are removed; the operating system, its firewall and packages stay.",
}

type journey struct {
	*env
	admin struct{ user, password, totp string }

	visitor   string
	workToken string
	issueKey  string
	issue     string
	supportPW string
	flags     []string // of `can up`
	kitPass   string
	kit       string // the recovery kit of the first install
}

func TestJourney(t *testing.T) {
	e := newEnv(t)
	j := &journey{env: e}
	e.rep.StandIns = standIns
	e.facts(t)
	t.Cleanup(func() { j.finish(t) })

	step := func(name string, budget time.Duration, fn func(t *testing.T) string) {
		t.Helper()
		note := ""
		ok := t.Run(name, func(t *testing.T) {
			start := time.Now()
			defer func() {
				status := e.rep.Add(name, time.Since(start), budget, t.Failed(), note)
				t.Logf("%s: %s in %s (budget %s)", name, status, time.Since(start).Round(time.Second), budget)
			}()
			note = fn(t)
		})
		if !ok {
			t.FailNow()
		}
	}

	step("init a company in an empty directory", budgetInit, j.init)
	j.seedCertificate(t)
	e.memory(t, "before k3s is installed", 0, 0)
	step("make up: k3s, platform, site, forms, admin, backup timer", budgetUp, j.firstUp)
	step("site served over HTTPS", budgetSite, j.site)
	e.memory(t, "after make up, idle", time.Minute, budgetIdleMiB)
	step("(stand-in) run the SMTP sink and let can-core use it", 0, func(t *testing.T) string { e.deploySink(t); return "not part of the product" })
	step("contact form submitted", budgetForm, j.submitForm)
	step("notification email received", budgetMail, func(t *testing.T) string { e.waitMail(t, j.visitor); return "STARTTLS and AUTH" })
	step("admin signs in with TOTP", budgetSignIn, func(t *testing.T) string { j.signIn(t, adminURL); return "" })
	step("enable work and support (make up again)", budgetModules, j.enableModules)
	e.memory(t, "with work and support, idle", 90*time.Second, budgetModulesMiB)
	step("first project: token in the admin UI, API call", budgetUse, j.firstProject)
	step("SSO launch of work and create an issue", budgetUse, j.createIssue)
	step("support: sign in and create a mailbox", budgetUse, j.supportMailbox)
	step("backup to the bucket and verify it", budgetBackup, j.backup)
	j.verifyServer(t)
	step("destroy the server (can down)", budgetDestroy, j.destroy)
	j.seedCertificate(t)
	step("restore: new server (can up --provision-only)", budgetNewVM, j.provisionOnly)
	step("restore: can restore on the server", budgetRestore, j.restore)
	step("restore: make up", budgetUpAgain, j.upAfterRestore)
	step("data present after the restore", budgetUse, j.dataPresent)
	e.memory(t, "after the restore, idle", time.Minute, budgetModulesMiB)
}

// finish writes the report whatever happened, and fails the journey on a budget.
func (j *journey) finish(t *testing.T) {
	if t.Failed() {
		j.diagnostics()
		j.rep.Note("the journey stopped at the first failed step; later steps did not run")
	}
	md := j.redact(j.rep.Markdown())
	_ = os.WriteFile(filepath.Join(j.out, "report.md"), []byte(md), 0o644) //nolint:gosec // published as a CI artifact
	if b, err := j.rep.JSON(); err == nil {
		_ = os.WriteFile(filepath.Join(j.out, "report.json"), []byte(j.redact(string(b))), 0o644) //nolint:gosec // published
	}
	t.Log("\n" + md)
	if v := j.rep.Violations(); len(v) > 0 && !t.Failed() {
		t.Errorf("budgets exceeded: %s", strings.Join(v, "; "))
	}
}

func (e *env) facts(t *testing.T) {
	t.Helper()
	e.rep.Facts["server"] = strings.TrimSpace(e.mustSSH(t, time.Minute, `. /etc/os-release; echo "$PRETTY_NAME, $(nproc) CPUs, $(awk '/^MemTotal:/{print int($2/1024)}' /proc/meminfo) MiB"`))
	out, _, err := run(time.Minute, e.company, operatorEnv(), "", e.canBin, "version")
	if err != nil {
		t.Fatal(err)
	}
	e.rep.Facts["can"] = strings.Join(strings.Fields(out), " ")
	if b, err := os.ReadFile(filepath.Join(e.out, "build.env")); err == nil {
		e.rep.Facts["built beforehand (not part of the journey)"] = strings.Join(strings.Fields(string(b)), ", ")
	}
}

// --- init ------------------------------------------------------------------------------------

func (j *journey) init(t *testing.T) string {
	entries, err := os.ReadDir(j.company)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the company directory must start empty: %v %v", entries, err)
	}
	j.mustCan(t, time.Minute, "init", ".", "--name", companyName, "--domain", domain, "--provider", "ssh", "--non-interactive")
	// What the operator writes into it: the server, the relay and who hears about new leads.
	j.editCompany(t, func(c map[string]any) {
		c["provider"] = map[string]any{"type": "ssh", "options": map[string]any{"host": j.host, "user": j.user, "key_file": filepath.Join(j.standins, "id_ed25519")}}
		c["email"] = map[string]any{"relay": map[string]any{"host": sinkHost, "port": 2525, "from": "no-reply@" + domain}}
		mapAt(mapAt(c, "modules"), "forms")["settings"] = map[string]any{"notify_email": []any{salesEmail}}
	})
	j.mustCan(t, time.Minute, "validate", ".")

	j.kitPass = "acceptance-" + randomHex(t, 12)
	j.secret(j.kitPass)
	pass := j.write(t, filepath.Join(j.standins, "kit.pass"), j.kitPass+"\n")
	smtp := j.write(t, filepath.Join(j.standins, "smtp.env"), "SMTP_USERNAME="+smtpUser+"\nSMTP_PASSWORD="+j.smtpPassword+"\n")
	j.flags = []string{
		"up", ".", "--images-from", j.images, "--output", "json", "--print-secrets", "--domain-live",
		"--backup-endpoint", s3URL, "--backup-bucket", "can-acceptance", "--backup-credentials-file", filepath.Join(j.standins, "s3.env"),
		"--kit-passphrase-file", pass, "--smtp-credentials-file", smtp,
	}
	return "company.yaml edited: ssh host, email relay, notify_email"
}

func mapAt(m map[string]any, key string) map[string]any {
	v, ok := m[key].(map[string]any)
	if !ok {
		v = map[string]any{}
		m[key] = v
	}
	return v
}

func (j *journey) companyYAML(t *testing.T) map[string]any {
	t.Helper()
	var c map[string]any
	if err := yaml.Unmarshal(j.read(t, filepath.Join(j.company, "company.yaml")), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (j *journey) editCompany(t *testing.T, edit func(map[string]any)) {
	t.Helper()
	c := j.companyYAML(t)
	edit(c)
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	j.write(t, filepath.Join(j.company, "company.yaml"), string(b))
}

// --- make up ----------------------------------------------------------------------------------

type upOutput struct {
	Phases []report.Phase `json:"phases"`
	Admin  *struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TOTPSecret string `json:"totp_secret"`
		TOTPURI    string `json:"totp_uri"`
	} `json:"admin"`
	KitFile       string   `json:"recovery_kit_file"`
	Warnings      []string `json:"warnings"`
	SiteReachable bool     `json:"site_reachable"`
}

// up runs `can up` (what `make up` runs) and records its phases and warnings.
func (j *journey) up(t *testing.T, label, kit string, timeout time.Duration) (upOutput, time.Duration) {
	t.Helper()
	args := append(append([]string{}, j.flags...), "--kit-out", filepath.Join(j.standins, kit))
	start := time.Now()
	out, _ := j.mustCan(t, timeout, args...)
	took := time.Since(start)
	var r upOutput
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("`can up --output json` did not print one JSON document: %v", err)
	}
	if r.Admin != nil {
		j.secret(r.Admin.Password, r.Admin.TOTPSecret, r.Admin.TOTPURI)
	}
	j.rep.Runs = append(j.rep.Runs, report.Run{Label: label, Phases: r.Phases})
	for _, w := range r.Warnings {
		j.rep.Note("`can up` (%s) warns: %s", label, j.redact(w))
	}
	if !r.SiteReachable {
		t.Errorf("`can up` says the site did not answer")
	}
	return r, took
}

func (j *journey) status(t *testing.T) {
	t.Helper()
	eventually(t, 2*time.Minute, "`can status` to say ready", func() (bool, string) {
		out, errOut, err := j.can(2*time.Minute, "status", ".")
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "warning:") {
				j.rep.Note("`can status` warns: %s", strings.TrimPrefix(l, "warning: "))
			}
		}
		return err == nil, out + errOut
	})
}

func (j *journey) firstUp(t *testing.T) string {
	r, took := j.up(t, "first, on a blank server", "kit-1.enc", budgetUp)
	if r.Admin == nil || r.Admin.Password == "" || r.Admin.TOTPSecret == "" {
		t.Fatal("no admin credentials")
	}
	j.admin.user, j.admin.password, j.admin.totp = r.Admin.Username, r.Admin.Password, r.Admin.TOTPSecret
	if r.KitFile == "" {
		t.Fatal("no recovery kit was written")
	}
	j.kit = r.KitFile
	j.status(t)
	// The erasure ledger upload needs `can` on the server, which `can up` does not copy
	// (docs/backup-restore.md): the operator's step.
	j.mustSSH(t, time.Minute, "install -m 0755 "+j.canBin+" /usr/local/bin/can")
	note := "within the five-minute promise"
	if took > promiseUp {
		note = fmt.Sprintf("OVER the five-minute promise (%.0f s)", took.Seconds())
		j.rep.Note("`make up` took %.0f s, more than the promised %.0f", took.Seconds(), promiseUp.Seconds())
	}
	return note + "; includes the k3s install, excludes creating a VM and building the images"
}

// --- the public site --------------------------------------------------------------------------

func (j *journey) site(t *testing.T) string {
	b := j.browser()
	var home page
	eventually(t, 90*time.Second, "https://"+domain, func() (bool, string) {
		p, err := b.do("GET", siteURL+"/", "", nil)
		if err != nil {
			return false, err.Error()
		}
		home = p
		return p.status == 200, fmt.Sprint(p.status)
	})
	if !strings.Contains(home.body, companyName) {
		t.Fatalf("the site does not carry the company name: %.300s", home.body)
	}
	if !strings.Contains(home.body, `href="/_forms/contact/form"`) {
		t.Fatalf("the site of a new company has no link to a contact form: %.1500s", home.body)
	}
	if p := b.get(t, "http://"+domain+"/", 301, 302, 307, 308); !strings.HasPrefix(p.header.Get("Location"), "https://") {
		t.Fatalf("plain HTTP goes to %q", p.header.Get("Location"))
	}
	return ""
}

var (
	reFormAction = regexp.MustCompile(`<form method="post" action="([^"]+)"`)
	reFormToken  = regexp.MustCompile(`name="_t" value="([^"]+)"`)
)

// submit follows the site's link to the form page and posts it, as a visitor would.
func (j *journey) submit(t *testing.T) {
	t.Helper()
	b := j.browser()
	pageURL := siteURL + "/_forms/contact/form"
	var p page
	eventually(t, 90*time.Second, "the form page", func() (bool, string) {
		got, err := b.do("GET", pageURL, "", nil)
		if err != nil {
			return false, err.Error()
		}
		p = got
		return got.status == 200, fmt.Sprint(got.status)
	})
	base, err := url.Parse(pageURL)
	if err != nil {
		t.Fatal(err)
	}
	action, err := base.Parse(html.UnescapeString(match(t, "form action", reFormAction, p.body)))
	if err != nil {
		t.Fatal(err)
	}
	token := html.UnescapeString(match(t, "timing token", reFormToken, p.body))
	time.Sleep(4 * time.Second) // faster than a person is refused (min_fill_seconds)
	j.visitor = "visitor" + randomHex(t, 4) + "@example.com"
	b.post(t, action.String(), url.Values{"name": {"A Visitor"}, "email": {j.visitor}, "message": {"Hello from the acceptance journey"}, "_t": {token}}, 200, 201, 303)
}

func (j *journey) submitForm(t *testing.T) string {
	j.submit(t)
	return "through the page the site links to"
}

// --- the admin --------------------------------------------------------------------------------

var reCanPAT = regexp.MustCompile(`<code>(canpat_[A-Za-z0-9_-]+)</code>`)

// signIn signs the admin in on the admin host with the password and a TOTP code.
func (j *journey) signIn(t *testing.T, base string) *browser {
	t.Helper()
	b := j.browser()
	var login page
	eventually(t, 60*time.Second, "the admin sign-in page", func() (bool, string) {
		p, err := b.do("GET", base+"/login", "", nil)
		if err != nil {
			return false, err.Error()
		}
		login = p
		return p.status == 200, fmt.Sprint(p.status)
	})
	form := url.Values{"csrf": {harness.CSRF(login.body)}, "username": {j.admin.user}, "password": {j.admin.password}, "totp": {j.totp(t, j.admin.totp)}}
	in := b.post(t, base+"/login", form, 303)
	if loc := in.header.Get("Location"); strings.Contains(loc, "login") {
		t.Fatalf("sign-in went back to %q", loc)
	}
	b.get(t, base+"/overview", 200)
	return b
}

// --- modules ----------------------------------------------------------------------------------

func (j *journey) rollout(t *testing.T, timeout time.Duration, module string) {
	t.Helper()
	start := time.Now()
	eventually(t, timeout, module+" to be rolled out", func() (bool, string) {
		out, err := j.ssh(2*time.Minute, fmt.Sprintf("k3s kubectl -n mod-%[1]s rollout status deploy/%[1]s --timeout=20s", module))
		if err != nil {
			problems, _ := j.ssh(time.Minute, "k3s kubectl -n can-system exec deploy/can-core -- /can-core modules problems; k3s kubectl -n mod-"+module+" get pods || true")
			return false, err.Error() + "\n" + problems
		}
		return true, out
	})
	t.Logf("%s rolled out %s after `can up` returned", module, time.Since(start).Round(time.Second))
}

func (j *journey) enableModules(t *testing.T) string {
	j.editCompany(t, func(c map[string]any) {
		m := mapAt(c, "modules")
		m["work"] = map[string]any{"expose": map[string]any{"ui": true, "api": true}}
		m["support"] = map[string]any{"expose": map[string]any{"ui": true}}
	})
	_, took := j.up(t, "enabling work and support", "kit-2.enc", budgetModules)
	returned := time.Now()
	j.rollout(t, 5*time.Minute, "work")
	j.rollout(t, 12*time.Minute, "support")
	j.status(t)
	return fmt.Sprintf("`can up` returned after %.0f s; both modules ready %.0f s later", took.Seconds(), time.Since(returned).Seconds())
}

func (j *journey) firstProject(t *testing.T) string {
	b := j.signIn(t, adminURL)
	var tokens page
	eventually(t, 2*time.Minute, "work tokens to be offered", func() (bool, string) {
		tokens = b.get(t, adminURL+"/tokens", 200)
		return strings.Contains(tokens.body, `value="work.admin"`), "work.admin is not offered"
	})
	r := b.post(t, adminURL+"/tokens", url.Values{"csrf": {harness.CSRF(tokens.body)}, "name": {"acceptance"}, "module": {"work"},
		"permissions": {"work.read", "work.write", "work.admin"}, "ttl_hours": {"24"}}, 200)
	j.workToken = match(t, "new token", reCanPAT, r.body)
	j.secret(j.workToken)
	api := j.browser()
	body := `{"key":"ACC","name":"Acceptance"}`
	eventually(t, 2*time.Minute, "the work API to create a project", func() (bool, string) {
		p, err := api.do("POST", workURL+"/v1/projects", body, map[string]string{"Authorization": "Bearer " + j.workToken, "Content-Type": "application/json"})
		if err != nil {
			return false, err.Error()
		}
		return p.status == 201, fmt.Sprintf("%d %.300s", p.status, p.body)
	})
	j.rep.Note("the work web UI has no way to create a project: a new company needs an API call with a token, and the API is only reachable with expose.api (or a port-forward)")
	return "the work UI cannot create a project"
}

var (
	reLaunch = regexp.MustCompile(`href="(https://work\.` + regexp.QuoteMeta(domain) + `/[^"]+)"`)
	reIdem   = regexp.MustCompile(`name="idem" value="([^"]*)"`)
)

func (j *journey) createIssue(t *testing.T) string {
	b := j.signIn(t, adminURL)
	var apps page
	eventually(t, 2*time.Minute, "work to be offered on the Apps page", func() (bool, string) {
		apps = b.get(t, adminURL+"/apps", 200)
		return strings.Contains(apps.body, `action="/apps/work/launch"`), tail(apps.body, 20)
	})
	b.get(t, workURL+"/ui/", 303, 401) // not signed in to work yet
	launched := b.post(t, adminURL+"/apps/work/launch", url.Values{"csrf": {harness.CSRF(apps.body)}}, 200)
	link, err := url.Parse(html.UnescapeString(match(t, "launch address", reLaunch, launched.body)))
	if err != nil {
		t.Fatal("bad launch address")
	}
	ticket := link.Query().Get("t")
	j.secret(ticket)
	confirm := b.get(t, link.String(), 200)
	in := b.post(t, workURL+link.Path, url.Values{"csrf": {harness.CSRF(confirm.body)}, "ticket": {ticket}}, 303)
	next, err := link.Parse(in.header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	b.get(t, next.String(), 200)
	project := b.get(t, workURL+"/ui/projects/ACC", 200)
	j.issue = "Order more anvils " + randomHex(t, 4)
	b.post(t, workURL+"/ui/projects/ACC/issues", url.Values{"csrf": {harness.CSRF(project.body)}, "idem": {match(t, "idempotency key", reIdem, project.body)},
		"title": {j.issue}, "type": {"task"}, "priority": {"P1"}}, 303)
	if !strings.Contains(b.get(t, workURL+"/ui/projects/ACC", 200).body, j.issue) {
		t.Fatal("the issue is not on the project page")
	}
	j.issueKey = "ACC-1"
	j.checkIssue(t)
	return "created in the web UI after the launch; read back with `can work get`"
}

// checkIssue reads the issue with the CLI and the token issued before.
func (j *journey) checkIssue(t *testing.T) {
	t.Helper()
	out, errOut, err := run(time.Minute, j.company, operatorEnv("CAN_WORK_URL="+workURL, "CAN_WORK_TOKEN="+j.workToken), "", j.canBin, "work", "get", j.issueKey)
	if err != nil || !strings.Contains(out, j.issue) {
		t.Fatalf("can work get %s: %v %s %s", j.issueKey, err, j.redact(out), j.redact(errOut))
	}
}

var reLaravelToken = regexp.MustCompile(`name="_token" value="([^"]+)"`)

const supportMailboxName = "Acceptance mailbox"

// supportSignIn signs in to FreeScout with the password can-core generated for it.
func (j *journey) supportSignIn(t *testing.T) *browser {
	t.Helper()
	b := j.browser()
	var login page
	eventually(t, 3*time.Minute, "the support sign-in page", func() (bool, string) {
		p, err := b.do("GET", supportURL+"/login", "", nil)
		if err != nil {
			return false, err.Error()
		}
		login = p
		return p.status == 200, fmt.Sprintf("%d %.200s", p.status, p.body)
	})
	p := b.post(t, supportURL+"/login", url.Values{"_token": {match(t, "support CSRF", reLaravelToken, login.body)}, "email": {"admin@" + domain}, "password": {j.supportPW}}, 302)
	if strings.HasSuffix(p.header.Get("Location"), "/login") {
		t.Fatal("support refused the generated password")
	}
	return b
}

// supportData reads the mailbox from the database and the marker from the volume.
const supportData = `k3s kubectl -n can-data exec deploy/postgres -- psql -U postgres -d mod_support -tAc "select email from mailboxes where name = '` + supportMailboxName + `'"
k3s kubectl -n mod-support exec deploy/support -c app -- cat /data/can-acceptance.txt`

func (j *journey) supportMailbox(t *testing.T) string {
	j.supportPW = strings.TrimSpace(j.mustSSH(t, time.Minute, `k3s kubectl -n mod-support get secret can-module -o jsonpath='{.data.CAN_SUPPORT_ADMIN_PASSWORD}' | base64 -d`))
	j.secret(j.supportPW)
	b := j.supportSignIn(t)
	form := b.get(t, supportURL+"/mailbox/new", 200)
	b.post(t, supportURL+"/mailbox/new", url.Values{"_token": {match(t, "mailbox CSRF", reLaravelToken, form.body)}, "email": {"help@" + domain}, "name": {supportMailboxName}}, 302)
	j.mustSSH(t, time.Minute, `k3s kubectl -n mod-support exec deploy/support -c app -- sh -c 'echo attachment-marker >/data/can-acceptance.txt'`)
	j.checkSupportData(t)
	return "a mailbox through the web UI, a file on its volume; the password is read from the cluster Secret"
}

func (j *journey) checkSupportData(t *testing.T) {
	t.Helper()
	out := j.mustSSH(t, 2*time.Minute, supportData)
	if !strings.Contains(out, "help@"+domain) || !strings.Contains(out, "attachment-marker") {
		t.Fatalf("support data: %q", out)
	}
}

// --- backup, destroy, restore -----------------------------------------------------------------

func (j *journey) backup(t *testing.T) string {
	out := j.mustSSH(t, 14*time.Minute, `k3s kubectl -n can-system exec deploy/can-core -- /can-core dump-request --wait 10m
systemctl start can-backup.service || { journalctl -u can-backup --no-pager | tail -40; exit 1; }
journalctl -u can-backup --no-pager | tail -30
can backup status
can backup verify
systemctl is-enabled can-backup.timer`)
	j.save("backup", out)
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "WARNING") || strings.Contains(l, "FAILED") {
			j.rep.Note("backup: %s", strings.TrimSpace(l))
		}
	}
	// Before anything is destroyed (docs/real-server-verification.md 4.2): the kit alone opens the
	// repository, and what a new server needs is copied away.
	j.mustSSH(t, 5*time.Minute, fmt.Sprintf(`install -d -m 0700 /root/kit-check
install -m 0600 %s /root/kit-check/kit.enc
install -m 0600 %s /root/kit-check/pass
can backup snapshots --kit /root/kit-check/kit.enc --passphrase-file /root/kit-check/pass
rm -rf /root/kit-check
install -m 0755 "$(command -v restic)" %s`, j.kit, filepath.Join(j.standins, "kit.pass"), filepath.Join(j.standins, "restic")))
	return "dump run, can-backup.service, status, verify, snapshots with the kit alone"
}

// verifyServer records what `can verify --server` says about the install. It is not a step of the
// journey: a failed check is reported, the journey goes on.
func (j *journey) verifyServer(t *testing.T) {
	out, errOut, err := j.can(10*time.Minute, "verify", "--server", "--probe-public", "--probe-backup-repo")
	j.save("verify", out+errOut)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(strings.ToUpper(l)), "FAIL") {
			j.rep.Note("`can verify --server`: %s", strings.Join(strings.Fields(l), " "))
		}
	}
	result := "no check failed"
	if err != nil {
		result = "exit " + err.Error()
	}
	j.rep.Note("`can verify --server --probe-public --probe-backup-repo`: %s", result)
	t.Log(j.redact(out))
}

func (j *journey) destroy(t *testing.T) string {
	j.mustCan(t, budgetDestroy, "down", ".", "--yes")
	// The machine stays: take away what a blank server would not have.
	j.mustSSH(t, time.Minute, `test ! -e /usr/local/bin/k3s
test ! -e /var/lib/rancher/k3s/storage
test ! -e /var/lib/can
test ! -e /etc/systemd/system/can-backup.timer
rm -rf /usr/local/bin/can /usr/local/bin/restic /var/cache/can-backup /root/.cache/restic`)
	if _, _, err := j.can(time.Minute, "status", "."); err == nil {
		t.Fatal("`can status` says ready after `can down`")
	}
	return ""
}

func (j *journey) provisionOnly(t *testing.T) string {
	j.mustCan(t, budgetNewVM, "up", ".", "--provision-only")
	return ""
}

func (j *journey) restore(t *testing.T) string {
	before := j.companyYAML(t)
	// docs/backup-restore.md "Restore": can, restic, the kit and the images on the new server.
	out := j.mustSSH(t, budgetRestore, fmt.Sprintf(`install -d -m 0700 /root/restore
install -m 0755 %[1]s /usr/local/bin/can
install -m 0755 %[2]s /usr/local/bin/restic
install -m 0600 %[3]s /root/restore/kit.enc
install -m 0600 %[4]s /root/restore/pass
cp -r %[5]s /root/restore/images
cd /root/restore
can restore --kit /root/restore/kit.enc --passphrase-file /root/restore/pass --images-from /root/restore/images --dir /root/restore/company 2>&1`,
		j.canBin, filepath.Join(j.standins, "restic"), j.kit, filepath.Join(j.standins, "kit.pass"), j.images))
	j.save("restore", out)
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "WARNING") || strings.Contains(l, "warning:") || strings.Contains(l, "NOT replayed") {
			j.rep.Note("can restore: %s", strings.TrimSpace(l))
		}
	}
	var restored map[string]any
	if err := yaml.Unmarshal([]byte(j.mustSSH(t, time.Minute, "cat /root/restore/company/company.yaml")), &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, restored) {
		t.Errorf("the restored company.yaml differs from the operator's: %v / %v", restored, before)
	}
	return "images from `make images`, kit encrypted"
}

func (j *journey) upAfterRestore(t *testing.T) string {
	r, _ := j.up(t, "after the restore", "kit-3.enc", budgetUpAgain)
	if r.Admin != nil {
		t.Fatal("`can up` issued a new admin login over the restored one")
	}
	j.rollout(t, 5*time.Minute, "forms")
	j.rollout(t, 8*time.Minute, "work")
	j.rollout(t, 12*time.Minute, "support")
	j.status(t)
	return ""
}

func (j *journey) dataPresent(t *testing.T) string {
	j.site(t)
	j.signIn(t, adminURL) // the same password and TOTP secret
	leads := j.mustSSH(t, 3*time.Minute, `tok=$(k3s kubectl -n mod-forms get secret can-module -o jsonpath='{.data.CAN_FORMS_API_READ_TOKEN}' | base64 -d)
k3s kubectl -n mod-forms port-forward svc/forms-api 18081:8081 >/dev/null 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
for i in $(seq 1 60); do
  if printf 'header = "Authorization: Bearer %s"\n' "$tok" | curl -fsS -K - 'http://127.0.0.1:18081/v1/leads?form=contact'; then exit 0; fi
  sleep 2
done
exit 1`)
	if !strings.Contains(leads, j.visitor) {
		t.Fatalf("the lead of %s is not in the restored forms module", j.visitor)
	}
	eventually(t, 2*time.Minute, "the work API", func() (bool, string) {
		p, err := j.browser().do("GET", workURL+"/v1/projects", "", map[string]string{"Authorization": "Bearer " + j.workToken})
		if err != nil {
			return false, err.Error()
		}
		return p.status == 200, fmt.Sprintf("%d %.200s", p.status, p.body)
	})
	j.checkIssue(t)
	j.checkSupportData(t)
	j.supportSignIn(t)
	// The relay credentials came back too: a new lead is mailed.
	j.deploySink(t)
	j.submit(t)
	j.waitMail(t, j.visitor)
	return "site, admin login, lead, work issue and token, support mailbox, file and login, a new lead mailed"
}
