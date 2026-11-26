package contract

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/modsettings"
	"github.com/dodwmd/companyinacan/modules"
)

// settingsSchemas loads every shipped module's settings schema, by module id.
func settingsSchemas(t *testing.T) map[string]*modsettings.Schema {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "modules", "[a-z]*", "module.yaml"))
	if err != nil || len(paths) < 15 {
		t.Fatalf("%v %v", paths, err)
	}
	out := map[string]*modsettings.Schema{}
	for _, p := range paths {
		id := filepath.Base(filepath.Dir(p))
		rel, _ := readYAML(t, "modules/"+id+"/module.yaml")["settings_schema"].(string)
		if rel == "" {
			t.Errorf("%s has no settings_schema", id)
			continue
		}
		s, err := modsettings.LoadModule(os.DirFS(filepath.Dir(p)), rel)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		out[id] = s
		// `can` without a modules directory reads the built-in copy.
		if _, err := fs.Stat(modules.Shipped, id+"/settings.schema.json"); err != nil {
			t.Errorf("%s: the settings schema is not built into can: %v", id, err)
		}
	}
	return out
}

// Every property of every settings schema is mapped, handled elsewhere, or says why it is not
// applied; and the not applied ones are listed in docs/module-wiring.md.
func TestEverySettingIsMappedOrDocumentedAsNotApplied(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(root, "docs", "module-wiring.md"))
	if err != nil {
		t.Fatal(err)
	}
	for id, s := range settingsSchemas(t) {
		for _, p := range s.Properties() {
			name := id + "." + p.Name()
			if p.Annotation == nil {
				t.Errorf("%s: no x-can: map it (config or value), say who handles it, or why it is not applied", name)
				continue
			}
			if p.Annotation.NotApplied != "" && !strings.Contains(string(doc), "| `"+id+"` | `"+p.Name()+"` |") {
				t.Errorf("%s: not applied, and docs/module-wiring.md has no row | `%s` | `%s` |", name, id, p.Name())
			}
			if modsettings.SecretName(p.Path[len(p.Path)-1]) {
				t.Errorf("%s: a secret is never a setting", name)
			}
			if d, _ := p.Doc["description"].(string); d == "" && p.Doc["const"] == nil && p.Annotation.NotApplied == "" {
				t.Errorf("%s: no description for the admin UI form", name)
			}
		}
	}
}

// The mapping and the charts agree on what a module runs with while nothing is set: the admin UI
// shows the schema's default, and approval compares with the annotation's unset value.
func TestSettingDefaultsMatchTheCharts(t *testing.T) {
	norm := func(v any) string {
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			v = int64(f)
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	for id, s := range settingsSchemas(t) {
		var values map[string]any
		if v := readYAML(t, "modules/"+id+"/chart/values.yaml"); v != nil {
			values = v
		}
		for _, p := range s.Properties() {
			a := p.Annotation
			if a == nil || a.Target() == "" {
				continue
			}
			var chart any = values
			for _, k := range strings.Split(a.Target(), ".") {
				chart = asMap(chart)[k]
			}
			if chart == nil {
				continue
			}
			scaled := func(v any) any {
				if f, ok := v.(float64); ok && a.Scale > 0 {
					return int64(f) * a.Scale
				}
				return v
			}
			// An empty list or text in the chart only says "nothing".
			if fmt.Sprint(chart) == "" || len(asList(chart)) == 0 && norm(chart) == "[]" {
				continue
			}
			if d, ok := p.Doc["default"]; ok && fmt.Sprint(d) != "" && norm(scaled(d)) != norm(chart) {
				t.Errorf("%s.%s: schema default %v, chart %v", id, p.Name(), d, chart)
			}
			if a.Widens != "" && norm(a.Unset) != norm(chart) {
				t.Errorf("%s.%s: unset %v, chart %v", id, p.Name(), a.Unset, chart)
			}
		}
	}
}

func TestFinanceCurrenciesAreTheOnesFinanceKnows(t *testing.T) {
	var s struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	b, err := os.ReadFile(filepath.Join(root, "modules", "finance", "settings.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &s) // other properties have enums too
	have := s.Properties["base_currency"].Enum
	for a := 'A'; a <= 'Z'; a++ {
		for b := 'A'; b <= 'Z'; b++ {
			for c := 'A'; c <= 'Z'; c++ {
				code := string([]rune{a, b, c})
				if _, ok := finance.CurrencyExponent(code); ok != slices.Contains(have, code) {
					t.Errorf("%s: finance knows it: %v", code, ok)
				}
			}
		}
	}
}

// The annotation loader is what keeps a schema from reaching what can-core owns.
func TestAMappingCannotReachSecretsOrPlatformValues(t *testing.T) {
	for _, x := range []string{
		`{"config":"listen"}`, `{"config":"db_path"}`, `{"config":"ui.enabled"}`, `{"config":"ui"}.`, `{"config":"webhook.url"}`, `{"config":"keyring_file"}`,
		`{"config":"api_token"}`, `{"value":"image.repository"}`, `{"value":"secrets.module"}`, `{"value":"securityContext.runAsNonRoot"}`, `{"value":"ingress.host"}`,
		`{"config":"a","also_when_true":{"listen":":1"}}`, `{"config":"a","not_applied":"b"}`, `{}`, `{"config":"a","unknown":1}`, `{"config":"a","fact":["secret"]}`, `{"config":"a","widens":"sometimes","risk":"x"}`,
	} {
		x = strings.TrimSuffix(x, ".")
		if _, err := modsettings.Load([]byte(`{"type":"object","properties":{"a":{"type":"string","x-can":` + x + `}}}`)); err == nil {
			t.Errorf("accepted: %s", x)
		}
	}
}
