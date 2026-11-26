package config

// The mail settings of the support module in company.yaml (modules.support.settings,
// specs/059-support-mailbox). One reading of them for `can validate`, can-core, the site and the
// CLI. No field is a secret: the IMAP password lives only in the Secret
// can-system/can-support-imap, the relay's in can-system/can-smtp-relay.

import (
	"slices"
	"strings"
	"unicode"
)

// Support module inbound encryptions: both require TLS.
const (
	IMAPSSL      = "ssl"      // implicit TLS, port 993
	IMAPStartTLS = "starttls" // STARTTLS, port 143
)

// SupportMail is what company.yaml says about the support inbox.
type SupportMail struct {
	Enabled     bool   // modules.support is listed and not disabled
	Address     string // support_address, lower case; "" when unset or invalid
	MailboxName string
	AdminEmail  string       // the first FreeScout administrator
	IMAP        *SupportIMAP // nil: nothing is fetched
}

// SupportIMAP is the mailbox that is fetched.
type SupportIMAP struct {
	Host       string // "" when the Secret is to name it
	Port       int    // 0: the default of the encryption
	Username   string // "" when the Secret is to name it
	Encryption string // IMAPSSL or IMAPStartTLS
	Folder     string
}

// SupportIssue is one validation finding. All of them are warnings: releases before specs/059
// accepted these keys and ignored them, so a company.yaml must not start to be refused.
type SupportIssue struct {
	Path, Rule, Message string
	// Invalid: a value that cannot be applied. While there is one, no mailbox setting is changed.
	Invalid bool
}

// SupportMailFor is SupportMailOf for one enabled module's settings.
func SupportMailFor(id, domain string, email *Email, settings map[string]any) (SupportMail, []SupportIssue) {
	on := true
	return SupportMailOf(&Company{Domain: domain, Email: email, Modules: map[string]ModuleEntry{id: {Enabled: &on, Settings: settings}}})
}

// SupportMailOf reads modules.support.settings. A disabled or unlisted module has no findings.
func SupportMailOf(c *Company) (SupportMail, []SupportIssue) {
	var m SupportMail
	entry, listed := c.Modules["support"]
	if !listed || !entry.IsEnabled() {
		return m, nil
	}
	m.Enabled = true
	var issues []SupportIssue
	const base = "/modules/support/settings"
	bad := func(path, msg string) {
		issues = append(issues, SupportIssue{Path: base + path, Rule: "semantic.support-mail", Message: msg, Invalid: true})
	}
	warn := func(path, msg string) {
		issues = append(issues, SupportIssue{Path: base + path, Rule: "semantic.support-mail-incomplete", Message: msg})
	}
	text := func(in map[string]any, key, path string, max int) string {
		v, set := in[key]
		if !set || v == nil {
			return ""
		}
		s, ok := v.(string)
		if !ok || len(s) > max || strings.IndexFunc(s, unicode.IsControl) >= 0 {
			bad(path, key+" must be one line of text")
			return ""
		}
		return strings.TrimSpace(s)
	}
	object := func(in map[string]any, key, path string) (map[string]any, bool) {
		v, set := in[key]
		if !set || v == nil {
			return nil, false
		}
		o, ok := v.(map[string]any)
		if !ok {
			bad(path, key+" must be a mapping")
		}
		return o, ok
	}

	s := entry.Settings
	m.AdminEmail = text(s, "admin_email", "/admin_email", 254)
	if m.AdminEmail == "" {
		m.AdminEmail = "admin@" + c.Domain
	}
	m.MailboxName = text(s, "mailbox_name", "/mailbox_name", 40)
	if a := text(s, "support_address", "/support_address", 128); a != "" {
		switch {
		case !ValidAddress(a):
			bad("/support_address", "support_address must be a plain ASCII email address")
		case strings.EqualFold(a, m.AdminEmail):
			bad("/support_address", "support_address must differ from the administrator's sign-in address ("+m.AdminEmail+"): FreeScout refuses a mailbox with a user's address")
		default:
			m.Address = strings.ToLower(a)
		}
	}

	mode := "imap"
	inbound, _ := object(s, "inbound", "/inbound")
	if v := text(inbound, "mode", "/inbound/mode", 16); v != "" {
		mode = v
	}
	imap, hasIMAP := object(inbound, "imap", "/inbound/imap")
	switch mode {
	case "imap":
	case "webhook":
		warn("/inbound/mode", "the inbound-parse webhook is not built (its adapter does not exist): no mail comes in; use mode imap")
		return m, issues
	default:
		bad("/inbound/mode", "mode is imap or webhook")
		return m, issues
	}
	if hasIMAP {
		in := &SupportIMAP{Encryption: IMAPSSL, Folder: "INBOX"}
		in.Host = text(imap, "host", "/inbound/imap/host", 253)
		if in.Host != "" && !ValidRelayHost(in.Host) {
			bad("/inbound/imap/host", "host is a host name or IP address, without scheme or port")
			in.Host = ""
		}
		if in.Host == "" {
			bad("/inbound/imap/host", "host is required")
		}
		if v, set := imap["port"]; set {
			p, ok := v.(int)
			if f, isFloat := v.(float64); isFloat && f == float64(int(f)) {
				p, ok = int(f), true
			}
			if !ok || p < 1 || p > 65535 {
				bad("/inbound/imap/port", "port is a number between 1 and 65535")
			} else {
				in.Port = p
			}
		}
		in.Username = text(imap, "username", "/inbound/imap/username", 254)
		if v := text(imap, "encryption", "/inbound/imap/encryption", 16); v != "" {
			if v != IMAPSSL && v != IMAPStartTLS {
				bad("/inbound/imap/encryption", "encryption is ssl (implicit TLS) or starttls; mail is not fetched without TLS")
			} else {
				in.Encryption = v
			}
		}
		if v := text(imap, "folder", "/inbound/imap/folder", 128); v != "" {
			in.Folder = v
		}
		if m.Address == "" {
			bad("/inbound/imap", "set support_address too: it is the address of the mailbox that is fetched into")
		}
		invalid := slices.ContainsFunc(issues, func(is SupportIssue) bool { return is.Invalid })
		if m.Address != "" && in.Host != "" && !invalid {
			m.IMAP = in
		}
	}
	if m.Address != "" {
		if !hasIMAP {
			warn("/inbound", "no inbound.imap: the mailbox is created but no mail comes into it")
		}
		relay := c.Email != nil && c.Email.Relay != nil
		on := c.Email != nil && c.Email.Modules["support"].Enabled != nil && *c.Email.Modules["support"].Enabled
		if !on || !relay {
			warn("/support_address", "replies cannot be sent until email.relay is set and email.modules.support.enabled is true (modules/support/README.md)")
		}
	}
	return m, issues
}

// SiteSupportAddress is the address site.support links to: "" without the section, or when it has
// nothing to point at, which the warning then says.
func SiteSupportAddress(c *Company) (address, warning string) {
	if c.Site.Support == nil {
		return "", ""
	}
	m, _ := SupportMailOf(c)
	if m.Address == "" {
		return "", "site.support is left out of the site: it needs the support module enabled with modules.support.settings.support_address"
	}
	return m.Address, ""
}
