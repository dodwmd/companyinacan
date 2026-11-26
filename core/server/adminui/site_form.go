package adminui

// The form of the website editor: one field per value of company.yaml's `site:` section, named by its
// path, so that a validation error lands on its field. There is no script: every list shows what it
// has and one empty entry to add another.

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/siteedit"
)

type siteField struct {
	Name, ID, Label, Value, Hint, Error string
	Kind                                string // text, textarea, check, hidden
	Max, Rows                           int
	Checked, ReadOnly                   bool
}

type siteGroup struct {
	Legend string // "" for the fields of a section that is not a list
	Fields []siteField
	Open   bool
}

type siteSection struct {
	ID, Title, Help, Error string
	Groups                 []siteGroup
}

// siteList is one repeated part of the form.
type siteList struct {
	path string
	max  int
}

var (
	listSameAs   = siteList{"same_as", 10}
	listFeatures = siteList{"features", 12}
	listPlans    = siteList{"pricing.plans", 4}
	listFAQ      = siteList{"faq", 20}
	listLinks    = siteList{"footer.links", 12}
	listPages    = siteList{"pages", 12}
)

// siteValues are the form's values by field name, as typed.
type siteValues map[string]string

func rowName(l siteList, i int, key string) string { return l.path + "." + strconv.Itoa(i) + "." + key }

// valuesOf fills the form from the saved section.
func valuesOf(c config.Site) siteValues {
	v := siteValues{}
	set := func(k, s string) {
		if s != "" {
			v[k] = s
		}
	}
	flag := func(k string, on bool) {
		if on {
			v[k] = "on"
		}
	}
	link := func(prefix string, l *config.SiteLink) {
		if l != nil {
			set(prefix+".label", l.Label)
			set(prefix+".href", l.Href)
		}
	}
	set("tagline", c.Tagline)
	set("description", c.Description)
	set("lang", c.Lang)
	set("og_image", c.OGImage)
	flag("noindex", c.NoIndex)
	set("same_as", strings.Join(c.SameAs, "\n"))
	for i, col := range c.Brand.Colours {
		if i < 2 {
			set("brand.colours."+strconv.Itoa(i), col)
		}
	}
	if h := c.Hero; h != nil {
		set("hero.headline", h.Headline)
		set("hero.subheadline", h.Subheadline)
		link("hero.cta", h.CTA)
		link("hero.secondary_cta", h.SecondaryCTA)
	}
	for i, f := range c.Features {
		set(rowName(listFeatures, i, "title"), f.Title)
		set(rowName(listFeatures, i, "description"), f.Description)
	}
	if p := c.Pricing; p != nil {
		set("pricing.title", p.Title)
		set("pricing.subtitle", p.Subtitle)
		for i, pl := range p.Plans {
			set(rowName(listPlans, i, "name"), pl.Name)
			set(rowName(listPlans, i, "price"), pl.Price)
			set(rowName(listPlans, i, "period"), pl.Period)
			set(rowName(listPlans, i, "description"), pl.Description)
			set(rowName(listPlans, i, "features"), strings.Join(pl.Features, "\n"))
			link(listPlans.path+"."+strconv.Itoa(i)+".cta", pl.CTA)
			flag(rowName(listPlans, i, "highlighted"), pl.Highlighted)
		}
	}
	for i, q := range c.FAQ {
		set(rowName(listFAQ, i, "question"), q.Question)
		set(rowName(listFAQ, i, "answer"), q.Answer)
	}
	if t := c.CTA; t != nil {
		set("cta.headline", t.Headline)
		set("cta.text", t.Text)
		link("cta.button", &t.Button)
	}
	if t := c.Contact; t != nil {
		v["contact.on"] = "on"
		set("contact.title", t.Title)
		set("contact.text", t.Text)
		set("contact.form", t.Form)
		set("contact.button", t.Button)
	}
	if t := c.Support; t != nil {
		v["support.on"] = "on"
		set("support.title", t.Title)
		set("support.text", t.Text)
		set("support.button", t.Button)
	}
	if t := c.Careers; t != nil {
		v["careers.on"] = "on"
		set("careers.label", t.Label)
	}
	set("footer.text", c.Footer.Text)
	for i := range c.Footer.Links {
		link(listLinks.path+"."+strconv.Itoa(i), &c.Footer.Links[i])
	}
	for i, p := range c.Pages {
		set(rowName(listPages, i, "slug"), p.Slug)
		set(rowName(listPages, i, "title"), p.Title)
		set(rowName(listPages, i, "body"), p.Body)
	}
	return v
}

