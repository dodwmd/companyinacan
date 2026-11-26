package config

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func supportCompanyYAML(t *testing.T, y string) *Company {
	t.Helper()
	var c Company
	if err := yaml.Unmarshal([]byte("domain: acme.example\n"+y), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

const mailReady = "email:\n  relay: {host: smtp.example.net}\n  modules:\n    support: {enabled: true}\n"

func TestSupportMailOf(t *testing.T) {
	c := supportCompanyYAML(t, mailReady+`modules:
  support:
    settings:
      support_address: Help@Acme.example
      mailbox_name: Acme Support
      inbound:
        imap: {host: imap.example.net, username: help@acme.example}
`)
	m, issues := SupportMailOf(c)
	if len(issues) != 0 {
		t.Fatalf("issues: %v", issues)
	}
	if m.Address != "help@acme.example" || m.AdminEmail != "admin@acme.example" || m.MailboxName != "Acme Support" {
		t.Errorf("%+v", m)
	}
	if m.IMAP == nil || *m.IMAP != (SupportIMAP{Host: "imap.example.net", Username: "help@acme.example", Encryption: "ssl", Folder: "INBOX"}) {
		t.Errorf("imap: %+v", m.IMAP)
	}
}

func TestSupportMailFindings(t *testing.T) {
	set := func(settings string) string {
		return mailReady + "modules:\n  support:\n    settings: " + settings + "\n"
	}
	for name, tc := range map[string]struct {
		yaml, path, want string
		warning          bool
	}{
		"admin's own address":  {set("{support_address: admin@acme.example}"), "/support_address", "differ", false},
		"chosen admin address": {set("{admin_email: me@acme.example, support_address: ME@acme.example}"), "/support_address", "differ", false},
		"not an address":       {set("{support_address: nobody}"), "/support_address", "email address", false},
		"not text":             {set("{support_address: [a@b.example]}"), "/support_address", "one line of text", false},
		"no TLS": {set("{support_address: help@acme.example, inbound: {imap: {host: h.example, encryption: none}}}"),
			"/inbound/imap/encryption", "TLS", false},
		"no host": {set("{support_address: help@acme.example, inbound: {imap: {}}}"), "/inbound/imap/host", "required", false},
		"url as host": {set("{support_address: help@acme.example, inbound: {imap: {host: 'imaps://h'}}}"),
			"/inbound/imap/host", "host name", false},
		"bad port": {set("{support_address: help@acme.example, inbound: {imap: {host: h.example, port: 70000}}}"),
			"/inbound/imap/port", "between", false},
		"imap without address": {set("{inbound: {imap: {host: h.example}}}"), "/inbound/imap", "support_address", false},
		"webhook":              {set("{support_address: help@acme.example, inbound: {mode: webhook}}"), "/inbound/mode", "not built", true},
		"no inbound":           {set("{support_address: help@acme.example}"), "/inbound", "no mail comes", true},
		"no relay opt-in": {"modules:\n  support:\n    settings: {support_address: help@acme.example, inbound: {imap: {host: h.example}}}\n",
			"/support_address", "replies cannot be sent", true},
	} {
		_, issues := SupportMailOf(supportCompanyYAML(t, tc.yaml))
		found := false
		for _, is := range issues {
			if is.Path == "/modules/support/settings"+tc.path && strings.Contains(is.Message, tc.want) && is.Warning == tc.warning {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: want %q (warning=%v) at %s, got %v", name, tc.want, tc.warning, tc.path, issues)
		}
	}
}

func TestSupportMailOffOrUnconfigured(t *testing.T) {
	for _, y := range []string{"", "modules:\n  support: {enabled: false, settings: {support_address: bad}}\n", "modules:\n  support: {}\n",
		"modules:\n  support:\n    settings: {admin_email: me@acme.example, retention_days: 90}\n"} {
		m, issues := SupportMailOf(supportCompanyYAML(t, y))
		if len(issues) != 0 || m.Address != "" || m.IMAP != nil {
			t.Errorf("%q: %+v %v", y, m, issues)
		}
	}
}

func TestSiteSupportAddress(t *testing.T) {
	on := "site:\n  support: {}\nmodules:\n  support:\n    settings: {support_address: help@acme.example}\n"
	if a, w := SiteSupportAddress(supportCompanyYAML(t, on)); a != "help@acme.example" || w != "" {
		t.Errorf("%q %q", a, w)
	}
	for _, y := range []string{"site:\n  support: {}\n", strings.Replace(on, "    settings:", "    enabled: false\n    settings:", 1)} {
		if a, w := SiteSupportAddress(supportCompanyYAML(t, y)); a != "" || !strings.Contains(w, "left out") {
			t.Errorf("%q: %q %q", y, a, w)
		}
	}
	if a, w := SiteSupportAddress(supportCompanyYAML(t, "modules:\n  support:\n    settings: {support_address: help@acme.example}\n")); a != "" || w != "" {
		t.Errorf("without the section: %q %q", a, w)
	}
}
