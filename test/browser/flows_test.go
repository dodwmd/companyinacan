//go:build browser

package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/dodwmd/companyinacan/test/e2e/harness"
)

var (
	env = harness.FromEnv()
	web = harness.NewClient(env)
)

func TestMain(m *testing.M) {
	if err := waitForEdge(); err != nil {
		fmt.Fprintf(os.Stderr, "browser: the edge does not answer at 127.0.0.1:%s: %v. Start the stack with `make e2e-up`.\n", env.Port, err)
		os.Exit(2)
	}
	stop, err := startChrome()
	if err != nil {
		fmt.Fprintf(os.Stderr, "browser: cannot start Chrome (set E2E_CHROME to the binary): %v\n", err)
		os.Exit(2)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

func waitForEdge() error {
	c := &http.Client{Timeout: 3 * time.Second}
	var last error
	for end := time.Now().Add(90 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Second) {
		r, err := c.Get("http://127.0.0.1:" + env.Port + "/_edge/healthz")
		if err == nil {
			_ = r.Body.Close()
			if r.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("HTTP %d", r.StatusCode)
		}
		last = err
	}
	return last
}

// The addresses a person types. The admin UI and the module UIs share the host name localhost and
// differ by port, which is what the Apps launcher needs over plain http (docs/local-compose.md).
func appURL(offset int) string { return fmt.Sprintf("http://localhost:%d", env.AppsPort+offset) }

func adminURL() string   { return appURL(0) }
func workURL() string    { return appURL(1) + "/ui" }
func peopleURL() string  { return appURL(2) }
func jobsURL() string    { return appURL(3) }
func financeURL() string { return appURL(4) }
func filesURL() string   { return appURL(5) }
func equityURL() string  { return appURL(6) }

func host(name string) string { return "http://" + name + ".localhost:" + env.Port }

func apiOf(t *testing.T, module, key string) harness.API {
	return harness.API{C: web, Host: module + ".localhost", Token: env.Secret(t, module, key)}
}

var totpSecrets = map[string]string{}

// signIn fills the admin sign-in form. A TOTP code can be used once per 30 second step, so when
// the code was used already it waits for the next step and tries once more.
func signIn(s *session, name, secret string) {
	s.t.Helper()
	signInAt(s, adminURL(), name, secret)
}

// signInAt is signIn at one of the admin UI's addresses.
func signInAt(s *session, base, name, secret string) {
	s.t.Helper()
	pw := env.Secret(s.t, "core", "CAN_ADMIN_PASSWORD")
	s.allow(`HTTP 401|status of 401`)
	defer func() { s.allowed = s.allowed[:len(s.allowed)-1] }()
	for attempt := 0; ; attempt++ {
		s.goTo(base + "/login")
		code, err := harness.TOTP(secret, time.Now())
		if err != nil {
			s.t.Fatalf("%v", err)
		}
		s.fill("#username", name)
		s.fill("#password", pw)
		s.fill("#totp", code)
		s.submit("/login")
		if s.path() == "/overview" {
			return
		}
		if attempt >= 1 {
			s.t.Fatalf("sign-in of %s failed: %.300s", name, s.text())
		}
		time.Sleep(time.Duration(31-time.Now().Unix()%30) * time.Second)
	}
}

// adminSecret rotates the admin's TOTP secret once per run, as dev/compose/bootstrap-admin.sh does.
func adminSecret(t *testing.T) string {
	t.Helper()
	if totpSecrets["admin"] == "" {
		pw := env.Secret(t, "core", "CAN_ADMIN_PASSWORD")
		out := env.Run(t, "exec", "-T", "-e", "CAN_ADMIN_PASSWORD="+pw, "core", "/can-core", "admin", "reset-password", "--rotate-totp")
		totpSecrets["admin"] = harness.ParseTOTPSecret(out)
		if totpSecrets["admin"] == "" {
			t.Fatal("no TOTP secret from can-core admin reset-password")
		}
	}
	return totpSecrets["admin"]
}

func TestBrowser(t *testing.T) {
	for _, vp := range viewports {
		t.Run(vp.Name, func(t *testing.T) { journey(t, vp) })
	}
}

// run is what the scenarios of one viewport share.
type run struct {
	vp    viewport
	u     string // unique suffix of this run's data
	admin *session
	key   string // work project
}

func journey(t *testing.T, vp viewport) {
	r := &run{vp: vp, u: harness.Unique()}
	r.admin = newSession(t, vp)
	secret := adminSecret(t)

	if !t.Run("admin_login_totp", func(t *testing.T) {
		s := r.admin.in(t)
		// Signed out, the admin UI sends the browser to the sign-in page.
		s.goTo(adminURL() + "/overview")
		if s.path() != "/login" {
			t.Fatalf("signed out, /overview shows %s", s.path())
		}
		signIn(s, "admin", secret)
		var cookies []*network.Cookie
		s.run(chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			cookies, err = network.GetCookies().WithURLs([]string{adminURL() + "/"}).Do(ctx)
			return err
		}))
		found := false
		for _, c := range cookies {
			if c.Name == "can_session" {
				found = true
				if !c.HTTPOnly || c.SameSite != network.CookieSameSiteStrict {
					t.Errorf("can_session cookie: HttpOnly=%v SameSite=%q", c.HTTPOnly, c.SameSite)
				}
			}
		}
		if !found {
			t.Error("the browser holds no can_session cookie after sign-in")
		}
	}) {
		return
	}

	// One directory person for the admin account: can-people's UI admits only people it knows.
	apiOf(t, "people", "CAN_PEOPLE_API_TOKEN").Post(t, "/v1/people", map[string]any{
		"kind": "employee", "status": "active", "given_name": "Admin", "family_name": r.u,
		"work_email": "admin-" + r.u + "@acme.example", "sso_subject": "user:admin",
	}).Expect(t, "link a directory person to the admin account", 201, 409, 422)

	t.Run("lead_form", func(t *testing.T) { leadForm(t, r) })
	t.Run("overview_and_admin_pages", func(t *testing.T) { adminPages(t, r) })
	t.Run("website_editor", func(t *testing.T) { websiteEditor(t, r) })
	t.Run("launch_ticket_binding", func(t *testing.T) { launchBinding(t, r) })
	t.Run("work", func(t *testing.T) { workFlow(t, r) })
	t.Run("people", func(t *testing.T) { peopleFlow(t, r) })
	t.Run("jobs_and_careers", func(t *testing.T) { jobsFlow(t, r) })
	t.Run("finance_and_signing", func(t *testing.T) { financeFlow(t, r) })
	t.Run("files", func(t *testing.T) { filesFlow(t, r) })
	t.Run("equity", func(t *testing.T) { equityFlow(t, r) })
	t.Run("overview_links", func(t *testing.T) { overviewLinks(t, r) })
	t.Run("token_login_pages", func(t *testing.T) { tokenLogins(t, r) })
	t.Run("cross_site_posts", func(t *testing.T) { crossSite(t, r) })
	t.Run("account_switch", func(t *testing.T) { accountSwitch(t, r) })
}

