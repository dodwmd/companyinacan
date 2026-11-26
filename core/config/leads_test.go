package config

import (
	"slices"
	"testing"
)

func TestLeadIntakeOf(t *testing.T) {
	company := func(support, newsletter, forms map[string]any) *Company {
		return &Company{Modules: map[string]ModuleEntry{"support": {Settings: support}, "newsletter": {Settings: newsletter}, "forms": {Settings: forms}}}
	}
	in, issues := LeadIntakeOf(company(nil, nil, nil))
	if len(in.SupportForms)+len(in.NewsletterForms)+len(issues) != 0 || in.NewsletterList != "newsletter" {
		t.Fatalf("nothing configured: %+v %v", in, issues)
	}
	in, issues = LeadIntakeOf(company(
		map[string]any{"support_address": "support@example.com", "tickets_from_forms": []any{"contact", "contact"}},
		map[string]any{"subscribe_from_forms": []any{"contact"}, "default_list": "News"},
		map[string]any{"newsletter_consent": map[string]any{"text": "Yes please"}}))
	if len(issues) != 0 || !slices.Equal(in.SupportForms, []string{"contact"}) || !slices.Equal(in.NewsletterForms, []string{"contact"}) || in.NewsletterList != "News" {
		t.Fatalf("%+v %v", in, issues)
	}
	// No mailbox, no checkbox, and a list that is not one: said, and nothing half applied.
	in, issues = LeadIntakeOf(company(
		map[string]any{"tickets_from_forms": []any{"contact"}},
		map[string]any{"subscribe_from_forms": []any{"contact"}}, nil))
	if len(issues) != 2 {
		t.Fatalf("%+v %v", in, issues)
	}
	in, issues = LeadIntakeOf(company(map[string]any{"tickets_from_forms": []any{"contact", "Not a form"}}, nil, nil))
	if len(in.SupportForms) != 0 || len(issues) != 1 {
		t.Fatalf("%+v %v", in, issues)
	}
	off := false
	c := company(nil, map[string]any{"subscribe_from_forms": []any{"contact"}}, nil)
	c.Modules["newsletter"] = ModuleEntry{Enabled: &off, Settings: c.Modules["newsletter"].Settings}
	if in, _ := LeadIntakeOf(c); len(in.NewsletterForms) != 0 {
		t.Fatalf("a disabled module takes leads: %+v", in)
	}
}
