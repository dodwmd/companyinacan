package forms

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func consentConfig(t *testing.T, extra string) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(extra + "forms:\n  - id: contact\n    fields:\n      - {name: email, type: email, required: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewsletterConsentIsAddedUnticked(t *testing.T) {
	f := consentConfig(t, "newsletter_consent: {text: 'Send me news <b>'}\n").Form("contact")
	page := hostedPage(f, "tok")
	if !strings.Contains(page, `name="newsletter" type="checkbox" value="yes"> Send me news &lt;b&gt;`) || strings.Contains(page, "checked") {
		t.Fatalf("page: %s", page)
	}
	for in, want := range map[string]bool{"yes": true, "on": true, "no": false, "": false} {
		clean, errs := f.Validate(map[string]string{"email": "a@example.com", "newsletter": in})
		if len(errs) != 0 || slices.Contains(f.Consents(clean), "newsletter") != want {
			t.Errorf("%q: %v %v", in, clean, errs)
		}
	}
	if _, errs := f.Validate(map[string]string{"email": "a@example.com", "newsletter": "maybe"}); len(errs) != 1 {
		t.Errorf("errs = %v", errs)
	}
}

// A mistake in the setting adds no checkbox and never stops the forms.
func TestNewsletterConsentMistakesAreIgnored(t *testing.T) {
	for _, s := range []string{"newsletter_consent: {text: ''}\n", "newsletter_consent: {text: x, forms: [nope]}\n", "newsletter_consent: {text: \"a\\nb\"}\n"} {
		if n := len(consentConfig(t, s).Form("contact").Fields); n != 1 {
			t.Errorf("%q: %d fields", s, n)
		}
	}
}

func TestConsentFieldCannotBeRequiredAndNeedsWording(t *testing.T) {
	for _, field := range []string{"{name: ok, type: consent, label: x, required: true}", "{name: ok, type: consent}"} {
		if _, err := ParseConfig([]byte("forms:\n  - id: contact\n    fields:\n      - " + field + "\n")); err == nil {
			t.Errorf("%s accepted", field)
		}
	}
}

// Only a checkbox is consent: a text field of the same name records none.
func TestConsentsAreStored(t *testing.T) {
	st, err := OpenStore(t.TempDir() + "/f.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	text := Form{Fields: []Field{{Name: "newsletter", Type: TypeText}}}
	if got := text.Consents(map[string]string{"newsletter": "yes"}); got != nil {
		t.Fatalf("a text field is consent: %v", got)
	}
	for _, want := range [][]string{nil, {"newsletter"}} {
		l := &Lead{FormID: "contact", Fields: map[string]string{}, Consents: want, RetentionUntil: time.Now()}
		if err := st.Insert(context.Background(), l); err != nil {
			t.Fatal(err)
		}
		got, err := st.Get(context.Background(), l.ID)
		if err != nil || !slices.Equal(got.Consents, want) {
			t.Fatalf("consents %v, want %v (%v)", got.Consents, want, err)
		}
	}
}