// launch opens a module from the Apps page, the way a person does: the interstitial moves the
// browser to the module (another origin), whose Continue form redeems the ticket with the nonce
// cookie can-core set.
func launch(s *session, module, base string) {
	s.t.Helper()
	s.goTo(adminURL() + "/apps")
	s.navigate(chromedp.Click(`form[action="/apps/`+module+`/launch"] button`, chromedp.ByQuery))
	prefix := ""
	if module == "work" {
		prefix = "/ui"
	}
	s.waitPath(prefix + "/login/ticket")
	if !strings.HasPrefix(s.rawURL(), base) {
		s.t.Fatalf("%s was launched at %s, want %s", module, s.url(), base)
	}
	s.check()
}

// open launches a module and signs in with the Continue button.
func open(s *session, module, base string) {
	s.t.Helper()
	launch(s, module, base)
	action := "/login/ticket"
	if module == "work" {
		action = "/ui/login/ticket"
	}
	s.submit(action)
	if strings.Contains(s.path(), "/login") {
		s.t.Fatalf("%s: still not signed in after Continue: %.400s", module, s.text())
	}
}

var skipCrawl = regexp.MustCompile(`/(logout|login|content|exports/|document|resume|certificate|html$|feed|sitemap)|\.(csv|xml|json|ics|txt)(\?|$)|/(en|auth)/oidc|/static/`)

