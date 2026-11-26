package config

import (
	"strings"
	"testing"
)

func TestValidAddressRefusesInjectionAndRouting(t *testing.T) {
	for _, ok := range []string{"a@b.example", "first.last+tag@sub.acme.example", "o'neil@acme.example"} {
		if !ValidAddress(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{
		"", "a", "a@b", "a@@b.example", "a@b.example\r\nBcc: x@y.example", "a@b.example\n", "a@b.example,x@y.example", "<a@b.example>", "A <a@b.example>",
		"a b@c.example", `"a"@b.example`, "a%evil.example@relay.example", "evil.example!a@relay.example", "@relay.example:a@b.example",
		".a@b.example", "a..b@c.example", "a@-b.example", "ü@b.example", "a@bü.example", "a@b.example\x00", strings.Repeat("a", 65) + "@b.example",
		"a@" + strings.Repeat("b.", 130) + "example",
	} {
		if ValidAddress(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestRecipientAllowed(t *testing.T) {
	rules := []string{"Sales@Partner.example", "@acme.example"}
	for _, ok := range []string{"sales@partner.example", "anyone@ACME.example"} {
		if !RecipientAllowed(ok, rules) {
			t.Errorf("%q should be allowed", ok)
		}
	}
	for _, bad := range []string{"other@partner.example", "a@sub.acme.example", "a@acme.example.evil.example", "a@evilacme.example", "a%x.example@acme.example"} {
		if RecipientAllowed(bad, rules) {
			t.Errorf("%q should be refused", bad)
		}
	}
	if RecipientAllowed("a@acme.example", nil) {
		t.Error("no rules must allow nobody")
	}
}

func TestValidateEmail(t *testing.T) {
	if got := ValidateEmail(nil); len(got) != 0 {
		t.Fatalf("nil: %v", got)
	}
	got := ValidateEmail(&Email{Relay: &EmailRelay{Host: "h", From: "x"}, Modules: map[string]EmailModule{"forms": {Recipients: []string{"@ok.example", "*"}}}})
	if len(got) != 2 || got[0].Path != "/email/relay/from" || got[1].Path != "/email/modules/forms/recipients/1" {
		t.Fatalf("%v", got)
	}
}
