package adminui

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/modmgr"
	"github.com/dodwmd/companyinacan/core/server/siteedit"
	"github.com/dodwmd/companyinacan/core/site"
)

const siteCompany = `# the company
schema: 1
name: Acme
domain: acme.example
provider:
  type: local
modules:
  web:
    enabled: true
    settings: {site_title: Acme}
site:
  tagline: Anvils, delivered. # keep this comment
  brand:
    colours: ["#b4440e"]
    logo: logo.svg
  hero:
    headline: Heavy things, handled.
    cta: { label: See pricing, href: "#features" }
  features:
    - title: Next-day delivery
`

const testLogo = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"/>`

type siteHarness struct {
	*mm
	dir string // the published site
}

// newSiteHarness has a published site (built as `can site build` would) and the editor over it.
func newSiteHarness(t *testing.T) *siteHarness {
	t.Helper()
	m := newMM(t)
	must(t, os.WriteFile(m.store.Path, []byte(siteCompany), 0o644))
	repo := t.TempDir()
	must(t, os.WriteFile(filepath.Join(repo, "logo.svg"), []byte(testLogo), 0o644))
	c, err := modmgr.ParseCompany([]byte(siteCompany))
	must(t, err)
	out, err := site.Render(c, site.Options{Assets: os.DirFS(repo)})
	must(t, err)
	h := &siteHarness{mm: m, dir: t.TempDir()}
	must(t, out.Write(h.dir, site.WriteOptions{}))
	m.ui.Site = &siteedit.Service{DB: m.db, Audit: m.audit, Company: m.store, Publisher: siteedit.DirPublisher{Dir: h.dir},
		ModulesDir: m.svc.ModulesDir, BaseURL: "https://acme.example", Now: func() time.Time { return *m.now }}
	return h
}

func (h *siteHarness) company() string {
	h.t.Helper()
	return string(mustRead(h.t, h.store.Path))
}

func (h *siteHarness) public(name string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, name))
	if os.IsNotExist(err) {
		return ""
	}
	must(h.t, err)
	return string(b)
}

var (
	reInput    = regexp.MustCompile(`<input type="(text|checkbox|hidden)"[^>]*?name="([^"]+)"[^>]*?>`)
	reValue    = regexp.MustCompile(`value="([^"]*)"`)
	reTextarea = regexp.MustCompile(`(?s)<textarea [^>]*?name="([^"]+)"[^>]*>\n(.*?)</textarea>`)
	reDraft    = regexp.MustCompile(`^/site/drafts/([0-9a-f]{24})$`)
)

// formOf is what a browser would send for the page's editor form, untouched.
func formOf(body string) url.Values {
	v := url.Values{}
	body = body[strings.Index(body, `<form method="post" action="/site"`):]
	for _, m := range reInput.FindAllStringSubmatch(body, -1) {
		if m[1] == "checkbox" && !strings.Contains(m[0], " checked") {
			continue
		}
		val := ""
		if x := reValue.FindStringSubmatch(m[0]); x != nil {
			val = html.UnescapeString(x[1])
		}
		v.Add(m[2], val)
	}
	for _, m := range reTextarea.FindAllStringSubmatch(body, -1) {
		v.Add(m[1], strings.ReplaceAll(html.UnescapeString(m[2]), "\n", "\r\n"))
	}
	return v
}

func (h *siteHarness) editor(s session) url.Values {
	h.t.Helper()
	r := h.do("GET", "/site", nil, s.with()...)
	if r.StatusCode != http.StatusOK {
		h.t.Fatalf("GET /site: %d %s", r.StatusCode, r.body)
	}
	return formOf(r.body)
}

// preview submits the form and returns the draft id.
func (h *siteHarness) preview(s session, f url.Values) string {
	h.t.Helper()
	r := h.do("POST", "/site", f, s.with()...)
	m := reDraft.FindStringSubmatch(r.Header.Get("Location"))
	if r.StatusCode != http.StatusSeeOther || m == nil {
		h.t.Fatalf("preview: %d %s", r.StatusCode, r.body)
	}
	return m[1]
}

func (h *siteHarness) publish(s session, id string) resp {
	return h.do("POST", "/site/drafts/"+id+"/publish", url.Values{"csrf": {s.csrf}}, s.with()...)
}