func adminPages(t *testing.T, r *run) {
	s := r.admin.in(t)
	s.goTo(adminURL() + "/overview")
	s.has("Business overview")
	for _, p := range []string{"/dashboard", "/modules", "/catalogue", "/audit", "/events", "/leads", "/email", "/settings", "/tokens", "/approvals", "/notifications", "/users", "/apps", "/site", "/site/history"} {
		s.goTo(adminURL() + p)
	}
	// An API token: the permission checkboxes, the copy-once page.
	s.goTo(adminURL() + "/tokens")
	s.fill(`form[action="/tokens"] [name="name"]`, "browser token "+r.u)
	s.fill(`form[action="/tokens"] [name="module"]`, "work")
	s.run(chromedp.Click(`form[action="/tokens"] input[value="work.read"]`, chromedp.ByQuery))
	s.submit("/tokens")
	s.has("canpat_")
	// Revoke it again: the number of live tokens per person is limited and a run can be repeated.
	var revoke string
	s.run(chromedp.Evaluate(fmt.Sprintf(`[...document.querySelectorAll('tbody tr')].find(tr => tr.textContent.includes(%q)).querySelector('form').getAttribute('action')`, "browser token "+r.u), &revoke))
	s.submit(revoke)
	// A catalogue change up to its preview (the diff is the widest thing the admin UI shows).
	s.goTo(adminURL() + "/catalogue")
	s.run(chromedp.Click(`input[name="enable"][value="equity"]`, chromedp.ByQuery))
	s.submit("/changes")
	if !strings.HasPrefix(s.path(), "/changes/") {
		t.Errorf("no change preview: %.300s", s.text())
	}
	s.goTo(adminURL() + "/overview")
	s.crawl(adminURL()+"/overview", 40, skipCrawl)
}

// overviewLinks presses every "Open <module>" button of the business overview: a launch with a
// landing place, which must exist in the module.
func overviewLinks(t *testing.T, r *run) {
	s := r.admin.in(t)
	s.goTo(adminURL() + "/overview")
	type link struct{ App, Return string }
	var links []link
	s.run(chromedp.Evaluate(`[...document.querySelectorAll('form[action$="/launch"]')]
		.map(f => ({App: f.getAttribute('action').split('/')[2], Return: f.elements['return'].value}))`, &links))
	seen := map[link]bool{}
	for _, l := range links {
		if seen[l] {
			continue
		}
		seen[l] = true
		prefix := ""
		if l.App == "work" {
			prefix = "/ui"
		}
		s.goTo(adminURL() + "/overview")
		s.navigate(chromedp.Click(`form[action="/apps/`+l.App+`/launch"]:has(input[name="return"][value="`+l.Return+`"]) button`, chromedp.ByQuery))
		s.waitPath(prefix + "/login/ticket")
		s.submit(prefix + "/login/ticket")
		if want := prefix + l.Return; l.Return != "" && l.Return != "/" && s.path() != want {
			t.Errorf("%s: the overview button for %s landed on %s", l.App, l.Return, s.path())
		}
	}
	if len(seen) == 0 {
		t.Error("the overview offers no launch button")
	}
	t.Logf("followed %d overview buttons", len(seen))
}

