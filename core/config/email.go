package config

import (
	"regexp"
	"sort"
	"strconv"
)

// Email is the email: block of company.yaml. No field of it is a secret: the relay user name and
// password live only in the Secret can-system/can-smtp-relay (docs/email.md).
type Email struct {
	Relay   *EmailRelay            `yaml:"relay"`
	Modules map[string]EmailModule `yaml:"modules"`
	Inbound map[string]any         `yaml:"inbound"`
	// NotificationRecipients limits where notification mail may go (addresses or "@domain"). Unset:
	// the company domain. A person's verified single sign-on address is always accepted.
	NotificationRecipients []string `yaml:"notification_recipients"`
}

// EmailRelay is the outbound SMTP relay.
type EmailRelay struct {
	Host      string `yaml:"host"`
	Port      int    `yaml:"port"`
	TLS       string `yaml:"tls"`
	From      string `yaml:"from"`
	SecretRef string `yaml:"secret_ref"`
}

// EmailModule is one module's sending rules.
type EmailModule struct {
	Enabled    *bool    `yaml:"enabled"`
	Recipients []string `yaml:"recipients"`
	PerHour    int      `yaml:"per_hour"`
	PerDay     int      `yaml:"per_day"`
	// NotificationText lets notification mail caused by this module carry the module's own title and
	// text. Off: a fixed text with the notification type and a link to the inbox.
	NotificationText bool `yaml:"notification_text"`
}

// EmailIssue is one validation finding with the path of the offending field.
type EmailIssue struct{ Path, Rule, Message string }

var relayHostRe = regexp.MustCompile(`^[A-Za-z0-9.:-]{1,253}$`)

// ValidRelayHost accepts a host name or IP address, without scheme, port or credentials.
func ValidRelayHost(h string) bool { return relayHostRe.MatchString(h) }

// ValidateEmail checks what the JSON schema cannot express.
func ValidateEmail(e *Email) []EmailIssue {
	if e == nil {
		return nil
	}
	var out []EmailIssue
	if r := e.Relay; r != nil && r.From != "" && !ValidAddress(r.From) {
		out = append(out, EmailIssue{"/email/relay/from", "semantic.email-address", "from must be a plain ASCII email address"})
	}
	for i, rule := range e.NotificationRecipients {
		if !ValidRecipientRule(rule) {
			out = append(out, EmailIssue{"/email/notification_recipients/" + strconv.Itoa(i), "semantic.email-recipient",
				"a recipient rule is an address (a@b.example) or a domain (@b.example)"})
		}
	}
	ids := make([]string, 0, len(e.Modules))
	for id := range e.Modules {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for i, rule := range e.Modules[id].Recipients {
			if !ValidRecipientRule(rule) {
				out = append(out, EmailIssue{"/email/modules/" + id + "/recipients/" + strconv.Itoa(i), "semantic.email-recipient",
					"a recipient rule is an address (a@b.example) or a domain (@b.example)"})
			}
		}
	}
	return out
}
