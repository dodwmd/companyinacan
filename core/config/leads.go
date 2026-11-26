package config

// Which website leads can-core hands to which module (specs/069-lead-adapters). One reading of
// company.yaml serves `can validate`, the installer and the adapters.

import (
	"regexp"
	"slices"
	"strings"
)

// DefaultNewsletterList is the list a lead is subscribed to when default_list is not written.
const DefaultNewsletterList = "newsletter"

// LeadIntake is what company.yaml says about leads. Nothing is handed on unless a form is named.
type LeadIntake struct {
	// SupportForms are the forms whose leads become support conversations
	// (modules.support.settings.tickets_from_forms).
	SupportForms []string
	// NewsletterForms are the forms whose leads may subscribe, with consent
	// (modules.newsletter.settings.subscribe_from_forms).
	NewsletterForms []string
	// NewsletterList is the name or UUID of the double opt-in list
	// (modules.newsletter.settings.default_list).
	NewsletterList string
}

// LeadIssue is something in company.yaml that keeps leads from being handed on.
type LeadIssue struct{ Path, Message string }

var leadFormRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`)

// FormList reads a list of form ids from a setting; ok is false when something in it is not one.
func FormList(v any) (ids []string, ok bool) {
	if v == nil {
		return nil, true
	}
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	for _, e := range list {
		s, isText := e.(string)
		if !isText || !leadFormRe.MatchString(s) {
			return nil, false // all or nothing: a typing mistake must not half apply
		}
		if !slices.Contains(ids, s) {
			ids = append(ids, s)
		}
	}
	return ids, true
}

// LeadIntakeOf reads company.yaml. A disabled or unlisted module takes no leads.
func LeadIntakeOf(c *Company) (LeadIntake, []LeadIssue) {
	var out LeadIntake
	var issues []LeadIssue
	bad := func(path, msg string) {
		issues = append(issues, LeadIssue{path, msg})
	}
	settings := func(id string) map[string]any {
		e, ok := c.Modules[id]
		if !ok || !e.IsEnabled() {
			return nil
		}
		return e.Settings
	}
	forms := func(id, key string) []string {
		ids, ok := FormList(settings(id)[key])
		if !ok {
			bad("/modules/"+id+"/settings/"+key, key+" must be a list of form ids; no lead is handed on until it is")
		}
		return ids
	}
	out.SupportForms = forms("support", "tickets_from_forms")
	out.NewsletterForms = forms("newsletter", "subscribe_from_forms")
	out.NewsletterList = DefaultNewsletterList
	if v, ok := settings("newsletter")["default_list"].(string); ok && strings.TrimSpace(v) != "" {
		out.NewsletterList = strings.TrimSpace(v)
	}

	if len(out.SupportForms) > 0 {
		if a, _ := settings("support")["support_address"].(string); a == "" {
			bad("/modules/support/settings/tickets_from_forms", "set support_address too: leads become conversations in the mailbox it names")
		}
	}
	if len(out.SupportForms)+len(out.NewsletterForms) > 0 {
		if e, ok := c.Modules["forms"]; ok && !e.IsEnabled() {
			bad("/modules/forms", "leads are handed on from the forms module, which is disabled")
		}
	}
	if len(out.NewsletterForms) > 0 {
		consent, _ := settings("forms")["newsletter_consent"].(map[string]any)
		var carry []string
		if text, _ := consent["text"].(string); strings.TrimSpace(text) != "" {
			carry, _ = FormList(consent["forms"])
			if len(carry) == 0 {
				carry = []string{DefaultContactForm}
			}
		}
		for _, f := range out.NewsletterForms {
			if !slices.Contains(carry, f) {
				bad("/modules/newsletter/settings/subscribe_from_forms", "form "+f+" has no consent checkbox, so nobody is subscribed from it: set modules.forms.settings.newsletter_consent (text, and forms when it is not contact)")
			}
		}
	}
	return out, issues
}