// launchBinding: the launch cookie binds the ticket to the browser that asked for it. The same
// address opened in another browser is refused, and a launch from a host name that shares no
// cookie with the modules is refused by can-core.
func launchBinding(t *testing.T, r *run) {
	s := r.admin.in(t)
	launch(s, "work", appURL(1))
	link := s.rawURL()
	var cookies []*network.Cookie
	s.run(chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs([]string{workURL() + "/login/ticket"}).Do(ctx)
		return err
	}))
	nonce := 0
	for _, c := range cookies {
		if strings.Contains(c.Name, "launch") {
			nonce++
			if !c.HTTPOnly || c.SameSite != network.CookieSameSiteStrict {
				t.Errorf("launch cookie %s: HttpOnly=%v SameSite=%q", c.Name, c.HTTPOnly, c.SameSite)
			}
		}
	}
	if nonce == 0 {
		t.Errorf("the browser holds no launch cookie for the work UI")
	}

	other := newSession(t, r.vp)
	other.allow(`HTTP 401|status of 401`)
	other.goTo(link)
	if other.lastStatus() != http.StatusUnauthorized || other.exists(`form[action="/ui/login/ticket"]`) {
		t.Errorf("the launch link in another browser answered HTTP %d: %.300s", other.lastStatus(), other.text())
	}

	// The admin UI by its other name: the sign-in works, a launch cannot.
	elsewhere := newSession(t, r.vp)
	elsewhere.allow(`HTTP 409|status of 409`)
	signInAt(elsewhere, host("admin"), "admin", totpSecrets["admin"])
	elsewhere.goTo(host("admin") + "/apps")
	elsewhere.click(`form[action="/apps/work/launch"] button`)
	if elsewhere.lastStatus() != http.StatusConflict {
		t.Errorf("a launch from admin.localhost answered HTTP %d: %.300s", elsewhere.lastStatus(), elsewhere.text())
	}
}

func workFlow(t *testing.T, r *run) {
	s := r.admin.in(t)
	r.key = projectKey(r.u)
	open(s, "work", appURL(1))
	createProject(s, r.key, "Browser project "+r.u)
	title := "Browser issue " + r.u + " with a rather long title that has to wrap on a phone screen"
	s.fill("#c-title", title)
	s.fill("#c-labels", "browser, averyveryveryveryveryveryverylonglabel")
	s.fill("#c-due", "2030-01-15")
	s.submit("/ui/projects/" + r.key + "/issues")
	s.has(title)
	if !strings.Contains(s.path(), "/ui/issues/") {
		s.click(`a[href^="/ui/issues/` + r.key + `-"]`)
	}
	if s.exists(`textarea[name="body"]`) {
		s.fill(`textarea[name="body"]`, "A comment from the browser with https://example.com/"+strings.Repeat("longpath/", 12))
		s.click(`form[action$="/comments"] button`)
		s.has("A comment from the browser")
	} else {
		t.Error("no comment form on the issue page")
	}
	s.goTo(workURL() + "/projects/" + r.key + "/board?sprint=all")
	s.has(r.key + "-1")
	workManage(s, r)
	s.crawl(workURL()+"/", 35, skipCrawl)
}

func peopleFlow(t *testing.T, r *run) {
	s := r.admin.in(t)
	open(s, "people", peopleURL())
	s.goTo(peopleURL() + "/goals")
	title := "Browser goal " + r.u
	s.fill("#g-title", title)
	s.fill("#g-desc", "Created by the browser suite.")
	s.fill("#g-period", "2027-Q1")
	s.submit("/goals")
	s.has(title)
	s.goTo(peopleURL() + "/me")
	s.fill("#preferred_name", "Ada")
	s.submit("/me")
	s.crawl(peopleURL()+"/", 30, skipCrawl)
}

