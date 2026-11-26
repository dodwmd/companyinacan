package modmgr

import (
	"encoding/json"
	"net/url"
	"testing"

	"go.yaml.in/yaml/v3"
)

// submit is what a browser sends for a form that was not touched.
func submit(f Form) url.Values {
	v := url.Values{}
	for _, fl := range f.Fields {
		switch fl.Kind {
		case KindBool:
			if fl.Checked {
				v.Set(FormPrefix+fl.Name, "on")
			}
		case KindEnum:
			x := fl.Value
			if x == "" {
				x = fl.Options[0] // what a select shows first
			}
			v.Set(FormPrefix+fl.Name, x)
		default:
			v.Set(FormPrefix+fl.Name, fl.Value)
		}
	}
	return v
}

// Saving a shipped module's form without touching it writes nothing: no default, no empty mapping,
// and what company.yaml had stays as it was.
func TestAnUntouchedShippedFormChangesNothing(t *testing.T) {
	cat, err := LoadCatalogue("../../../modules")
	must(t, err)
	current := map[string]string{
		"finance":   `{payment_instructions: "line one\nline two\n", seller: {address: "1 Way\nTown"}, base_currency: EUR, ui: {idle_minutes: 10}, retention_years: 7, old_key: 1}`,
		"forms":     `{notify_email: [a@b.example, c@d.example], captcha_provider: turnstile, trust_proxy: false}`,
		"files":     `{scanner_required: true, inline_images: true}`,
		"people":    `{retention: {people-timeoff: 3y}}`,
		"board":     `{retention: {board-minutes: legal}}`,
		"support":   `{inbound: {imap: {host: imap.example.com, port: 993}}}`,
		"analytics": `{retention_days: 90}`,
		"equity":    `{identity: attestation, break_glass_until: 2026-11-30}`,
	}
	for _, e := range cat.Entries {
		if e.Schema == nil {
			continue
		}
		if e.Mapping == nil {
			t.Fatalf("%s: %s", e.ID, e.SchemaProblem)
		}
		for _, src := range []string{"{}", current[e.ID]} {
			cur := map[string]any{}
			if src != "" {
				must(t, yaml.Unmarshal([]byte(src), &cur))
			}
			got, errs := e.ParseSettings(cur, submit(e.BuildForm(cur)))
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(cur)
			if len(errs) > 0 || string(a) != string(b) {
				t.Errorf("%s: %s became %s %v", e.ID, b, a, errs)
			}
		}
	}
}

func TestNestedSettingsForm(t *testing.T) {
	cat, err := LoadCatalogue("../../../modules")
	must(t, err)
	e := cat.Get("finance")
	f := e.BuildForm(map[string]any{})
	kinds := map[string]string{}
	for _, fl := range f.Fields {
		kinds[fl.Name] = fl.Kind
	}
	for n, k := range map[string]string{"seller.name": KindString, "seller.address": KindText, "payment_instructions": KindText, "ui.idle_minutes": KindInt, "base_currency": KindEnum} {
		if kinds[n] != k {
			t.Errorf("%s: %q, want %q", n, kinds[n], k)
		}
	}
	if len(f.NotApplied) != 1 || f.NotApplied[0].Name != "webhook_url" || len(f.Unsupported) != 0 {
		t.Errorf("%+v %v", f.NotApplied, f.Unsupported)
	}
	form := submit(f)
	form.Set(FormPrefix+"seller.name", "Acme Pty Ltd")
	form.Set(FormPrefix+"seller.address", "1 Way\r\nTown")
	got, errs := e.ParseSettings(map[string]any{}, form)
	b, _ := json.Marshal(got)
	if len(errs) > 0 || string(b) != `{"seller":{"address":"1 Way\nTown","name":"Acme Pty Ltd"}}` {
		t.Fatalf("%s %v", b, errs)
	}
	// Emptying them again removes the mapping.
	got, errs = e.ParseSettings(got, submit(e.BuildForm(map[string]any{})))
	if len(errs) > 0 || len(got) != 0 {
		t.Fatalf("%v %v", got, errs)
	}
	form.Set(FormPrefix+"ui.idle_minutes", "1")
	if _, errs := e.ParseSettings(map[string]any{}, form); errs["ui.idle_minutes"] != "range" {
		t.Fatalf("%v", errs)
	}
}
