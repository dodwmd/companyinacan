// Package site renders the `site:` section of company.yaml into a static website.
//
// The renderer is a pure function of the company description (and an optional assets
// directory holding the logo): no clock, no randomness, no network. The same input always
// produces byte-identical output, so builds are reviewable and cacheable. It is shared by the
// `can` CLI and, later, can-core.
package site

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	text "text/template"
	"unicode"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/routing"
)

//go:embed templates/classic/*.gohtml templates/classic/site.css
var templateFS embed.FS

// DefaultTemplate is the only built-in template.
const DefaultTemplate = "classic"

const (
	defaultAccent = "#2457c5"
	maxLogoBytes  = 1 << 20
)

var (
	colourRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	langRe   = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	slugRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`)
	logoExts = map[string]bool{".svg": true, ".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true}
)

// Options tune a render.
type Options struct {
	// BaseURL is the public origin used for canonical, Open Graph, sitemap and JSON-LD URLs.
	// Default: https://<domain>.
	BaseURL string
	// Assets holds files referenced from company.yaml (the logo), normally os.DirFS of the
	// company repository. May be nil when nothing is referenced.
	Assets fs.FS
	// Forms are the forms the forms module serves (config.FormsOfModules); nil means the shipped ones.
	Forms []config.HostedForm
	// AbsentForms says what a company.yaml without the forms module means for site.contact.
	AbsentForms config.AbsentForms
}

// Output is a rendered site: slash-separated relative paths mapped to file contents.
type Output struct {
	Files map[string][]byte
	// Warnings are things the operator should know about a build that succeeded.
	Warnings []string
}

// Paths returns the output paths in sorted order.
func (o *Output) Paths() []string {
	ps := make([]string, 0, len(o.Files))
	for p := range o.Files {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

// Error lists every problem found in the site configuration.
type Error struct{ Problems []string }

func (e *Error) Error() string { return "site: " + strings.Join(e.Problems, "; ") }

type navItem struct{ Label, Anchor string }

type page struct {
	Lang, Title, Description, Canonical, BaseURL string
	OGImage, CSS, Name, Tagline, Logo            string
	NoIndex                                      bool
	Nav                                          []navItem
	JSONLD                                       template.JS

	HeroHeadline, HeroSub string
	HeroCTA, HeroCTA2     *config.SiteLink
	Features              []config.SiteFeature
	Pricing               *config.SitePricing
	FAQ                   []config.SiteFAQ
	CTA                   *config.SiteCTA
	CTAAnchor             string
	Contact               *contactSection
	Support               *supportSection
	Footer                config.SiteFooter
	// Root prefixes the section links of the header: "" on the home and not-found pages, "/" elsewhere.
	Root    string
	Careers *config.SiteLink
	// Text is set on the extra text pages.
	Text *textPage
}

type contactSection struct{ Title, Text, Button, Href string }
type supportSection struct{ Title, Text, Button, Address, Href string }

type cssData struct {
	Accent, AccentText, Accent2, Accent2Text, Link, LinkDark string
	Pages                                                    bool
}

type orgLD struct {
	Context     string   `json:"@context"`
	Type        string   `json:"@type"`
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	Logo        string   `json:"logo,omitempty"`
	Description string   `json:"description,omitempty"`
	SameAs      []string `json:"sameAs,omitempty"`
}

type button struct {
	L         *config.SiteLink
	Secondary bool
}

func btn(l *config.SiteLink, secondary bool) button { return button{L: l, Secondary: secondary} }

func isExternal(href string) bool {
	return strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://")
}

// cssTemplate fills the brand colours into the stylesheet. Colours are validated as #rrggbb
// before they get here, so nothing else can reach the CSS.
var cssTemplate = text.Must(text.ParseFS(templateFS, "templates/classic/site.css"))

var pageTemplates = template.Must(template.New("site").Funcs(template.FuncMap{"external": isExternal, "btn": btn}).
	ParseFS(templateFS, "templates/classic/*.gohtml"))

// safeHref accepts only http(s) URLs, mailto: links, site-relative paths and fragments.
func safeHref(h string) bool {
	if h == "" || len(h) > 500 {
		return false
	}
	for _, r := range h {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	switch {
	case strings.HasPrefix(h, "#"):
		return true
	case strings.HasPrefix(h, "/"):
		return !strings.HasPrefix(h, "//") && !strings.HasPrefix(h, "/\\")
	case strings.HasPrefix(h, "mailto:"):
		return len(h) > len("mailto:")
	case isExternal(h):
		u, err := url.Parse(h)
		return err == nil && u.Host != ""
	}
	return false
}

type validator struct{ problems []string }

func (v *validator) add(format string, a ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, a...))
}

func (v *validator) link(where string, l *config.SiteLink) {
	if l == nil {
		return
	}
	if strings.TrimSpace(l.Label) == "" {
		v.add("%s.label: must not be empty", where)
	}
	if !safeHref(l.Href) {
		v.add("%s.href: %q is not an http(s) URL, mailto: link, site-relative path or fragment", where, l.Href)
	}
}

func normaliseBase(base, domain string) string {
	if base == "" {
		base = "https://" + domain
	}
	return strings.TrimRight(base, "/")
}

// Render builds the site. All configuration problems are reported together in an *Error.
func Render(c *config.Company, opts Options) (*Output, error) {
	s := c.Site
	v := &validator{}
	if strings.TrimSpace(c.Name) == "" {
		v.add("name: must not be empty")
	}
	if strings.TrimSpace(c.Domain) == "" && opts.BaseURL == "" {
		v.add("domain: must not be empty")
	}
	if s.Template != "" && s.Template != DefaultTemplate {
		v.add("site.template: unknown template %q (built in: %s)", s.Template, DefaultTemplate)
	}
	lang := s.Lang
	if lang == "" {
		lang = "en"
	} else if !langRe.MatchString(lang) {
		v.add("site.lang: %q is not a language tag such as en or pt-BR", lang)
	}
	for i, col := range s.Brand.Colours {
		if !colourRe.MatchString(col) {
			v.add("site.brand.colours[%d]: %q is not a #rrggbb colour", i, col)
		}
	}
	for i, u := range s.SameAs {
		if !strings.HasPrefix(u, "https://") || !safeHref(u) {
			v.add("site.same_as[%d]: %q is not an https URL", i, u)
		}
	}
	if og := s.OGImage; og != "" && (!safeHref(og) || (!strings.HasPrefix(og, "https://") && !strings.HasPrefix(og, "/"))) {
		v.add("site.og_image: %q must be an https URL or a site-relative path", s.OGImage)
	}
	if s.Hero != nil {
		v.link("site.hero.cta", s.Hero.CTA)
		v.link("site.hero.secondary_cta", s.Hero.SecondaryCTA)
	}
	if s.Pricing != nil {
		for i := range s.Pricing.Plans {
			v.link(fmt.Sprintf("site.pricing.plans[%d].cta", i), s.Pricing.Plans[i].CTA)
		}
	}
	if s.CTA != nil {
		v.link("site.cta.button", &s.CTA.Button)
	}
	for i := range s.Footer.Links {
		v.link(fmt.Sprintf("site.footer.links[%d]", i), &s.Footer.Links[i])
	}

	forms := opts.Forms
	if forms == nil {
		forms = config.ShippedForms()
	}
	contact := config.CheckContact(c, forms, opts.AbsentForms)
	var warnings []string
	for _, is := range contact.Issues {
		if is.Warning {
			warnings = append(warnings, is.Message)
		} else {
			v.add("%s", is.Message)
		}
	}

	supportAddress, supportWarning := config.SiteSupportAddress(c)
	if supportWarning != "" {
		warnings = append(warnings, supportWarning)
	}
	careers, careersWarning := config.CheckCareers(c)
	if careersWarning != "" {
		warnings = append(warnings, careersWarning)
	}
	for i, pg := range s.Pages {
		if !slugRe.MatchString(pg.Slug) {
			v.add("site.pages[%d].slug: %q must be lower-case letters, digits and hyphens, starting with a letter", i, pg.Slug)
		}
		if strings.TrimSpace(pg.Title) == "" {
			v.add("site.pages[%d].title: must not be empty", i)
		}
	}
	for _, is := range config.CheckPages(s.Pages) {
		v.add("site.pages[%d].slug: %s", is.Index, is.Message)
	}

	logoPath, logoData := "", []byte(nil)
	if s.Brand.Logo != "" {
		logoPath, logoData = loadLogo(v, s.Brand.Logo, opts.Assets)
	}
	if len(v.problems) > 0 {
		return nil, &Error{Problems: v.problems}
	}

	base := normaliseBase(opts.BaseURL, c.Domain)
	accent, accent2 := defaultAccent, ""
	if len(s.Brand.Colours) > 0 {
		accent = strings.ToLower(s.Brand.Colours[0])
	}
	if len(s.Brand.Colours) > 1 {
		accent2 = strings.ToLower(s.Brand.Colours[1])
	} else {
		accent2 = accent
	}

	var css bytes.Buffer
	if err := cssTemplate.Execute(&css, cssData{Accent: accent, AccentText: contrastText(accent), Accent2: accent2, Accent2Text: contrastText(accent2),
		Link: readable(accent, lightBG), LinkDark: readable(accent, darkBG), Pages: len(s.Pages) > 0}); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(css.Bytes())
	cssPath := "assets/site." + hex.EncodeToString(sum[:4]) + ".css"

	p := &page{
		Lang: lang, Name: c.Name, Tagline: s.Tagline, BaseURL: base, Canonical: base + "/",
		NoIndex: s.NoIndex, CSS: "/" + cssPath,
		Features: s.Features, Pricing: s.Pricing, FAQ: s.FAQ, CTA: s.CTA, Footer: s.Footer,
	}
	p.Title = c.Name
	if s.Tagline != "" {
		p.Title = c.Name + " - " + s.Tagline
	}
	p.Description = firstNonEmpty(s.Description, s.Tagline, c.Name)
	if logoPath != "" {
		p.Logo = "/" + logoPath
	}
	if strings.HasPrefix(s.OGImage, "/") {
		p.OGImage = base + s.OGImage
	} else {
		p.OGImage = s.OGImage
	}
	if s.Hero != nil {
		p.HeroHeadline, p.HeroSub, p.HeroCTA, p.HeroCTA2 = s.Hero.Headline, s.Hero.Subheadline, s.Hero.CTA, s.Hero.SecondaryCTA
	} else {
		p.HeroHeadline, p.HeroSub = c.Name, s.Tagline
	}
	if len(s.Features) > 0 {
		p.Nav = append(p.Nav, navItem{"Features", "features"})
	}
	if s.Pricing != nil && len(s.Pricing.Plans) > 0 {
		p.Nav = append(p.Nav, navItem{"Pricing", "pricing"})
	} else {
		p.Pricing = nil
	}
	if len(s.FAQ) > 0 {
		p.Nav = append(p.Nav, navItem{"FAQ", "faq"})
	}
	// "contact" is the anchor of the contact section when there is one, otherwise of the CTA.
	p.CTAAnchor = "contact"
	if contact.Show {
		sc := s.Contact
		p.CTAAnchor = "cta"
		p.Contact = &contactSection{
			Title: firstNonEmpty(sc.Title, "Contact us"), Text: sc.Text, Button: firstNonEmpty(sc.Button, "Send us a message"),
			// The page the forms module serves, same origin: it carries the timing token, so no script is needed.
			Href: routing.SitePrefixes["forms"] + "/" + sc.FormID() + "/form",
		}
	}
	if s.CTA != nil || p.Contact != nil {
		p.Nav = append(p.Nav, navItem{"Contact", "contact"})
	}
	if supportAddress != "" {
		ss := s.Support
		local, domain, _ := strings.Cut(supportAddress, "@")
		p.Support = &supportSection{
			Title: firstNonEmpty(ss.Title, "Support"), Text: ss.Text, Button: firstNonEmpty(ss.Button, "Email support"),
			// The local part is escaped: it may hold characters ("?", "#") that would end the address.
			Address: supportAddress, Href: "mailto:" + url.PathEscape(local) + "@" + domain,
		}
		p.Nav = append(p.Nav, navItem{"Support", "support"})
	}

	// The footer lists the extra pages, then the careers link, then the operator's own links.
	var auto []config.SiteLink
	for _, pg := range s.Pages {
		auto = append(auto, config.SiteLink{Label: pg.Title, Href: "/" + pg.Slug})
	}
	if careers {
		p.Careers = &config.SiteLink{Label: firstNonEmpty(s.Careers.Label, "Careers"), Href: "https://" + routing.Aliases["jobs"] + "." + c.Domain + "/careers"}
		auto = append(auto, *p.Careers)
	}
	if len(auto) > 0 {
		p.Footer.Links = append(auto, s.Footer.Links...)
	}

	ld := orgLD{Context: "https://schema.org", Type: "Organization", Name: c.Name, URL: base + "/",
		Description: firstNonEmpty(s.Description, s.Tagline), SameAs: s.SameAs}
	if logoPath != "" {
		ld.Logo = base + "/" + logoPath
	}
	// json.Marshal escapes <, > and & as \u00XX, so the result cannot close the script element.
	ldJSON, err := json.Marshal(ld)
	if err != nil {
		return nil, err
	}
	p.JSONLD = template.JS(ldJSON) //nolint:gosec // generated by json.Marshal, which HTML-escapes

	out := &Output{Files: map[string][]byte{cssPath: css.Bytes()}, Warnings: warnings}
	if logoPath != "" {
		out.Files[logoPath] = logoData
	}
	if out.Files["index.html"], err = execute("index", p); err != nil {
		return nil, err
	}
	for _, pg := range s.Pages {
		sp := *p
		sp.Root = "/"
		sp.Title = pg.Title + " - " + c.Name
		sp.Description = sp.Title
		sp.Canonical = base + "/" + pg.Slug
		sp.Text = &textPage{Title: pg.Title, Blocks: textBlocks(pg.Body)}
		// "#anchor" links of the footer point at sections of the home page.
		sp.Footer.Links = append([]config.SiteLink(nil), p.Footer.Links...)
		for i := range sp.Footer.Links {
			if strings.HasPrefix(sp.Footer.Links[i].Href, "#") {
				sp.Footer.Links[i].Href = "/" + sp.Footer.Links[i].Href
			}
		}
		if out.Files[pg.Slug+".html"], err = execute("textpage", &sp); err != nil {
			return nil, err
		}
	}
	nf := *p
	nf.Title = "Page not found - " + c.Name
	nf.NoIndex = true
	nf.Canonical = ""
	if out.Files["404.html"], err = execute("notfound", &nf); err != nil {
		return nil, err
	}
	if s.NoIndex {
		out.Files["robots.txt"] = []byte("User-agent: *\nDisallow: /\n")
	} else {
		out.Files["robots.txt"] = []byte("User-agent: *\nAllow: /\n\nSitemap: " + base + "/sitemap.xml\n")
		urls := "  <url><loc>" + xmlEscape(base+"/") + "</loc></url>\n"
		for _, pg := range s.Pages {
			urls += "  <url><loc>" + xmlEscape(base+"/"+pg.Slug) + "</loc></url>\n"
		}
		out.Files["sitemap.xml"] = []byte(xmlHeader + "<urlset xmlns=\"http://www.sitemaps.org/schemas/sitemap/0.9\">\n" + urls + "</urlset>\n")
	}
	return out, nil
}

const xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func execute(name string, data *page) ([]byte, error) {
	var b bytes.Buffer
	if err := pageTemplates.ExecuteTemplate(&b, name, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return b.Bytes(), nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// loadLogo validates the logo reference and reads it from the assets filesystem.
func loadLogo(v *validator, ref string, assets fs.FS) (string, []byte) {
	clean := path.Clean(ref)
	if strings.Contains(ref, "\\") || !fs.ValidPath(clean) || clean == "." {
		v.add("site.brand.logo: %q must be a relative path inside the company repository", ref)
		return "", nil
	}
	ext := strings.ToLower(path.Ext(clean))
	if !logoExts[ext] {
		v.add("site.brand.logo: %q must be one of .svg .png .jpg .jpeg .webp .gif", ref)
		return "", nil
	}
	if assets == nil {
		v.add("site.brand.logo: %q cannot be read because no assets directory was given", ref)
		return "", nil
	}
	data, err := fs.ReadFile(assets, clean)
	if err != nil {
		v.add("site.brand.logo: cannot read %q: %v", ref, err)
		return "", nil
	}
	if len(data) == 0 || len(data) > maxLogoBytes {
		v.add("site.brand.logo: %q must be between 1 byte and 1 MiB", ref)
		return "", nil
	}
	return "assets/logo" + ext, data
}
