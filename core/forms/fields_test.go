package forms

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testForm(t *testing.T) *Form {
	t.Helper()
	c, err := ParseConfig([]byte(minimalCfg))
	if err != nil {
		t.Fatal(err)
	}
	return c.Form("contact")
}

func codes(errs []FieldError) map[string]string {
	m := map[string]string{}
	for _, e := range errs {
		m[e.Field] = e.Code
	}
	return m
}

func TestValidateOK(t *testing.T) {
	clean, errs := testForm(t).Validate(map[string]string{"name": "  Ada  ", "email": "ada@example.com", "topic": "sales"})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if clean["name"] != "Ada" {
		t.Fatalf("expected trimmed value, got %q", clean["name"])
	}
}

func TestValidateErrors(t *testing.T) {
	long := strings.Repeat("a", 101)
	cases := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{"missing required", map[string]string{"email": "a@example.com"}, map[string]string{"name": "required"}},
		{"blank required", map[string]string{"name": "   ", "email": "a@example.com"}, map[string]string{"name": "required"}},
		{"bad email", map[string]string{"name": "x", "email": "not-an-email"}, map[string]string{"email": "invalid_email"}},
		{"display name email", map[string]string{"name": "x", "email": "Ada <a@example.com>"}, map[string]string{"email": "invalid_email"}},
		{"header injection", map[string]string{"name": "x", "email": "a@example.com\nBcc: x@y.z"}, map[string]string{"email": "invalid_email"}},
		{"too long", map[string]string{"name": long, "email": "a@example.com"}, map[string]string{"name": "too_long"}},
		{"not in list", map[string]string{"name": "x", "email": "a@example.com", "topic": "other"}, map[string]string{"topic": "not_allowed"}},
		{"unknown field", map[string]string{"name": "x", "email": "a@example.com", "admin": "1"}, map[string]string{"admin": "unknown_field"}},
		{"newline in single-line", map[string]string{"name": "a\nb", "email": "a@example.com"}, map[string]string{"name": "invalid_characters"}},
		{"control chars", map[string]string{"name": "a\x00b", "email": "a@example.com"}, map[string]string{"name": "invalid_characters"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := testForm(t).Validate(tc.in)
			got := codes(errs)
			for f, c := range tc.want {
				if got[f] != c {
					t.Fatalf("field %s: want %s, got %v", f, c, got)
				}
			}
		})
	}
}

func TestValidateOptionalEmpty(t *testing.T) {
	clean, errs := testForm(t).Validate(map[string]string{"name": "x", "email": "a@example.com", "topic": ""})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, ok := clean["topic"]; ok {
		t.Fatal("empty optional should be dropped")
	}
}

func TestMultilineAllowsBreaks(t *testing.T) {
	f := &Form{ID: "x", Fields: []Field{{Name: "m", Type: "text", MaxLength: 50, Multiline: true}}}
	if _, errs := f.Validate(map[string]string{"m": "a\nb"}); len(errs) != 0 {
		t.Fatal(errs)
	}
}

func TestMinLength(t *testing.T) {
	f := &Form{ID: "x", Fields: []Field{{Name: "m", Type: "text", MinLength: 5, MaxLength: 10}}}
	if _, errs := f.Validate(map[string]string{"m": "abc"}); codes(errs)["m"] != "too_short" {
		t.Fatal(errs)
	}
}

func req(ct, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	return r
}

func TestParseBody(t *testing.T) {
	w := httptest.NewRecorder()
	m, err := ParseBody(w, req("application/x-www-form-urlencoded", "a=1&b=two+words"), 1024)
	if err != nil || m["a"] != "1" || m["b"] != "two words" {
		t.Fatalf("%v %v", m, err)
	}
	m, err = ParseBody(w, req("application/json; charset=utf-8", `{"a":"1","b":"x"}`), 1024)
	if err != nil || m["b"] != "x" {
		t.Fatalf("%v %v", m, err)
	}
}

func TestParseBodyRejects(t *testing.T) {
	cases := []struct {
		name, ct, body string
		want           error
	}{
		{"media type", "text/plain", "a=1", ErrUnsupportedMedia},
		{"no media type", "", "a=1", ErrUnsupportedMedia},
		{"multipart", "multipart/form-data; boundary=x", "", ErrUnsupportedMedia},
		{"too large", "application/x-www-form-urlencoded", "a=" + strings.Repeat("x", 2000), ErrTooLarge},
		{"json too large", "application/json", `{"a":"` + strings.Repeat("x", 2000) + `"}`, ErrTooLarge},
		{"bad json", "application/json", `{`, ErrBadBody},
		{"json array", "application/json", `[1]`, ErrBadBody},
		{"json nested", "application/json", `{"a":{"b":1}}`, ErrBadBody},
		{"json number", "application/json", `{"a":1}`, ErrBadBody},
		{"json trailing", "application/json", `{"a":"1"} {"b":"2"}`, ErrBadBody},
		{"repeated key", "application/x-www-form-urlencoded", "a=1&a=2", ErrBadBody},
		{"bad escape", "application/x-www-form-urlencoded", "a=%zz", ErrBadBody},
		{"too many fields", "application/json", manyFields(100), ErrBadBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBody(httptest.NewRecorder(), req(tc.ct, tc.body), 1024)
			if err != tc.want {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func manyFields(n int) string {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"f` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `":"v"`)
	}
	b.WriteString("}")
	return b.String()
}
