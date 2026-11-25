package provision

import (
	"strings"
	"testing"
	"time"
)

var userAttrs = map[string]bool{"username": true, "id": true, "active": true, "emails.value": true, "name.givenname": true, "name.familyname": true, "displayname": true, "externalid": true}

func TestParseFilterAccepts(t *testing.T) {
	cases := []struct {
		in    string
		terms int
	}{
		{`userName eq "alice@example.com"`, 1},
		{`USERNAME EQ "a"`, 1},
		{`userName eq "a" and active eq true`, 2},
		{`userName eq "a" AND name.familyName eq "O'Brien" and emails.value eq "x@y"`, 3},
		{`displayName eq "say \"hi\" \\ there"`, 1},
		{`  userName   eq   "padded"  `, 1},
		{`active eq false`, 1},
	}
	for _, c := range cases {
		f, err := ParseFilter(c.in, userAttrs)
		if err != nil || len(f) != c.terms {
			t.Errorf("%q: %v %v", c.in, f, err)
		}
	}
}

func TestParseFilterRejects(t *testing.T) {
	long := `userName eq "` + strings.Repeat("a", 600) + `"`
	many := strings.TrimSuffix(strings.Repeat(`userName eq "a" and `, 20), " and ")
	cases := map[string]string{
		"empty":             ``,
		"unknown attr":      `salary eq "1"`,
		"password":          `password eq "x"`,
		"unsupported op co": `userName co "a"`,
		"unsupported op ne": `userName ne "a"`,
		"or":                `userName eq "a" or userName eq "b"`,
		"not":               `not (userName eq "a")`,
		"parens":            `(userName eq "a")`,
		"unterminated":      `userName eq "abc`,
		"no value":          `userName eq`,
		"bare word":         `userName eq abc`,
		"trailing and":      `userName eq "a" and`,
		"leading and":       `and userName eq "a"`,
		"bad escape":        `userName eq "a\qb"`,
		"too long":          long,
		"too many terms":    many,
		"nul":               "userName eq \"a\x00\"",
		"newline in value":  "userName eq \"a\nb\"",
		"junk after":        `userName eq "a" "b"`,
		"bool as string":    `active eq "true"`,
		"string as bool":    `userName eq true`,
		"filter injection":  `userName eq "a" and 1 eq 1`,
		"deep path":         `name.givenName.x eq "a"`,
	}
	for name, in := range cases {
		if f, err := ParseFilter(in, userAttrs); err == nil {
			t.Errorf("%s: accepted %q as %v", name, in, f)
		} else if se, ok := err.(*SCIMError); !ok || se.Type != "invalidFilter" || se.Status != 400 {
			t.Errorf("%s: error %v is not an invalidFilter", name, err)
		}
	}
}

func TestFilterMatch(t *testing.T) {
	f, _ := ParseFilter(`userName eq "Alice@Example.com" and active eq true`, userAttrs)
	get := func(vals map[string]string) func(string) (string, bool) {
		return func(a string) (string, bool) { v, ok := vals[a]; return v, ok }
	}
	if !f.Match(get(map[string]string{"username": "alice@example.com", "active": "true"})) {
		t.Error("userName compares case-insensitively")
	}
	if f.Match(get(map[string]string{"username": "alice@example.com", "active": "false"})) {
		t.Error("active differs")
	}
	if f.Match(get(map[string]string{"active": "true"})) {
		t.Error("a missing attribute never matches")
	}
	id, _ := ParseFilter(`id eq "pers_aaaaaaaaaaaaaaaaaaaaaaaa"`, userAttrs)
	if id.Match(get(map[string]string{"id": "PERS_AAAAAAAAAAAAAAAAAAAAAAAA"})) {
		t.Error("id is case-exact")
	}
}

// The parser is linear: a hostile filter at the size limit parses in well under a second.
func TestParseFilterIsLinear(t *testing.T) {
	in := `userName eq "` + strings.Repeat(`\\`, 250) + `"`
	start := time.Now()
	for i := 0; i < 2000; i++ {
		_, _ = ParseFilter(in, userAttrs)
		_, _ = ParseFilter(strings.Repeat("and ", 125), userAttrs)
		_, _ = ParseFilter(strings.Repeat(`"`, 500), userAttrs)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}

func FuzzParseFilter(f *testing.F) {
	for _, s := range []string{`userName eq "a"`, `userName eq "a" and active eq true`, `"`, `\`, `eq`, ``, strings.Repeat("a", 600)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		flt, err := ParseFilter(s, userAttrs)
		if err == nil && (len(flt) == 0 || len(flt) > maxFilterTerms) {
			t.Fatalf("accepted %q with %d terms", s, len(flt))
		}
	})
}