func jobsFlow(t *testing.T, r *run) {
	s := r.admin.in(t)
	open(s, "jobs", jobsURL())
	s.goTo(jobsURL() + "/jobs/new")
	title := "Browser Engineer " + r.u
	s.fill("#f-title", title)
	s.fill("#f-dept", "Engineering")
	s.fill("#f-city", "Austin")
	s.fill("#f-country", "US")
	s.fill("#f-hm", "user:admin")
	s.fill("#sh-1", "About the role")
	s.fill("#sb-1", "Build things. See https://example.com/"+strings.Repeat("a-long-path/", 10))
	s.submit("/jobs")
	s.has(title)
	jobPath := s.path()
	s.click(`form[action="` + jobPath + `/transition"] button[value="open"]`)
	if !s.exists(`dl a[href*="/careers/"]`) {
		t.Fatalf("the open job has no careers link: %.600s", s.text())
	}
	var careers string
	s.run(chromedp.Evaluate(`document.querySelector('dl a[href*="/careers/"]').href`, &careers))
	slug := careers[strings.LastIndex(careers, "/")+1:]
	s.crawl(jobsURL()+"/", 25, skipCrawl)

	// The candidate, in a browser that has never signed in anywhere.
	c := newSession(t, r.vp)
	c.goTo(host("jobs") + "/careers")
	c.click(`a[href="/careers/` + slug + `"]`)
	c.has(title)
	resume := filepath.Join(t.TempDir(), "resume.txt")
	if err := os.WriteFile(resume, []byte("Cass Candidate\nBuilds things.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.fill(`input[name="name"]`, "Cass Candidate "+r.u)
	c.fill(`input[name="email"]`, "cand-"+r.u+"@example.test")
	c.fill(`input[name="phone"]`, "+1 555 0100")
	c.fill(`textarea[name="cover_letter"]`, "I like building things.")
	c.run(chromedp.SetUploadFiles(`input[name="resume"]`, []string{resume}, chromedp.ByQuery), chromedp.Click(`input[name="consent"]`, chromedp.ByQuery))
	time.Sleep(3 * time.Second) // a form sent at once is a bot
	c.submit("/careers/" + slug + "/apply")
	c.has("received")

	// The recruiter sees the application and downloads the resume.
	s.goTo(jobsURL() + jobPath)
	s.has("Cass Candidate " + r.u)
	s.click(`a[href^="/applications/"]`)
	s.has("Cass Candidate " + r.u)
	if s.exists(`a[href$="/resume"]`) {
		_, got := s.download(`a[href$="/resume"]`)
		if !bytes.Contains(got, []byte("Builds things.")) {
			t.Errorf("the downloaded resume is %q", got)
		}
	} else {
		t.Error("the application page offers no resume download")
	}
}

type sunk struct {
	To   []string `json:"to"`
	Data string   `json:"data"`
}

var signLink = regexp.MustCompile(`https?://[^\s<>"]+/public/sign/[A-Za-z0-9_-]+`)

// mailedLink waits for the signing link mailed to an address.
func mailedLink(t *testing.T, to string) string {
	t.Helper()
	for end := time.Now().Add(45 * time.Second); time.Now().Before(end); time.Sleep(time.Second) {
		res := web.Do(t, harness.Req{Host: "mail.localhost", Path: "/messages"}).Expect(t, "SMTP sink", 200)
		var all []sunk
		if err := json.Unmarshal(res.Body, &all); err != nil {
			t.Fatalf("sink messages: %v", err)
		}
		for _, m := range all {
			if len(m.To) != 1 || m.To[0] != to {
				continue
			}
			msg, err := mail.ReadMessage(strings.NewReader(m.Data))
			if err != nil {
				t.Fatalf("%v", err)
			}
			body := io.Reader(msg.Body)
			if strings.EqualFold(msg.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
				body = quotedprintable.NewReader(msg.Body)
			}
			b, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if l := signLink.FindString(string(b)); l != "" {
				return l
			}
		}
	}
	t.Fatalf("no signing link for %s arrived at the SMTP sink", to)
	return ""
}

func financeFlow(t *testing.T, r *run) {
	s := r.admin.in(t)
	tmpl := apiOf(t, "finance", "CAN_FINANCE_ADMIN_TOKEN").Post(t, "/v1/contract-templates", map[string]any{
		"name": "Browser services " + r.u,
		"body": "SERVICES AGREEMENT\n\nAcme Co provides {{service}}.\n\nReference https://example.com/" + strings.Repeat("terms-and-conditions-", 8) + "\n",
	}).Expect(t, "create a contract template", 201).Str("id")
	open(s, "finance", financeURL())

	s.goTo(financeURL() + "/parties/new")
	party := "Browser Customer " + r.u
	s.fill("#p-name", party)
	s.fill("#p-email", "customer-with-a-long-address-"+r.u+"@subsidiary.customer.example")
	s.fill("#p-address", "1 Long Street Name Boulevard\nSpringfield")
	s.submit("/parties")
	s.has(party)
	partyID := s.path()[strings.LastIndex(s.path(), "/")+1:]

	s.goTo(financeURL() + "/invoices/new")
	s.fill("#i-party", partyID)
	s.fill(`input[name="desc"]`, "Anvil, extra heavy, delivered")
	s.fill(`input[name="qty"]`, "2")
	s.fill(`input[name="price"]`, "49.00")
	s.submit("/invoices")
	s.has("98.00")

	s.goTo(financeURL() + "/contracts/new?template=" + tmpl + "&party=" + partyID)
	title := "Browser contract " + r.u
	s.fill("#c-title", title)
	s.fill("#v-service", "anvil maintenance")
	s.submit("/contracts")
	s.has(title)
	contract := s.path()
	signer := "signer-" + r.u + "@visitor.example"
	if !s.exists("#l-other") {
		t.Fatalf("the contract offers no signature by link: %.800s", s.text())
	}
	s.fill("#l-other", "Cass Counterparty <"+signer+">")
	s.submit(contract + "/signing")
	s.has(signer)

	// The signer: no account, a link from the mail.
	link := mailedLink(t, signer)
	c := newSession(t, r.vp)
	c.goTo(link)
	c.has("SERVICES AGREEMENT", "anvil maintenance", "Cass Counterparty")
	name, doc := c.download(`a[href$="/document"]`)
	if !strings.HasSuffix(name, ".txt") || !bytes.Contains(doc, []byte("anvil maintenance")) {
		t.Errorf("downloaded document %q: %.100q", name, doc)
	}
	c.fill("#typed", "Cass Counterparty")
	c.run(chromedp.Click(`input[name="consent"]`, chromedp.ByQuery))
	c.click(`form:has(input[value="sign"]) button`)
	c.has("Your signature is recorded")
	c.allow(`HTTP 404|status of 404`)
	c.goTo(link)
	c.has("This link does not work")

	s.goTo(financeURL() + contract)
	s.has("Cass Counterparty")
	if s.exists(`a[href$="/signing/certificate"]`) {
		s.click(`a[href$="/signing/certificate"]`)
		s.has("Cass Counterparty")
	} else {
		t.Errorf("no certificate on the completed contract: %.600s", s.text())
	}
	s.crawl(financeURL()+"/", 45, skipCrawl)
}

func filesFlow(t *testing.T, r *run) {
	s := r.admin.in(t)
	open(s, "files", filesURL())
	space := "Browser space " + r.u
	s.fill("#space-name", space)
	s.submit("/spaces")
	s.has(space)
	spacePath := s.path()

	s.click(`a[href^="` + spacePath + `/upload"]`)
	content := []byte("%PDF-1.7\nbrowser upload " + r.u + "\n%%EOF\n")
	dir := t.TempDir()
	name := "a-rather-long-file-name-without-any-spaces-quarterly-report-" + r.u + ".pdf"
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	s.run(chromedp.SetUploadFiles("#file", []string{file}, chromedp.ByQuery))
	s.fill("#description", "Uploaded by the browser suite")
	s.submit(spacePath + "/files")
	s.has(name)
	if !strings.HasPrefix(s.path(), "/files/") {
		s.goTo(filesURL() + spacePath)
		s.click(`th a[href^="/files/"]`)
	}
	filePath := s.path()
	got, b := s.download(`a[href="` + filePath + `/content"]`)
	if got != name || !bytes.Equal(b, content) {
		t.Errorf("download: name %q, %d bytes; want %q, %d bytes", got, len(b), name, len(content))
	}

	// A second version, then a share link opened by somebody without an account.
	s.run(chromedp.SetUploadFiles(`form[action="`+filePath+`/versions"] input[type="file"]`, []string{file}, chromedp.ByQuery))
	s.submit(filePath + "/versions")
	s.has(name)
	if s.exists(`form[action="` + filePath + `/shares"]`) {
		s.fill("#label", "for the auditor")
		s.submit(filePath + "/shares")
		var link string
		s.run(chromedp.Evaluate(`(document.body.innerText.match(/https?:\/\/\S+\/s\/[A-Za-z0-9_.-]+/)||[""])[0]`, &link))
		if link == "" {
			t.Fatalf("no share link on the page: %.600s", s.text())
		}
		c := newSession(t, r.vp)
		c.run(chromedp.Navigate("about:blank"))
		c.drain()
		_, shared := downloadAddress(c, link)
		if !bytes.Equal(shared, content) {
			t.Errorf("the share link served %d bytes", len(shared))
		}
	} else {
		t.Logf("sharing is not published in this stack")
	}
	s.crawl(filesURL()+"/", 20, skipCrawl)
}

// leadForm sends the contact form of the compose site the way a visitor does.
func leadForm(t *testing.T, r *run) {
	c := newSession(t, r.vp)
	c.goTo("http://localhost:" + env.Port + "/")
	// The site runs no script, so its contact section links to the form the gateway serves.
	c.click(`#contact a[href="/_forms/contact/form"]`)
	email := "lead-" + r.u + "@visitor.example"
	c.fill(`input[name="name"]`, "Browser Lead")
	c.fill(`input[name="email"]`, email)
	c.fill(`textarea[name="message"]`, "Hello from a real browser.")
	time.Sleep(3 * time.Second) // min_fill_seconds
	c.click(`form button`)
	if c.path() != "/thanks/" {
		t.Fatalf("after sending the form the browser is at %s: %.400s", c.path(), c.text())
	}
	leads := harness.API{C: web, Host: "forms.localhost", Token: env.Secret(t, "forms", "CAN_FORMS_API_TOKEN")}
	if got := leads.Get(t, "/v1/leads?form=contact").Expect(t, "leads API", 200).String(); !strings.Contains(got, email) {
		t.Fatalf("the lead is not stored")
	}
	// With a trailing slash the page's relative links would break: the gateway redirects.
	c.goTo("http://localhost:" + env.Port + "/_forms/contact/form/")
	if c.path() != "/_forms/contact/form" {
		t.Fatalf("with a trailing slash the browser is at %s", c.path())
	}
	// And back to the site from the form.
	c.click(`a[href="/"]`)
	if c.path() != "/" {
		t.Fatalf("the way back leads to %s", c.path())
	}
}

// tokenLogins signs in to each module UI by pasting a token, in a fresh browser.
func tokenLogins(t *testing.T, r *run) {
	for _, m := range []struct{ id, base, key string }{
		{"work", "/ui", "CAN_WORK_ADMIN_TOKEN"}, {"people", "", "CAN_PEOPLE_API_TOKEN"}, {"jobs", "", "CAN_JOBS_ADMIN_TOKEN"},
		{"finance", "", "CAN_FINANCE_ADMIN_TOKEN"}, {"files", "", "CAN_FILES_API_TOKEN"}, {"equity", "", "CAN_EQUITY_API_TOKEN"},
	} {
		t.Run(m.id, func(t *testing.T) {
			c := newSession(t, r.vp)
			base := host(m.id) + m.base
			c.goTo(base + "/login")
			c.allow(`HTTP 401|status of 401|HTTP 403|status of 403`)
			c.fill(`input[name="token"]`, "not-a-token-0123456789abcdef0123456789")
			c.submit(m.base + "/login")
			if m.id == "people" || m.id == "equity" {
				return // these static tokens are services, not people: they have no web session
			}
			c.goTo(base + "/login")
			c.fill(`input[name="token"]`, env.Secret(t, m.id, m.key))
			c.submit(m.base + "/login")
			if strings.HasSuffix(c.path(), "/login") {
				t.Fatalf("token sign-in failed: %.300s", c.text())
			}
			c.submit(m.base + "/logout")
			if !strings.HasSuffix(c.path(), "/login") {
				t.Errorf("after logout the browser is at %s", c.path())
			}
		})
	}
}

// crossSite posts forms from another site, and from another origin of the same site, at the UIs the
// administrator is signed in to. Nothing may happen.
func crossSite(t *testing.T, r *run) {
	s := r.admin.in(t)
	targets := []string{adminURL() + "/logout", workURL() + "/logout", peopleURL() + "/logout", jobsURL() + "/logout", financeURL() + "/logout", filesURL() + "/logout", equityURL() + "/logout"}
	attacker := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><link rel="icon" href="data:,"><form method="post" action=%q><input name="csrf" value="guess"><button>go</button></form>`, r.URL.Query().Get("to"))
	}))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	attacker.Listener = l
	attacker.Start()
	defer attacker.Close()
	port := l.Addr().(*net.TCPAddr).Port
	s.allow(`HTTP 40[13]|status of 40[13]|the page answered|a11y|layout:|CookieIssue|SameSite`)
	for _, from := range []string{"127.0.0.1" /* another site */, "localhost" /* same site, other origin */} {
		for _, to := range targets {
			s.navigate(chromedp.Navigate(fmt.Sprintf("http://%s:%d/?to=%s", from, port, to)))
			s.navigate(chromedp.Click("button", chromedp.ByQuery))
			if st := s.lastStatus(); st != http.StatusForbidden && !strings.HasSuffix(s.path(), "/login") {
				t.Errorf("a post from %s to %s answered HTTP %d at %s", from, to, st, s.url())
			}
		}
	}
	s.allowed = nil
	s.drain()
	// Every session survived.
	for _, p := range []string{adminURL() + "/overview", workURL() + "/", peopleURL() + "/", jobsURL() + "/", financeURL() + "/", filesURL() + "/", equityURL() + "/"} {
		s.navigate(chromedp.Navigate(p))
		if strings.HasSuffix(s.path(), "/login") {
			t.Errorf("a cross-origin post ended the session at %s", p)
		}
	}
}

// accountSwitch: the browser still holds the admin's work session; a second administrator signs in
// to the admin UI and opens work. The module asks before replacing the other account's session.
func accountSwitch(t *testing.T, r *run) {
	s := r.admin.in(t)
	name := "switch" + r.u
	pw := env.Secret(t, "core", "CAN_ADMIN_PASSWORD")
	out := env.Run(t, "exec", "-T", "-e", "CAN_ADMIN_PASSWORD="+pw, "core", "/can-core", "admin", "bootstrap", "--username", name)
	secret := harness.ParseTOTPSecret(out)
	if secret == "" {
		t.Fatal("no TOTP secret for the second administrator")
	}
	env.Run(t, "exec", "-T", "core", "/can-core", "admin", "roles", "grant", name, "work:member")

	s.goTo(adminURL() + "/overview")
	s.submit("/logout")
	signIn(s, name, secret)
	launch(s, "work", appURL(1))
	s.has(name)
	if !s.exists("#replace") {
		t.Fatalf("no confirmation before replacing another account's session: %.600s", s.text())
	}
	// Without the confirmation the browser does not send the form.
	s.run(chromedp.Click(`form[action="/ui/login/ticket"] button`, chromedp.ByQuery))
	time.Sleep(500 * time.Millisecond)
	if s.path() != "/ui/login/ticket" {
		t.Fatalf("the form was sent without the confirmation")
	}
	s.drain()
	s.run(chromedp.Click("#replace", chromedp.ByQuery))
	s.submit("/ui/login/ticket")
	s.has(name)
}
