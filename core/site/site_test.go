package site

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

const fixtures = "../../testdata"

func load(t *testing.T, src string) *config.Company {
	t.Helper()
	doc, ps := config.LoadBytes("company.yaml", []byte(src))
	if doc == nil {
		t.Fatalf("parse: %v", ps)
	}
	c, err := config.DecodeCompany(doc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func render(t *testing.T, src string) *Output {
	t.Helper()
	out, err := Render(load(t, src), Options{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

const minimal = "schema: 1\nname: Acme Co\ndomain: acme.example\nprovider: {type: local}\n"

func fullCompany(t *testing.T) *config.Company {
	t.Helper()
	doc, ps := config.Load(filepath.Join(fixtures, "valid", "company-site-full.yaml"))
	if doc == nil {
		t.Fatalf("load: %v", ps)
	}
	c, err := config.DecodeCompany(doc)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func renderFull(t *testing.T) *Output {
	t.Helper()
	out, err := Render(fullCompany(t), Options{Assets: os.DirFS(filepath.Join(fixtures, "site"))})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGolden(t *testing.T) {
	out := renderFull(t)
	dir := filepath.Join(fixtures, "site", "golden")
	if *update {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		for _, p := range out.Paths() {
			dst := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, out.Files[p], 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	var want []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		want = append(want, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := out.Paths()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("file set differs\n got: %v\nwant: %v (run go test ./core/site -update)", got, want)
	}
	for _, p := range got {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, out.Files[p]) {
			t.Errorf("%s differs from golden (run go test ./core/site -update and review the diff)", p)
		}
	}
}

func TestDeterministic(t *testing.T) {
	a, b := renderFull(t), renderFull(t)
	if strings.Join(a.Paths(), ",") != strings.Join(b.Paths(), ",") {
		t.Fatal("file sets differ between runs")
	}
	for _, p := range a.Paths() {
		if !bytes.Equal(a.Files[p], b.Files[p]) {
			t.Errorf("%s differs between runs", p)
		}
	}
}

func TestExpectedFiles(t *testing.T) {
	out := renderFull(t)
	for _, p := range []string{"index.html", "404.html", "robots.txt", "sitemap.xml"} {
		if _, ok := out.Files[p]; !ok {
			t.Errorf("missing %s", p)
		}
	}
	css := ""
	for p := range out.Files {
		if strings.HasPrefix(p, "assets/site.") && strings.HasSuffix(p, ".css") {
			css = p
		}
	}
	if css == "" {
		t.Fatal("no hashed stylesheet")
	}
	if !strings.Contains(string(out.Files["index.html"]), "/"+css) {
		t.Errorf("index.html does not reference %s", css)
	}
	if _, ok := out.Files["assets/logo.svg"]; !ok {
		t.Error("logo not copied to assets/logo.svg")
	}
}

func TestSEO(t *testing.T) {
	out := renderFull(t)
	html := string(out.Files["index.html"])
	for _, want := range []string{
		`<title>Acme Co - Anvils, delivered.</title>`,
		`<meta name="description" content="Acme Co ships industrial-grade anvils to your door, fast.">`,
		`<link rel="canonical" href="https://acme.example/">`,
		`<meta property="og:title" content="Acme Co - Anvils, delivered.">`,
		`<meta property="og:type" content="website">`,
		`<meta property="og:url" content="https://acme.example/">`,
		`<meta property="og:image" content="https://acme.example/assets/share.png">`,
		`<meta name="twitter:card" content="summary_large_image">`,
		`<meta name="viewport" content="width=device-width, initial-scale=1">`,
		`<meta name="color-scheme" content="light dark">`,
		`<html lang="en">`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %s", want)
		}
	}
	if strings.Contains(html, `name="robots"`) {
		t.Error("indexable site must not emit a robots meta tag")
	}

	robots := string(out.Files["robots.txt"])
	if !strings.Contains(robots, "User-agent: *") || !strings.Contains(robots, "Sitemap: https://acme.example/sitemap.xml") {
		t.Errorf("robots.txt: %q", robots)
	}

	var sm struct {
		URLs []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal(out.Files["sitemap.xml"], &sm); err != nil {
		t.Fatalf("sitemap.xml: %v", err)
	}
	if len(sm.URLs) != 1 || sm.URLs[0].Loc != "https://acme.example/" {
		t.Errorf("sitemap urls: %+v", sm.URLs)
	}
}

var ldRe = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)

func TestJSONLD(t *testing.T) {
	html := string(renderFull(t).Files["index.html"])
	m := ldRe.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("no JSON-LD block")
	}
	var ld map[string]any
	if err := json.Unmarshal([]byte(m[1]), &ld); err != nil {
		t.Fatalf("JSON-LD does not parse: %v\n%s", err, m[1])
	}
	if ld["@context"] != "https://schema.org" || ld["@type"] != "Organization" || ld["name"] != "Acme Co" || ld["url"] != "https://acme.example/" {
		t.Errorf("JSON-LD: %v", ld)
	}
	if ld["logo"] != "https://acme.example/assets/logo.svg" {
		t.Errorf("logo: %v", ld["logo"])
	}
	same, _ := ld["sameAs"].([]any)
	if len(same) != 2 {
		t.Errorf("sameAs: %v", ld["sameAs"])
	}
}

func TestSectionsAndAccessibility(t *testing.T) {
	html := string(renderFull(t).Files["index.html"])
	for _, id := range []string{`id="features"`, `id="pricing"`, `id="faq"`, `id="contact"`} {
		if !strings.Contains(html, id) {
			t.Errorf("missing section %s", id)
		}
	}
	if n := strings.Count(html, "<h1"); n != 1 {
		t.Errorf("want exactly one h1, got %d", n)
	}
	for _, want := range []string{`<main id="main">`, `class="skip"`, `<nav`, `<footer`, `<details`, `aria-labelledby`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(html, "<script src") || regexp.MustCompile(`<script(?: [^>]*)?>`).FindAllString(html, -1)[0] != `<script type="application/ld+json">` {
		t.Error("the page must not load or embed any JavaScript")
	}
	if strings.Contains(html, " style=") {
		t.Error("inline style attributes are forbidden (strict CSP)")
	}
	if strings.Count(html, `class="plan highlighted"`) != 1 {
		t.Error("exactly one highlighted plan expected")
	}
}

func TestCSS(t *testing.T) {
	out := renderFull(t)
	var css string
	for p, b := range out.Files {
		if strings.HasSuffix(p, ".css") {
			css = string(b)
		}
	}
	for _, want := range []string{"prefers-color-scheme: dark", "prefers-reduced-motion", "@media (min-width", "#b4440e", ":focus-visible"} {
		if !strings.Contains(css, want) {
			t.Errorf("css missing %q", want)
		}
	}
	if strings.Contains(css, "@import") || strings.Contains(css, "url(http") {
		t.Error("css must not pull external resources")
	}
}

func TestMinimalSite(t *testing.T) {
	out := render(t, minimal)
	html := string(out.Files["index.html"])
	if !strings.Contains(html, "<title>Acme Co</title>") || !strings.Contains(html, "<h1") {
		t.Errorf("minimal page malformed:\n%s", html)
	}
	for _, id := range []string{`id="features"`, `id="pricing"`, `id="faq"`, `id="contact"`, "application/ld+json\">{\"@context\":\"https://schema.org\",\"@type\":\"Organization\",\"name\":\"Acme Co\",\"url\":\"https://acme.example/\"}"} {
		has := strings.Contains(html, id)
		if strings.HasPrefix(id, "id=") && has {
			t.Errorf("empty site must not render %s", id)
		}
		if !strings.HasPrefix(id, "id=") && !has {
			t.Errorf("minimal JSON-LD missing: %s", id)
		}
	}
}

func TestNoindex(t *testing.T) {
	out := render(t, minimal+"site: {noindex: true}\n")
	if !strings.Contains(string(out.Files["index.html"]), `<meta name="robots" content="noindex, nofollow">`) {
		t.Error("noindex meta missing")
	}
	if !strings.Contains(string(out.Files["robots.txt"]), "Disallow: /") || strings.Contains(string(out.Files["robots.txt"]), "Sitemap:") {
		t.Errorf("robots.txt: %q", out.Files["robots.txt"])
	}
	if _, ok := out.Files["sitemap.xml"]; ok {
		t.Error("noindex sites must not publish a sitemap")
	}
	if !strings.Contains(string(out.Files["404.html"]), `noindex`) {
		t.Error("404 page must always be noindex")
	}
}

func TestBaseURLOverride(t *testing.T) {
	out, err := Render(load(t, minimal), Options{BaseURL: "http://localhost:8080/"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out.Files["index.html"]), `<link rel="canonical" href="http://localhost:8080/">`) {
		t.Error("base URL not applied")
	}
}

// HTML escaping: hostile content must never become markup.
func TestEscaping(t *testing.T) {
	const evil = `<script>alert(1)</script>`
	src := `schema: 1
name: "Evil <b>Co</b> & \"Sons\""
domain: acme.example
provider: {type: local}
site:
  tagline: "` + evil + `"
  description: "\"><img src=x onerror=alert(1)>"
  hero:
    headline: "` + evil + `"
    subheadline: "</p><script>alert(2)</script>"
    cta: {label: "` + evil + `", href: "/ok?a=1&b=<x>"}
  features:
    - {title: "` + evil + `", description: "<img src=x onerror=alert(3)>"}
  pricing:
    plans:
      - {name: "` + evil + `", price: "<i>$1</i>", features: ["` + evil + `"]}
  faq:
    - {question: "` + evil + `", answer: "<iframe src=//evil></iframe>"}
  cta: {headline: "` + evil + `", button: {label: x, href: "#a"}}
  footer:
    text: "` + evil + `"
    links: [{label: "` + evil + `", href: "/x"}]
`
	out := render(t, src)
	for p, b := range out.Files {
		if !strings.HasSuffix(p, ".html") {
			continue
		}
		s := string(b)
		for _, bad := range []string{"<script>alert", "<img src=x", "<iframe", "<b>Co", "<i>$1"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains unescaped %q", p, bad)
			}
		}
		if !strings.Contains(s, "&lt;script&gt;alert(1)&lt;/script&gt;") && p == "index.html" {
			t.Errorf("%s: expected escaped payload", p)
		}
		if got := len(regexp.MustCompile(`<script`).FindAllString(s, -1)); got > 1 {
			t.Errorf("%s: %d <script elements, only the JSON-LD block is allowed", p, got)
		}
	}
	m := ldRe.FindStringSubmatch(string(out.Files["index.html"]))
	if m == nil {
		t.Fatal("no JSON-LD")
	}
	if strings.Contains(m[1], "<") || strings.Contains(m[1], ">") {
		t.Errorf("JSON-LD contains raw angle brackets: %s", m[1])
	}
	var ld map[string]any
	if err := json.Unmarshal([]byte(m[1]), &ld); err != nil || ld["name"] != `Evil <b>Co</b> & "Sons"` {
		t.Errorf("JSON-LD round trip: %v %v", err, ld["name"])
	}
	css := ""
	for p, b := range out.Files {
		if strings.HasSuffix(p, ".css") {
			css = string(b)
		}
	}
	if strings.Contains(css, "alert") {
		t.Error("content leaked into css")
	}
}

func TestRejectsBadInput(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"unknown template":  {minimal + "site: {template: nope}\n", "template"},
		"js href":           {minimal + "site: {hero: {headline: h, cta: {label: x, href: \"javascript:alert(1)\"}}}\n", "href"},
		"data href":         {minimal + "site: {cta: {headline: h, button: {label: x, href: \"data:text/html,x\"}}}\n", "href"},
		"protocol-relative": {minimal + "site: {cta: {headline: h, button: {label: x, href: \"//evil.example\"}}}\n", "href"},
		"css injection":     {minimal + "site: {brand: {colours: [\"#fff;} body{display:none\"]}}\n", "colour"},
		"short colour":      {minimal + "site: {brand: {colours: [\"#fff\"]}}\n", "colour"},
		"logo traversal":    {minimal + "site: {brand: {logo: ../../etc/passwd}}\n", "logo"},
		"logo absolute":     {minimal + "site: {brand: {logo: /etc/passwd}}\n", "logo"},
		"logo bad ext":      {minimal + "site: {brand: {logo: logo.exe}}\n", "logo"},
		"logo missing":      {minimal + "site: {brand: {logo: nothere.svg}}\n", "logo"},
		"bad lang":          {minimal + "site: {lang: \"en\\\"><x\"}\n", "lang"},
		"bad same_as":       {minimal + "site: {same_as: [\"javascript:x\"]}\n", "same_as"},
		"empty domain":      {"schema: 1\nname: A\ndomain: \"\"\nprovider: {type: local}\n", "domain"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Render(load(t, tc.src), Options{Assets: os.DirFS(filepath.Join(fixtures, "site"))})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

func TestAllProblemsReportedTogether(t *testing.T) {
	_, err := Render(load(t, minimal+"site: {template: nope, lang: \"!!\", brand: {colours: [red]}}\n"), Options{})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, w := range []string{"template", "lang", "colour"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("missing %q in %q", w, err)
		}
	}
}

func TestHrefAcceptance(t *testing.T) {
	for _, h := range []string{"#faq", "/path?x=1", "https://a.example/x", "http://a.example", "mailto:a@b.example"} {
		if !safeHref(h) {
			t.Errorf("%q should be accepted", h)
		}
	}
	for _, h := range []string{"", "javascript:alert(1)", " javascript:alert(1)", "JaVaScRiPt:x", "data:text/html,x", "//evil.example", "/\\evil.example", "vbscript:x", "ftp://x", "https://a b", "/a\nb"} {
		if safeHref(h) {
			t.Errorf("%q should be rejected", h)
		}
	}
}

func TestContrastText(t *testing.T) {
	if contrastText("#ffffff") != "#000000" || contrastText("#000000") != "#ffffff" || contrastText("#b4440e") != "#ffffff" {
		t.Error("unexpected contrast choices")
	}
}

func TestFaqIsNativeDetails(t *testing.T) {
	html := string(renderFull(t).Files["index.html"])
	if strings.Count(html, "<details") != 2 || strings.Count(html, "<summary") != 2 {
		t.Error("each FAQ entry should be a details/summary pair")
	}
}

func TestPathsSorted(t *testing.T) {
	p := renderFull(t).Paths()
	for i := 1; i < len(p); i++ {
		if p[i-1] >= p[i] {
			t.Fatalf("paths not sorted: %v", p)
		}
	}
}
