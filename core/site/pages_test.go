package site

import (
	"errors"
	"strings"
	"testing"
)

const pagesSite = minimal + `modules:
  jobs: {}
site:
  features: [{title: One}]
  careers: {label: Work with us}
  pages:
    - slug: privacy
      title: Privacy <policy>
      body: |
        We keep your <b>email</b>.
        On two lines.

        ## What
        - one
        - two
    - {slug: terms, title: Terms, body: Be nice.}
  footer:
    links: [{label: Features, href: "#features"}]
`

func TestPagesAndCareers(t *testing.T) {
	out := render(t, pagesSite)
	home, privacy := string(out.Files["index.html"]), string(out.Files["privacy.html"])
	for _, want := range []string{`href="/privacy">Privacy &lt;policy&gt;</a>`, `href="/terms">Terms</a>`, `href="https://careers.acme.example/careers" rel="noopener">Work with us</a>`, `href="#features"`} {
		if !strings.Contains(home, want) {
			t.Errorf("home lacks %q:\n%s", want, home)
		}
	}
	for _, want := range []string{"<h1>Privacy &lt;policy&gt;</h1>", "<p>We keep your &lt;b&gt;email&lt;/b&gt;. On two lines.</p>", "<h2>What</h2>", "<li>one</li>", `href="/#features"`,
		`<link rel="canonical" href="https://acme.example/privacy">`} {
		if !strings.Contains(privacy, want) {
			t.Errorf("privacy lacks %q:\n%s", want, privacy)
		}
	}
	if strings.Contains(privacy, `href="#features"`) || strings.Contains(privacy, "<script") {
		t.Errorf("privacy:\n%s", privacy)
	}
	if !strings.Contains(string(out.Files["sitemap.xml"]), "<loc>https://acme.example/terms</loc>") {
		t.Error("the sitemap lacks the pages")
	}

	// Without the jobs module there is no link, and a warning says why.
	off := render(t, strings.Replace(pagesSite, "  jobs: {}\n", "  jobs: {enabled: false}\n", 1))
	if strings.Contains(string(off.Files["index.html"]), "careers.acme.example") || !strings.Contains(strings.Join(off.Warnings, " "), "site.careers") {
		t.Errorf("careers without jobs: %v", off.Warnings)
	}
}

func TestPageSlugsMustBeFree(t *testing.T) {
	_, err := Render(load(t, minimal+"site:\n  pages:\n    - {slug: index, title: A, body: b}\n    - {slug: x, title: A, body: b}\n    - {slug: x, title: A, body: b}\n    - {slug: ../up, title: A, body: b}\n"), Options{})
	var se *Error
	if !errors.As(err, &se) || len(se.Problems) != 3 {
		t.Fatalf("%v", err)
	}
}

func TestPreviewIsSelfContained(t *testing.T) {
	out := renderFull(t)
	doc, hash, err := out.Preview("")
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if strings.Contains(s, "<script") || strings.Contains(s, `rel="stylesheet"`) || strings.Contains(s, `="/assets/`) || !strings.Contains(s, "<style>") || !strings.HasPrefix(hash, "'sha256-") {
		t.Errorf("%s\n%s", hash, s)
	}
	for _, p := range []string{"nope", "../index"} {
		if _, _, err := out.Preview(p); !errors.Is(err, ErrNoPage) {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestArchiveCarriesTheLogo(t *testing.T) {
	out := renderFull(t)
	a, n, err := out.Archive()
	if err != nil || n < len(out.Files) {
		t.Fatal(n, err)
	}
	b, _, err := out.Archive()
	if err != nil || string(a) != string(b) {
		t.Fatal("not deterministic", err)
	}
	name, data, err := ArchiveLogo(a)
	if err != nil || name != "assets/logo.svg" || string(data) != string(out.Files[name]) {
		t.Fatalf("%q %v", name, err)
	}
}