// valuesFrom reads the submitted form: only names the form has.
func valuesFrom(f url.Values) siteValues {
	v := siteValues{}
	for k := range f {
		if k == "csrf" || k == "base" || len(k) > 60 {
			continue
		}
		s := strings.ReplaceAll(f.Get(k), "\r\n", "\n")
		if s != "" {
			v[k] = s
		}
	}
	return v
}

func (v siteValues) get(k string) string { return strings.TrimSpace(v[k]) }

func (v siteValues) lines(k string) []string {
	var out []string
	for _, l := range strings.Split(v[k], "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (v siteValues) link(prefix string) *config.SiteLink {
	l := config.SiteLink{Label: v.get(prefix + ".label"), Href: v.get(prefix + ".href")}
	if l == (config.SiteLink{}) {
		return nil
	}
	return &l
}

// rows are the form rows of a list that hold something and are not marked for removal.
func (v siteValues) rows(l siteList, keys ...string) []int {
	var out []int
	for i := 0; i < l.max; i++ {
		prefix := l.path + "." + strconv.Itoa(i)
		if v[prefix+".remove"] != "" {
			continue
		}
		for _, k := range keys {
			if v.get(prefix+"."+k) != "" {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

// siteSubmission is the section the form describes.
type siteSubmission struct {
	site config.Site
	// rows maps, per list, the position in the section to the form row it came from.
	rows map[string][]int
}

// parse builds the section from the form; template is what the form does not edit.
func (v siteValues) parse(template string) siteSubmission {
	sub := siteSubmission{rows: map[string][]int{}}
	s := &sub.site
	s.Template = template
	s.Tagline, s.Description, s.Lang, s.OGImage = v.get("tagline"), v.get("description"), v.get("lang"), v.get("og_image")
	s.NoIndex = v["noindex"] != ""
	s.SameAs = v.lines("same_as")
	for _, k := range []string{"brand.colours.0", "brand.colours.1"} {
		if c := v.get(k); c != "" {
			s.Brand.Colours = append(s.Brand.Colours, c)
		}
	}
	hero := config.SiteHero{Headline: v.get("hero.headline"), Subheadline: v.get("hero.subheadline"), CTA: v.link("hero.cta"), SecondaryCTA: v.link("hero.secondary_cta")}
	if hero.Headline != "" || hero.Subheadline != "" || hero.CTA != nil || hero.SecondaryCTA != nil {
		s.Hero = &hero
	}
	use := func(l siteList, keys ...string) []int {
		r := v.rows(l, keys...)
		sub.rows[l.path] = r
		return r
	}
	for _, i := range use(listFeatures, "title", "description") {
		s.Features = append(s.Features, config.SiteFeature{Title: v.get(rowName(listFeatures, i, "title")), Description: v.get(rowName(listFeatures, i, "description"))})
	}
	var plans []config.SitePlan
	for _, i := range use(listPlans, "name", "price", "period", "description", "features", "cta.label", "cta.href") {
		n := func(k string) string { return v.get(rowName(listPlans, i, k)) }
		plans = append(plans, config.SitePlan{Name: n("name"), Price: n("price"), Period: n("period"), Description: n("description"),
			Features: v.lines(rowName(listPlans, i, "features")), CTA: v.link(listPlans.path + "." + strconv.Itoa(i) + ".cta"),
			Highlighted: v[rowName(listPlans, i, "highlighted")] != ""})
	}
	if p := (config.SitePricing{Title: v.get("pricing.title"), Subtitle: v.get("pricing.subtitle"), Plans: plans}); p.Title != "" || p.Subtitle != "" || len(plans) > 0 {
		s.Pricing = &p
	}
	for _, i := range use(listFAQ, "question", "answer") {
		s.FAQ = append(s.FAQ, config.SiteFAQ{Question: v.get(rowName(listFAQ, i, "question")), Answer: v.get(rowName(listFAQ, i, "answer"))})
	}
	cta := config.SiteCTA{Headline: v.get("cta.headline"), Text: v.get("cta.text")}
	if b := v.link("cta.button"); b != nil {
		cta.Button = *b
	}
	if cta != (config.SiteCTA{}) {
		s.CTA = &cta
	}
	if v["contact.on"] != "" {
		s.Contact = &config.SiteContact{Title: v.get("contact.title"), Text: v.get("contact.text"), Form: v.get("contact.form"), Button: v.get("contact.button")}
	}
	if v["support.on"] != "" {
		s.Support = &config.SiteSupport{Title: v.get("support.title"), Text: v.get("support.text"), Button: v.get("support.button")}
	}
	if v["careers.on"] != "" {
		s.Careers = &config.SiteCareers{Label: v.get("careers.label")}
	}
	s.Footer.Text = v.get("footer.text")
	for _, i := range use(listLinks, "label", "href") {
		prefix := listLinks.path + "." + strconv.Itoa(i)
		s.Footer.Links = append(s.Footer.Links, config.SiteLink{Label: v.get(prefix + ".label"), Href: v.get(prefix + ".href")})
	}
	// The offered privacy and terms rows count only once they have text.
	var pages []int
	for _, i := range v.rows(listPages, "slug", "title", "body") {
		if v[rowName(listPages, i, "preset")] != "" && v.get(rowName(listPages, i, "body")) == "" {
			continue
		}
		pages = append(pages, i)
		s.Pages = append(s.Pages, config.SitePage{Slug: v.get(rowName(listPages, i, "slug")), Title: v.get(rowName(listPages, i, "title")), Body: v.get(rowName(listPages, i, "body"))})
	}
	sub.rows[listPages.path] = pages
	return sub
}

// fieldName turns the path of a validation error into the name of the form field it belongs to.
func (sub siteSubmission) fieldName(path string) string {
	for list, rows := range sub.rows {
		rest, ok := strings.CutPrefix(path, list+".")
		if !ok {
			continue
		}
		head, tail, _ := strings.Cut(rest, ".")
		i, err := strconv.Atoi(head)
		if err != nil || i < 0 || i >= len(rows) {
			return path
		}
		if list == listSameAs.path {
			return list
		}
		name := list + "." + strconv.Itoa(rows[i])
		if tail != "" {
			// An entry of a plan's feature list belongs to its text box.
			if k, _, found := strings.Cut(tail, "."); found && k == "features" {
				tail = k
			}
			name += "." + tail
		}
		return name
	}
	if strings.HasPrefix(path, "same_as.") {
		return "same_as"
	}
	return path
}

func fieldID(name string) string { return "f-" + strings.ReplaceAll(name, ".", "-") }

// siteForm lays the form out. errs are by field name; those that match no field are returned.
func (s *Server) siteForm(v siteValues, errs map[string]string, logo string) (sections []siteSection, rest []siteFieldError) {
	used := map[string]bool{}
	field := func(name, label, kind string, max int) siteField {
		f := siteField{Name: name, ID: fieldID(name), Label: label, Kind: kind, Max: max, Value: v[name], Error: errs[name]}
		f.Checked = v[name] != ""
		used[name] = true
		return f
	}
	text := func(name, label string, max int) siteField { return field(name, label, "text", max) }
	area := func(name, label string, max, rows int) siteField {
		f := field(name, label, "textarea", max)
		f.Rows = rows
		return f
	}
	check := func(name, label string) siteField { return field(name, label, "check", 0) }
	hint := func(f siteField, h string) siteField { f.Hint = h; return f }
	link := func(prefix, labelLabel, hrefLabel string) []siteField {
		return []siteField{text(prefix+".label", labelLabel, 40), hint(text(prefix+".href", hrefLabel, 500), s.msg("si.href_hint"))}
	}
	// rows adds one group per entry in use, and one empty entry while there is room.
	rows := func(l siteList, legend string, keys []string, build func(prefix string) []siteField) []siteGroup {
		last := -1
		for i := 0; i < l.max; i++ {
			for _, k := range keys {
				if v[rowName(l, i, k)] != "" {
					last = i
				}
			}
		}
		n := min(last+2, l.max)
		var out []siteGroup
		for i := 0; i < n; i++ {
			prefix := l.path + "." + strconv.Itoa(i)
			g := siteGroup{Legend: legend + " " + strconv.Itoa(i+1), Fields: build(prefix)}
			if i <= last {
				g.Fields = append(g.Fields, check(prefix+".remove", s.msg("si.remove")))
			}
			out = append(out, g)
		}
		return out
	}
	sec := func(id, title, help string, groups ...siteGroup) {
		used[id] = true
		sections = append(sections, siteSection{ID: id, Title: title, Help: help, Groups: groups, Error: errs[id]})
	}
	plain := func(f ...siteField) siteGroup { return siteGroup{Fields: f} }

	sec("general", s.msg("si.general"), "", plain(
		text("tagline", s.msg("si.tagline"), 200),
		hint(area("description", s.msg("si.description"), 300, 3), s.msg("si.description_hint")),
		hint(text("lang", s.msg("si.lang"), 35), s.msg("si.lang_hint")),
		hint(text("og_image", s.msg("si.og_image"), 500), s.msg("si.og_image_hint")),
		hint(area("same_as", s.msg("si.same_as"), 5000, 3), s.msg("si.same_as_hint")),
		check("noindex", s.msg("si.noindex")),
	))
	theme := []siteField{
		hint(text("brand.colours.0", s.msg("si.colour1"), 7), s.msg("si.colour_hint")),
		text("brand.colours.1", s.msg("si.colour2"), 7),
	}
	themeHelp := s.msg("si.logo_none")
	if logo != "" {
		themeHelp = s.msg("si.logo", logo)
	}
	sec("brand", s.msg("si.theme"), themeHelp, plain(theme...))
	heroFields := []siteField{text("hero.headline", s.msg("si.headline"), 120), area("hero.subheadline", s.msg("si.subheadline"), 300, 2)}
	heroFields = append(heroFields, link("hero.cta", s.msg("si.button_label"), s.msg("si.button_href"))...)
	heroFields = append(heroFields, link("hero.secondary_cta", s.msg("si.button2_label"), s.msg("si.button2_href"))...)
	sec("hero", s.msg("si.hero"), "", plain(heroFields...))
	sec("features", s.msg("si.features"), "", rows(listFeatures, s.msg("si.feature"), []string{"title", "description"}, func(p string) []siteField {
		return []siteField{text(p+".title", s.msg("si.title_field"), 80), area(p+".description", s.msg("si.description"), 400, 2)}
	})...)
	used["pricing.plans"] = true
	pricing := []siteGroup{plain(text("pricing.title", s.msg("si.title_field"), 80), text("pricing.subtitle", s.msg("si.subtitle"), 300))}
	pricing = append(pricing, rows(listPlans, s.msg("si.plan"), []string{"name", "price", "period", "description", "features", "cta.label", "cta.href"}, func(p string) []siteField {
		f := []siteField{text(p+".name", s.msg("si.plan_name"), 40), text(p+".price", s.msg("si.price"), 30), text(p+".period", s.msg("si.period"), 30),
			text(p+".description", s.msg("si.description"), 200), hint(area(p+".features", s.msg("si.plan_features"), 2000, 4), s.msg("si.one_per_line"))}
		f = append(f, link(p+".cta", s.msg("si.button_label"), s.msg("si.button_href"))...)
		return append(f, check(p+".highlighted", s.msg("si.highlighted")))
	})...)
	sec("pricing", s.msg("si.pricing"), "", pricing...)
	if errs["pricing.plans"] != "" {
		sections[len(sections)-1].Error = errs["pricing.plans"]
	}
	sec("faq", s.msg("si.faq"), "", rows(listFAQ, s.msg("si.question_n"), []string{"question", "answer"}, func(p string) []siteField {
		return []siteField{text(p+".question", s.msg("si.question"), 200), area(p+".answer", s.msg("si.answer"), 1000, 3)}
	})...)
	ctaFields := []siteField{text("cta.headline", s.msg("si.headline"), 120), text("cta.text", s.msg("si.text"), 300)}
	ctaFields = append(ctaFields, link("cta.button", s.msg("si.button_label"), s.msg("si.button_href"))...)
	used["cta.button"] = true
	sec("cta", s.msg("si.cta"), "", plain(ctaFields...))
	sec("contact", s.msg("si.contact"), s.msg("si.contact_help"), plain(check("contact.on", s.msg("si.show")),
		text("contact.title", s.msg("si.title_field"), 80), text("contact.text", s.msg("si.text"), 300),
		hint(text("contact.form", s.msg("si.form"), 41), s.msg("si.form_hint")), text("contact.button", s.msg("si.button_label"), 40)))
	sec("support", s.msg("si.support"), s.msg("si.support_help"), plain(check("support.on", s.msg("si.show")),
		text("support.title", s.msg("si.title_field"), 80), text("support.text", s.msg("si.text"), 300), text("support.button", s.msg("si.button_label"), 40)))
	sec("careers", s.msg("si.careers"), s.msg("si.careers_help"), plain(check("careers.on", s.msg("si.show")), text("careers.label", s.msg("si.link_label"), 40)))
	sec("pages", s.msg("si.pages"), s.msg("si.pages_help"), s.pageGroups(v, field, check)...)
	footer := []siteGroup{plain(text("footer.text", s.msg("si.text"), 200))}
	footer = append(footer, rows(listLinks, s.msg("si.link_n"), []string{"label", "href"}, func(p string) []siteField {
		return link(p, s.msg("si.link_label"), s.msg("si.link_href"))
	})...)
	sec("footer", s.msg("si.footer"), s.msg("si.footer_help"), footer...)

	for name, msg := range errs {
		if !used[name] {
			rest = append(rest, siteFieldError{Field: name, Message: msg})
		}
	}
	return sections, rest
}

// pageGroups are the extra pages: those there are, the privacy and terms pages if missing, and one
// empty entry.
func (s *Server) pageGroups(v siteValues, field func(name, label, kind string, max int) siteField, check func(name, label string) siteField) []siteGroup {
	last := -1
	slugs := map[string]bool{}
	for i := 0; i < listPages.max; i++ {
		if v[rowName(listPages, i, "preset")] != "" && v.get(rowName(listPages, i, "body")) == "" {
			continue // an offered row nobody wrote in is offered again below
		}
		for _, k := range []string{"slug", "title", "body"} {
			if v[rowName(listPages, i, k)] != "" {
				last = i
				slugs[v.get(rowName(listPages, i, "slug"))] = true
			}
		}
	}
	var offers [][2]string
	for _, o := range [][2]string{{"privacy", s.msg("si.privacy_title")}, {"terms", s.msg("si.terms_title")}} {
		if !slugs[o[0]] {
			offers = append(offers, o)
		}
	}
	offers = append(offers, [2]string{})
	var out []siteGroup
	for i := 0; i < listPages.max; i++ {
		var offer [2]string
		if i > last {
			if len(offers) == 0 {
				break
			}
			offer, offers = offers[0], offers[1:]
		}
		prefix := listPages.path + "." + strconv.Itoa(i)
		slug := field(prefix+".slug", s.msg("si.slug"), "text", 41)
		slug.Hint = s.msg("si.slug_hint")
		title := field(prefix+".title", s.msg("si.title_field"), "text", 80)
		body := field(prefix+".body", s.msg("si.body"), "textarea", 20000)
		body.Rows, body.Hint = 10, s.msg("si.body_hint")
		g := siteGroup{Legend: s.msg("si.page_n", i+1), Fields: []siteField{slug, title, body}}
		switch {
		case i <= last:
			g.Fields = append(g.Fields, check(prefix+".remove", s.msg("si.remove")))
		case offer[0] != "":
			g.Fields[0].Value, g.Fields[1].Value = offer[0], offer[1]
			g.Fields[2].Value = ""
			g.Legend = offer[1]
			g.Fields = append(g.Fields, siteField{Name: prefix + ".preset", Kind: "hidden", Value: "1"})
		default:
			for j := range g.Fields {
				g.Fields[j].Value = ""
			}
		}
		out = append(out, g)
	}
	return out
}

type siteFieldError struct{ Field, Message string }

// formErrors puts validation errors on the form fields.
func formErrors(sub siteSubmission, fe []siteedit.FieldError) map[string]string {
	out := map[string]string{}
	for _, e := range fe {
		name := sub.fieldName(e.Field)
		if _, ok := out[name]; !ok {
			out[name] = e.Message
		}
	}
	return out
}
