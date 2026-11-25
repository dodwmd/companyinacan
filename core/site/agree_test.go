package site

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/schema"
)

// The JSON schema and the renderer must accept and reject the same site: content, so that a
// document that passes `can validate` can never fail `can site build` (logo files excepted:
// whether they exist is not a schema matter).
func TestSchemaAndRendererAgree(t *testing.T) {
	cases := []struct {
		name, site string
		ok         bool
	}{
		{"empty", `{}`, true},
		{"anchor", `{cta: {headline: h, button: {label: x, href: "#a"}}}`, true},
		{"relative", `{cta: {headline: h, button: {label: x, href: "/a?b=1"}}}`, true},
		{"root", `{cta: {headline: h, button: {label: x, href: "/"}}}`, true},
		{"https", `{cta: {headline: h, button: {label: x, href: "https://a.example/x"}}}`, true},
		{"http", `{cta: {headline: h, button: {label: x, href: "http://a.example"}}}`, true},
		{"mailto", `{cta: {headline: h, button: {label: x, href: "mailto:a@b.example"}}}`, true},
		{"protocol-relative", `{cta: {headline: h, button: {label: x, href: "//evil.example"}}}`, false},
		{"empty host", `{cta: {headline: h, button: {label: x, href: "https:///x"}}}`, false},
		{"query host", `{cta: {headline: h, button: {label: x, href: "https://?x"}}}`, false},
		{"backslash", `{cta: {headline: h, button: {label: x, href: "/\\evil.example"}}}`, false},
		{"javascript", `{cta: {headline: h, button: {label: x, href: "javascript:alert(1)"}}}`, false},
		{"data", `{cta: {headline: h, button: {label: x, href: "data:text/html,x"}}}`, false},
		{"space", `{cta: {headline: h, button: {label: x, href: "/a b"}}}`, false},
		{"empty mailto", `{cta: {headline: h, button: {label: x, href: "mailto:"}}}`, false},
		{"footer link bad", `{footer: {links: [{label: x, href: "//x"}]}}`, false},
		{"plan cta bad", `{pricing: {plans: [{name: a, price: b, cta: {label: x, href: "ftp://x"}}]}}`, false},
		{"og https", `{og_image: "https://a.example/i.png"}`, true},
		{"og relative", `{og_image: "/i.png"}`, true},
		{"og protocol-relative", `{og_image: "//x"}`, false},
		{"og empty host", `{og_image: "https:///x"}`, false},
		{"og http", `{og_image: "http://a.example/i.png"}`, false},
		{"template classic", `{template: classic}`, true},
		{"template other", `{template: other}`, false},
		{"lang ok", `{lang: pt-BR}`, true},
		{"lang bad", `{lang: "EN"}`, false},
		{"colour ok", `{brand: {colours: ["#aBcDeF"]}}`, true},
		{"colour short", `{brand: {colours: ["#fff"]}}`, false},
		{"same_as ok", `{same_as: ["https://a.example/p"]}`, true},
		{"same_as http", `{same_as: ["http://a.example/p"]}`, false},
		{"same_as empty host", `{same_as: ["https:///p"]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := minimal + "site: " + tc.site + "\n"
			doc, ps := config.LoadBytes("company.yaml", []byte(src))
			if doc == nil {
				t.Fatal(ps)
			}
			schemaOK := len(schema.Validate(schema.Company, doc)) == 0
			c, err := config.DecodeCompany(doc)
			if err != nil {
				t.Fatal(err)
			}
			_, rerr := Render(c, Options{})
			if schemaOK != tc.ok {
				t.Errorf("schema accepts=%v, want %v", schemaOK, tc.ok)
			}
			if (rerr == nil) != tc.ok {
				t.Errorf("renderer accepts=%v (err %v), want %v", rerr == nil, rerr, tc.ok)
			}
		})
	}
}

func TestLinkColoursStayReadable(t *testing.T) {
	for _, accent := range []string{"#ffff00", "#ffffff", "#00ff00", "#101216", "#000000", "#2457c5", "#b4440e", "#ff69b4", "#808080", "#777777"} {
		if r := contrastRatio(readable(accent, lightBG), lightBG); r < minLinkContrast {
			t.Errorf("%s on light: link contrast %.2f", accent, r)
		}
		if r := contrastRatio(readable(accent, darkBG), darkBG); r < minLinkContrast {
			t.Errorf("%s on dark: link contrast %.2f", accent, r)
		}
		if r := contrastRatio(accent, contrastText(accent)); r < minLinkContrast {
			t.Errorf("%s: button text contrast %.2f", accent, r)
		}
	}
	// A readable accent is left alone.
	if got := readable("#2457c5", lightBG); got != "#2457c5" {
		t.Errorf("readable accent changed to %s", got)
	}
}

func TestLowContrastAccentIsClampedInCSS(t *testing.T) {
	out := render(t, minimal+"site: {brand: {colours: [\"#ffff00\"]}}\n")
	for p, b := range out.Files {
		if strings.HasSuffix(p, ".css") {
			css := string(b)
			if i := strings.Index(css, "--link:"); strings.HasPrefix(css[i:], "--link: #ffff00") {
				t.Error("yellow link on a white page")
			}
			if !strings.Contains(css, "--accent: #ffff00") {
				t.Error("accent itself should be kept for fills")
			}
		}
	}
}
