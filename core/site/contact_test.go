package site

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

func contactCompany(forms *config.ModuleEntry) *config.Company {
	c := &config.Company{Schema: 1, Name: "Acme Co", Domain: "acme.example"}
	c.Site.Contact = &config.SiteContact{}
	if forms != nil {
		c.Modules = map[string]config.ModuleEntry{"forms": *forms}
	}
	return c
}

func TestContactSectionLinksToTheHostedForm(t *testing.T) {
	c := contactCompany(&config.ModuleEntry{})
	out, err := Render(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	page := string(out.Files["index.html"])
	for _, want := range []string{
		`<section id="contact"`, `<a class="btn" href="/_forms/contact/form">Send us a message</a>`,
		`>Contact us</h2>`, `<a href="#contact">Contact</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("lacks %q:\n%s", want, page)
		}
	}
	// The site runs no script and frames nothing (apart from the JSON-LD data block).
	if strings.Contains(page, "<iframe") || strings.Contains(page, "<form") || strings.Count(page, "<script") != strings.Count(page, `<script type="application/ld+json">`) {
		t.Errorf("unexpected script, frame or form:\n%s", page)
	}
	if len(out.Warnings) != 0 {
		t.Errorf("warnings: %v", out.Warnings)
	}
}

func TestContactSectionBesideACTA(t *testing.T) {
	c := contactCompany(&config.ModuleEntry{})
	c.Site.CTA = &config.SiteCTA{Headline: "Go", Button: config.SiteLink{Label: "Mail", Href: "mailto:a@acme.example"}}
	c.Site.Contact = &config.SiteContact{Title: "Talk <to> us", Button: "Write", Form: "sales"}
	out, err := Render(c, Options{Forms: []config.HostedForm{{ID: "sales"}}})
	if err != nil {
		t.Fatal(err)
	}
	page := string(out.Files["index.html"])
	if strings.Count(page, `id="contact"`) != 1 || !strings.Contains(page, `<section id="cta"`) {
		t.Errorf("anchors:\n%s", page)
	}
	if !strings.Contains(page, `href="/_forms/sales/form">Write</a>`) || !strings.Contains(page, "Talk &lt;to&gt; us") {
		t.Errorf("section:\n%s", page)
	}
	if strings.Count(page, `<a href="#contact">Contact</a>`) != 1 {
		t.Errorf("one Contact entry in the navigation:\n%s", page)
	}

	// Without a contact section the CTA keeps the #contact anchor.
	c.Site.Contact = nil
	if out, err = Render(c, Options{}); err != nil || !strings.Contains(string(out.Files["index.html"]), `<section id="contact"`) {
		t.Fatalf("CTA anchor: %v", err)
	}
}

func TestContactSectionWithoutForms(t *testing.T) {
	off, no := false, false
	for name, tc := range map[string]struct {
		forms  *config.ModuleEntry
		absent config.AbsentForms
		shown  bool
		warns  bool
	}{
		"disabled":          {forms: &config.ModuleEntry{Enabled: &off}, warns: true},
		"not public":        {forms: &config.ModuleEntry{Expose: &config.Expose{Public: &no}}, warns: true},
		"not listed":        {shown: true, warns: true},
		"added by can up":   {absent: config.AbsentAdded, shown: true},
		"no default bundle": {absent: config.AbsentOff, warns: true},
	} {
		out, err := Render(contactCompany(tc.forms), Options{AbsentForms: tc.absent})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		page := string(out.Files["index.html"])
		if got := strings.Contains(page, "/_forms/"); got != tc.shown {
			t.Errorf("%s: link shown = %v", name, got)
		}
		if !tc.shown && strings.Contains(page, `href="#contact"`) {
			t.Errorf("%s: the navigation still points at the missing section", name)
		}
		if got := len(out.Warnings) == 1; got != tc.warns {
			t.Errorf("%s: warnings %v", name, out.Warnings)
		}
	}
}

func TestContactFormMustExist(t *testing.T) {
	c := contactCompany(&config.ModuleEntry{})
	c.Site.Contact.Form = "careers"
	_, err := Render(c, Options{})
	if err == nil || !strings.Contains(err.Error(), `no form "careers" (it has: contact)`) {
		t.Fatalf("unknown form: %v", err)
	}
	c.Site.Contact.Form = ""
	_, err = Render(c, Options{Forms: []config.HostedForm{{ID: "contact", Captcha: true}}})
	if err == nil || !strings.Contains(err.Error(), "CAPTCHA") {
		t.Fatalf("CAPTCHA form: %v", err)
	}
	// An empty list is not "unknown": the module serves nothing.
	if _, err = Render(c, Options{Forms: []config.HostedForm{}}); err == nil {
		t.Fatal("a module without forms was accepted")
	}
}
