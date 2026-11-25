package forms

import (
	"strings"
	"testing"
	"time"
)

const minimalCfg = `
forms:
  - id: contact
    name: Contact
    fields:
      - {name: name, type: text, required: true, max_length: 100}
      - {name: email, type: email, required: true}
      - {name: topic, type: select, options: [sales, support]}
`

func TestParseConfigDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(minimalCfg))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.APIListen != ":8081" || c.PathPrefix != "/_forms" {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.MaxBodyBytes != 65536 || c.RateLimit.PerMinute != 10 || c.RateLimit.MaxTrackedIPs != 10000 {
		t.Fatalf("limit defaults wrong: %+v", c)
	}
	if c.RetentionDays != 730 {
		t.Fatalf("retention default: %d", c.RetentionDays)
	}
	f := c.Forms[0]
	if f.MinFill() != 3*time.Second {
		t.Fatalf("min fill default: %v", f.MinFill())
	}
	if f.Honeypot != "website" {
		t.Fatalf("honeypot default: %q", f.Honeypot)
	}
	if f.Fields[1].MaxLength != 254 || f.Fields[0].MaxLength != 100 {
		t.Fatalf("field max_length default: %d", f.Fields[1].MaxLength)
	}
}

func TestParseConfigExplicitZeroMinFill(t *testing.T) {
	c, err := ParseConfig([]byte(strings.Replace(minimalCfg, "    name: Contact", "    name: Contact\n    min_fill_seconds: 0", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Forms[0].MinFill() != 0 {
		t.Fatalf("explicit 0 must be kept, got %v", c.Forms[0].MinFill())
	}
}

func TestParseConfigRejects(t *testing.T) {
	cases := map[string]string{
		"unknown key":      "bogus: 1\n" + minimalCfg,
		"no forms":         "listen: ':1'\n",
		"bad form id":      strings.Replace(minimalCfg, "id: contact", "id: 'Con tact'", 1),
		"dup form id":      minimalCfg + strings.TrimPrefix(minimalCfg, "\nforms:\n"),
		"bad field type":   strings.Replace(minimalCfg, "type: email", "type: number", 1),
		"select no opts":   strings.Replace(minimalCfg, "options: [sales, support]", "options: []", 1),
		"dup field":        strings.Replace(minimalCfg, "{name: email, type: email, required: true}", "{name: name, type: text}", 1),
		"reserved field":   strings.Replace(minimalCfg, "{name: email,", "{name: _t,", 1),
		"honeypot clash":   strings.Replace(minimalCfg, "name: Contact", "name: Contact\n    honeypot: email", 1),
		"bad redirect":     strings.Replace(minimalCfg, "name: Contact", "name: Contact\n    redirect: 'https://evil.example/'", 1),
		"bad notify email": strings.Replace(minimalCfg, "name: Contact", "name: Contact\n    notify_email: [nope]", 1),
		"bad max":          strings.Replace(minimalCfg, "max_length: 100", "max_length: 100000", 1),
		"bad prefix":       "path_prefix: nope\n" + minimalCfg,
		"bad captcha":      "captcha: {provider: recaptcha}\n" + minimalCfg,
		"bad webhook":      "webhook: {url: 'ftp://x'}\n" + minimalCfg,
		"form captcha off": "captcha: {provider: none}\n" + strings.Replace(minimalCfg, "name: Contact", "name: Contact\n    captcha: true", 1),
		"min>max":          strings.Replace(minimalCfg, "{name: name, type: text, required: true, max_length: 100}", "{name: name, type: text, min_length: 200, max_length: 100}", 1),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(in)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseConfigReportsAllProblems(t *testing.T) {
	in := strings.Replace(minimalCfg, "type: email", "type: number", 1)
	in = strings.Replace(in, "id: contact", "id: BAD", 1)
	_, err := ParseConfig([]byte(in))
	if err == nil || !strings.Contains(err.Error(), "BAD") || !strings.Contains(err.Error(), "number") {
		t.Fatalf("want both problems reported, got %v", err)
	}
}

func TestFormLookup(t *testing.T) {
	c, _ := ParseConfig([]byte(minimalCfg))
	if c.Form("contact") == nil || c.Form("other") != nil {
		t.Fatal("lookup wrong")
	}
}
