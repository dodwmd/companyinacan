package modmgr

import (
	"net/url"
	"strings"
	"testing"
)

func webEntry(t *testing.T) *Entry {
	t.Helper()
	cat, err := LoadCatalogue(writeModules(t))
	must(t, err)
	return cat.Get("web")
}

func TestBuildFormRendersSafeSubset(t *testing.T) {
	e := webEntry(t)
	f := e.BuildForm(map[string]any{"site_title": "Acme", "max_rows": 7, "origins": []any{"https://a.example"}})
	kinds := map[string]string{}
	for _, fl := range f.Fields {
		kinds[fl.Name] = fl.Kind
	}
	want := map[string]string{"site_title": KindString, "max_rows": KindInt, "verbose": KindBool, "captcha": KindEnum, "api_key_secret": KindSecretRef}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("field %s kind %q, want %q", k, kinds[k], v)
		}
	}
	if len(f.Unsupported) != 1 || f.Unsupported[0] != "origins" {
		t.Fatalf("arrays are not rendered, they are reported: %v", f.Unsupported)
	}
	for _, fl := range f.Fields {
		if fl.Name == "site_title" && (fl.Value != "Acme" || !fl.Required || fl.MaxLen != 20) {
			t.Errorf("%+v", fl)
		}
		if fl.Name == "captcha" && (len(fl.Options) != 2 || fl.Value != "none") {
			t.Errorf("enum default: %+v", fl)
		}
	}
}

func TestParseSettingsValidatesWithTheModuleSchema(t *testing.T) {
	e := webEntry(t)
	cur := map[string]any{"site_title": "Old", "origins": []any{"https://keep.example"}}
	got, errs := e.ParseSettings(cur, url.Values{"s.site_title": {"New"}, "s.max_rows": {"25"}, "s.verbose": {"on"}, "s.captcha": {"turnstile"}, "s.api_key_secret": {"web-api-key"}})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if got["site_title"] != "New" || got["max_rows"] != int64(25) || got["verbose"] != true || got["captcha"] != "turnstile" || got["api_key_secret"] != "web-api-key" {
		t.Fatalf("%v", got)
	}
	if o, _ := got["origins"].([]any); len(o) != 1 {
		t.Fatalf("a setting the form cannot render must be kept as it was: %v", got)
	}
	if cur["site_title"] != "Old" {
		t.Fatal("the current settings map was modified")
	}
}

func TestParseSettingsRejectsWhatTheSchemaRejects(t *testing.T) {
	e := webEntry(t)
	for name, f := range map[string]url.Values{
		"missing required": {"s.site_title": {""}},
		"too long":         {"s.site_title": {strings.Repeat("x", 21)}},
		"int above max":    {"s.site_title": {"a"}, "s.max_rows": {"101"}},
		"int not number":   {"s.site_title": {"a"}, "s.max_rows": {"1e3"}},
		"enum outside":     {"s.site_title": {"a"}, "s.captcha": {"evil"}},
		"secret-ref shape": {"s.site_title": {"a"}, "s.api_key_secret": {"has space/../x"}},
	} {
		if _, errs := e.ParseSettings(nil, f); len(errs) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	// the error is attached to the field it is about
	_, errs := e.ParseSettings(nil, url.Values{"s.site_title": {"a"}, "s.max_rows": {"0"}})
	if errs["max_rows"] == "" {
		t.Fatalf("%v", errs)
	}
}

func TestParseSettingsIgnoresUnknownFormFields(t *testing.T) {
	e := webEntry(t)
	got, errs := e.ParseSettings(nil, url.Values{"s.site_title": {"a"}, "s.__proto__": {"x"}, "s.nope": {"1"}, "csrf": {"zzz"}})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, ok := got["nope"]; ok || len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestSchemaWithRemoteRefIsNotFetched(t *testing.T) {
	_, err := compileSettingsSchema([]byte(`{"type":"object","properties":{"a":{"$ref":"http://127.0.0.1:1/x.json"}}}`))
	if err == nil {
		t.Fatal("a remote $ref must be refused, not fetched")
	}
	_, err = compileSettingsSchema([]byte(`{"type":"object","properties":{"a":{"$ref":"file:///etc/passwd"}}}`))
	if err == nil {
		t.Fatal("a file $ref must be refused")
	}
}

func TestFormNeverCarriesSecretValues(t *testing.T) {
	e := webEntry(t)
	// A secret-ref holds the NAME of a Secret; whatever else is in the settings, the page shows only
	// the declared fields.
	f := e.BuildForm(map[string]any{"site_title": "x", "api_key_secret": "web-api-key", "hidden_token": "hunter2"})
	for _, fl := range f.Fields {
		if strings.Contains(fl.Value, "hunter2") {
			t.Fatal("undeclared setting echoed")
		}
	}
}
