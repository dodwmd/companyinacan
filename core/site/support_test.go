package site

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
)

func TestSupportSection(t *testing.T) {
	render := func(y string) (string, []string) {
		t.Helper()
		var c config.Company
		if err := yaml.Unmarshal([]byte("name: Acme\ndomain: acme.example\n"+y), &c); err != nil {
			t.Fatal(err)
		}
		out, err := Render(&c, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return string(out.Files["index.html"]), out.Warnings
	}
	mail := "modules:\n  support:\n    settings: {support_address: 'help?x@acme.example'}\n"

	html, warnings := render("site:\n  support: {title: Need help, text: Write to us.}\n" + mail)
	for _, want := range []string{`<section id="support"`, "Need help", "Write to us.", `href="mailto:help%3Fx@acme.example"`, "Email support", `<a href="#support">Support</a>`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
	if strings.Contains(html, "support.acme.example") {
		t.Error("the public site must not link to the helpdesk's sign-in")
	}

	html, warnings = render("site:\n  support: {}\n")
	if strings.Contains(html, `id="support"`) || len(warnings) != 1 {
		t.Errorf("without the module the section is left out and said so: %v", warnings)
	}
	if html, _ = render(mail); strings.Contains(html, `id="support"`) {
		t.Error("the section is opt-in")
	}
}