func TestSiteEditPreviewPublishAndRollback(t *testing.T) {
	h := newSiteHarness(t)
	s := h.loginAs("ops")

	f := h.editor(s)
	for name, want := range map[string]string{"tagline": "Anvils, delivered.", "hero.cta.href": "#features", "features.0.title": "Next-day delivery", "pages.0.slug": "privacy", "pages.1.slug": "terms"} {
		if f.Get(name) != want {
			t.Errorf("form %s = %q, want %q", name, f.Get(name), want)
		}
	}
	f.Set("hero.headline", "Heavier things <b>handled</b>")
	f.Set("pages.0.body", "We keep your email.\n\n## Contact\n- by mail\n<script>alert(1)</script>")
	f.Set("brand.colours.1", "#112233")
	id := h.preview(s, f)

	if h.company() != siteCompany || strings.Contains(h.public("index.html"), "Heavier") {
		t.Fatal("a preview changed something")
	}
	page := h.do("GET", "/site/drafts/"+id, nil, s.with()...)
	csp := page.Header.Get("Content-Security-Policy")
	if page.StatusCode != 200 || !strings.Contains(csp, "frame-src 'self'") || !strings.Contains(csp, "script-src 'none'") {
		t.Fatalf("draft page: %d %q", page.StatusCode, csp)
	}
	if !strings.Contains(page.body, `<iframe class="preview" name="preview" sandbox=""`) || !strings.Contains(page.body, "?page=privacy") {
		t.Errorf("no sandboxed frame or page list:\n%s", page.body)
	}
	if p := a11yProblems(page.body); len(p) > 0 {
		t.Errorf("a11y: %v", p)
	}
	for _, q := range []string{"", "?page=privacy"} {
		fr := h.do("GET", "/site/drafts/"+id+"/frame"+q, nil, s.with()...)
		csp = fr.Header.Get("Content-Security-Policy")
		if fr.StatusCode != 200 || !strings.HasPrefix(csp, "sandbox;") || !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "allow-") ||
			!strings.Contains(csp, "style-src 'sha256-") || !strings.Contains(csp, "frame-ancestors 'self'") {
			t.Fatalf("frame%s: %d %q", q, fr.StatusCode, csp)
		}
		if strings.Contains(strings.ToLower(fr.body), "<script") {
			t.Errorf("frame%s holds a script element:\n%s", q, fr.body)
		}
		if !strings.Contains(fr.body, "<style>") || !strings.Contains(fr.body, "data:image/svg+xml;base64,") || strings.Contains(fr.body, `href="/privacy"`) {
			t.Errorf("frame%s is not self-contained:\n%s", q, fr.body)
		}
	}

	// Publishing needs a recent proof of identity.
	*h.now = h.now.Add(11 * time.Minute)
	if r := h.publish(s, id); r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/reauth?next=") {
		t.Fatalf("publish without step-up: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if h.company() != siteCompany {
		t.Fatal("published without a proof of identity")
	}
	s = h.loginAs("ops")
	if r := h.publish(s, id); r.StatusCode != 303 || r.Header.Get("Location") != "/site?flash=flash.site_published" {
		t.Fatalf("publish: %d %q %s", r.StatusCode, r.Header.Get("Location"), r.body)
	}
	got := h.company()
	for _, want := range []string{"# the company", "# keep this comment", "Heavier things <b>handled</b>", "logo: logo.svg", "slug: privacy", "settings: {site_title: Acme}", `"#112233"`} {
		if !strings.Contains(got, want) && !strings.Contains(got, strings.Trim(want, `"`)) {
			t.Errorf("company.yaml lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "slug: terms") {
		t.Errorf("an empty offered page was created:\n%s", got)
	}
	if i := h.public("index.html"); !strings.Contains(i, "Heavier things &lt;b&gt;handled&lt;/b&gt;") || !strings.Contains(i, `href="/privacy"`) || !strings.Contains(i, "assets/logo.svg") {
		t.Errorf("public home page:\n%s", i)
	}
	if p := h.public("privacy.html"); !strings.Contains(p, "<h2>Contact</h2>") || !strings.Contains(p, "<li>by mail") || strings.Contains(p, "<script>alert") {
		t.Errorf("privacy page:\n%s", p)
	}
	if h.public("assets/logo.svg") != testLogo {
		t.Error("the logo was lost")
	}
	if r := h.do("GET", "/site", nil, s.with()...); !strings.Contains(r.body, "The public site matches this content.") {
		t.Errorf("live state not shown:\n%s", r.body)
	}
	audit := auditActionsOf(t, h.mm)
	if !strings.Contains(audit, "site.publish") {
		t.Errorf("not audited: %s", audit)
	}
	// A repeated click does nothing.
	if r := h.publish(s, id); r.StatusCode != 303 {
		t.Errorf("republish click: %d", r.StatusCode)
	}

	// Saving the unchanged form again leaves the document byte for byte.
	before := h.company()
	again := h.preview(s, h.editor(s))
	if r := h.publish(s, again); r.StatusCode != 303 {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	if h.company() != before {
		t.Errorf("an unchanged form rewrote company.yaml:\n%s\nwas:\n%s", h.company(), before)
	}

	// History: the content found before the first publish can be brought back.
	hist := h.do("GET", "/site/history", nil, s.with()...)
	revs := regexp.MustCompile(`action="/site/history/(\d+)/restore"`).FindAllStringSubmatch(hist.body, -1)
	if hist.StatusCode != 200 || len(revs) != 1 || !strings.Contains(hist.body, "Found in company.yaml") {
		t.Fatalf("history: %d %s", hist.StatusCode, hist.body)
	}
	if p := a11yProblems(hist.body); len(p) > 0 {
		t.Errorf("a11y: %v", p)
	}
	r := h.do("POST", "/site/history/"+revs[0][1]+"/restore", url.Values{"csrf": {s.csrf}}, s.with()...)
	m := reDraft.FindStringSubmatch(r.Header.Get("Location"))
	if m == nil {
		t.Fatalf("restore: %d %s", r.StatusCode, r.body)
	}
	if h.company() != before {
		t.Fatal("restore published without a preview")
	}
	if r := h.publish(s, m[1]); r.StatusCode != 303 {
		t.Fatalf("rollback: %d %s", r.StatusCode, r.body)
	}
	if got := h.company(); strings.Contains(got, "Heavier") || strings.Contains(got, "slug: privacy") || !strings.Contains(got, "Heavy things, handled.") {
		t.Errorf("not rolled back:\n%s", got)
	}
	if h.public("privacy.html") != "" || !strings.Contains(h.public("index.html"), "Heavy things, handled.") {
		t.Error("the public site was not rolled back")
	}
}

func TestSiteValidationIsPerField(t *testing.T) {
	h := newSiteHarness(t)
	s := h.loginAs("ops")
	f := h.editor(s)
	f.Set("brand.colours.0", "orange")
	f.Set("hero.cta.href", "javascript:alert(1)")
	// The second row is submitted while the first is removed: errors stay on their row.
	f.Set("features.0.remove", "on")
	f.Set("features.1.description", "no title")
	f.Set("faq.0.question", "Why?")
	f.Set("pages.0.slug", "index")
	f.Set("pages.0.body", "text")
	r := h.do("POST", "/site", f, s.with()...)
	if r.StatusCode != http.StatusBadRequest || h.company() != siteCompany {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	for _, id := range []string{"f-brand-colours-0", "f-features-1-title", "f-faq-0-answer"} {
		if !strings.Contains(r.body, `href="#`+id+`"`) {
			t.Errorf("no error for %s in the summary:\n%s", id, r.body)
		}
	}
	if !strings.Contains(r.body, `value="javascript:alert(1)"`) {
		t.Error("what was typed is lost")
	}
	if p := a11yProblems(r.body); len(p) > 0 {
		t.Errorf("a11y: %v", p)
	}
	// Errors only the renderer finds, once the schema is satisfied.
	f = h.editor(s)
	f.Set("hero.cta.href", "javascript:alert(1)")
	f.Set("pages.0.slug", "index")
	f.Set("pages.0.body", "text")
	r = h.do("POST", "/site", f, s.with()...)
	for _, id := range []string{"f-hero-cta-href", "f-pages-0-slug"} {
		if !strings.Contains(r.body, `href="#`+id+`"`) {
			t.Errorf("no error for %s:\n%s", id, r.body)
		}
	}
}

func TestSiteConcurrentEditsAreNotOverwritten(t *testing.T) {
	h := newSiteHarness(t)
	s := h.loginAs("ops")
	f := h.editor(s)
	f.Set("tagline", "from the admin UI")
	id := h.preview(s, f)

	// `can apply` changes the website content meanwhile.
	cli := strings.Replace(siteCompany, "Heavy things, handled.", "Changed with the CLI", 1)
	must(t, os.WriteFile(h.store.Path, []byte(cli), 0o644))
	if r := h.publish(s, id); r.StatusCode != http.StatusConflict {
		t.Fatalf("publish over a CLI change: %d %s", r.StatusCode, r.body)
	}
	if r := h.do("POST", "/site", f, s.with()...); r.StatusCode != http.StatusConflict || !strings.Contains(r.body, "from the admin UI") {
		t.Fatalf("a stale form: %d %s", r.StatusCode, r.body)
	}
	if r := h.do("GET", "/site/drafts/"+id, nil, s.with()...); !strings.Contains(r.body, "cannot be published") || strings.Contains(r.body, `/publish"`) {
		t.Errorf("a stale draft offers to publish:\n%s", r.body)
	}
	if h.company() != cli {
		t.Fatalf("the CLI's change was overwritten:\n%s", h.company())
	}

	// A change elsewhere in company.yaml is kept.
	f = h.editor(s)
	f.Set("tagline", "from the admin UI")
	id = h.preview(s, f)
	other := strings.Replace(cli, "site_title: Acme", "site_title: Other", 1)
	must(t, os.WriteFile(h.store.Path, []byte(other), 0o644))
	if r := h.publish(s, id); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	if got := h.company(); !strings.Contains(got, "site_title: Other") || !strings.Contains(got, "from the admin UI") || !strings.Contains(got, "Changed with the CLI") {
		t.Errorf("merge lost something:\n%s", got)
	}
}

// failingPublisher cannot replace the site until told to.
type failingPublisher struct {
	siteedit.DirPublisher
	fail *bool
}

func (p failingPublisher) Publish(ctx context.Context, prev siteedit.Published, r siteedit.Release) error {
	if *p.fail {
		return context.DeadlineExceeded
	}
	return p.DirPublisher.Publish(ctx, prev, r)
}

func TestSitePublishCanBeRetried(t *testing.T) {
	h := newSiteHarness(t)
	fail := true
	h.ui.Site.Publisher = failingPublisher{siteedit.DirPublisher{Dir: h.dir}, &fail}
	s := h.loginAs("ops")
	f := h.editor(s)
	f.Set("tagline", "second try")
	id := h.preview(s, f)
	if r := h.publish(s, id); r.StatusCode != http.StatusBadGateway || !strings.Contains(h.company(), "second try") {
		t.Fatalf("%d %s", r.StatusCode, r.body)
	}
	if !strings.Contains(auditActionsOf(t, h.mm), "site.publish_failed") {
		t.Error("the failure is not audited")
	}
	fail = false
	if r := h.publish(s, id); r.StatusCode != http.StatusSeeOther || !strings.Contains(h.public("index.html"), "second try") {
		t.Fatalf("retry: %d %s", r.StatusCode, r.body)
	}
	revs, err := h.ui.Site.History(context.Background())
	must(t, err)
	if len(revs) != 2 || revs[0].Actor != "ops" || !revs[0].Current || revs[1].Kind != siteedit.KindExternal {
		t.Errorf("history after a retried publish: %+v", revs)
	}
}

func TestSiteRoutesAreGatedAndNeedCSRF(t *testing.T) {
	h := newSiteHarness(t)
	users := map[string]session{}
	for _, u := range []string{"root", "ops", "viewer", "nobody"} {
		users[u] = h.loginAs(u)
	}
	f := h.editor(users["ops"])
	f.Set("tagline", "x")
	id := h.preview(users["ops"], f)
	for _, route := range Routes() {
		method, pattern, _ := strings.Cut(route, " ")
		if pattern != "/site" && !strings.HasPrefix(pattern, "/site/") {
			continue
		}
		path := strings.NewReplacer("{id}", id, "{rev}", "1").Replace(pattern)
		for _, u := range []string{"viewer", "nobody"} {
			var form url.Values
			if method == "POST" {
				form = url.Values{"csrf": {users[u].csrf}}
			}
			if r := h.do(method, path, form, users[u].with()...); r.StatusCode != http.StatusForbidden {
				t.Errorf("%s as %s: %d, want 403", route, u, r.StatusCode)
			}
		}
		if method == "POST" {
			for _, form := range []url.Values{{}, {"csrf": {"nope"}}} {
				if r := h.do(method, path, form, users["root"].with()...); r.StatusCode != http.StatusForbidden {
					t.Errorf("%s without the CSRF token: %d, want 403", route, r.StatusCode)
				}
			}
		}
	}
	if h.company() != siteCompany {
		t.Error("a refused request changed company.yaml")
	}
	if r := h.do("GET", "/site", nil, users["viewer"].with()...); strings.Contains(r.body, `href="/site"`) {
		t.Error("the navigation offers the editor to someone who may not use it")
	}
	for _, p := range []string{"/site/drafts/" + strings.Repeat("0", 24), "/site/drafts/zz/frame", "/site/drafts/" + id + "/frame?page=nope"} {
		if r := h.do("GET", p, nil, users["ops"].with()...); r.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, r.StatusCode)
		}
	}
	h.ui.Site = nil
	if r := h.do("GET", "/site", nil, users["root"].with()...); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("without a site: %d", r.StatusCode)
	}
}

func TestSiteFormRoundTrip(t *testing.T) {
	c, err := modmgr.ParseCompany(mustRead(t, "../../../testdata/valid/company-site-full.yaml"))
	must(t, err)
	got := valuesOf(c.Site).parse(c.Site.Template).site
	got.Brand.Logo = c.Site.Brand.Logo // not a field of the form
	if len(c.Site.Features) == 0 || len(c.Site.Footer.Links) == 0 {
		t.Fatal("the fixture covers too little")
	}
	if site.SectionHash(got) != site.SectionHash(c.Site) {
		t.Errorf("the form loses content:\n got %#v\nwant %#v", got, c.Site)
	}
}
