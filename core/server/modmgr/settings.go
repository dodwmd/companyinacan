package modmgr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modsettings"
	jsch "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// Field kinds of the settings-form renderer: the safe subset of JSON Schema it understands. Nested
// objects with properties are walked, their fields named by dotted path. Anything else (arrays of
// other than strings, numbers with fractions, oneOf ...) is not rendered; those settings are kept
// exactly as they are and edited in company.yaml.
const (
	KindString    = "string"
	KindInt       = "int"
	KindBool      = "bool"
	KindEnum      = "enum"
	KindSecretRef = "secret-ref"
	KindText      = "text" // several lines
	KindList      = "list" // strings, one per line
)

// FormPrefix is the form-field name prefix of generated settings fields (`s.<property>`).
const FormPrefix = "s."

// multilineFrom is the maxLength above which a string gets a text area.
const multilineFrom = 200

// MaxFields bounds how many properties of a schema are rendered.
const MaxFields = 60

// SettingsSchema is a module's compiled settings schema.
type SettingsSchema struct {
	doc      map[string]any
	compiled *jsch.Schema
}

// Field is one generated form control.
type Field struct {
	Name        string
	Kind        string
	Description string
	Required    bool
	Value       string // current value (or the schema default); for a secret-ref the NAME of a Secret
	Checked     bool   // bool fields
	Options     []string
	Min, Max    string // int bounds, empty when none
	MaxLen      int
	Fact        string // company.yaml facts used while the field is empty
}

// Form is the generated settings form of a module.
type Form struct {
	Fields []Field
	// Unsupported lists the properties the renderer cannot show; their values are preserved.
	Unsupported []string
	// NotApplied lists the settings the module cannot be given, with the reason.
	NotApplied []NotApplied
}

// NotApplied is a setting that reaches nothing (specs/064-module-settings).
type NotApplied struct {
	Name, Reason string
}

// noLoader refuses every external $ref: a schema is self-contained or it is not used.
type noLoader struct{}

func (noLoader) Load(u string) (any, error) {
	return nil, fmt.Errorf("external reference %q is not allowed", u)
}

const schemaURL = "settings.schema.json"

func compileSettingsSchema(raw []byte) (*SettingsSchema, error) {
	doc, err := jsch.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("not valid JSON")
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	c := jsch.NewCompiler()
	c.DefaultDraft(jsch.Draft2020)
	c.UseLoader(noLoader{})
	if err := c.AddResource(schemaURL, doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(schemaURL)
	if err != nil {
		return nil, err
	}
	return &SettingsSchema{doc: m, compiled: s}, nil
}

// leaf is one rendered or reported property.
type leaf struct {
	path     []string
	doc      map[string]any
	required bool
}

// leaves walks the schema's properties, ordered by name at every level. An object with properties
// and no annotation of its own is walked into.
func (s *SettingsSchema) leaves() []leaf {
	var out []leaf
	var walk func(doc map[string]any, path []string)
	walk = func(doc map[string]any, path []string) {
		props, _ := doc["properties"].(map[string]any)
		req := map[string]bool{}
		if r, ok := doc["required"].([]any); ok {
			for _, x := range r {
				if n, ok := x.(string); ok {
					req[n] = true
				}
			}
		}
		names := make([]string, 0, len(props))
		for k := range props {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			p, ok := props[k].(map[string]any)
			if !ok {
				continue
			}
			at := append(slices.Clone(path), k)
			if _, nested := p["properties"].(map[string]any); nested && p["x-can"] == nil && len(at) < 6 {
				walk(p, at)
				continue
			}
			// Required below an optional object is not required of the form.
			out = append(out, leaf{path: at, doc: p, required: req[k] && len(at) == 1})
		}
	}
	walk(s.doc, nil)
	return out
}

func numStr(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return ""
}

func fieldKind(p map[string]any) string {
	if c, ok := p["const"]; ok {
		if _, str := c.(string); str {
			return KindEnum
		}
		return ""
	}
	if e, ok := p["enum"].([]any); ok && len(e) > 0 {
		for _, x := range e {
			if _, ok := x.(string); !ok {
				return ""
			}
		}
		return KindEnum
	}
	switch p["type"] {
	case "string":
		if p["format"] == "secret-ref" {
			return KindSecretRef
		}
		if m, ok := p["maxLength"].(json.Number); ok {
			if n, err := m.Int64(); err == nil && n > multilineFrom {
				return KindText
			}
		}
		return KindString
	case "integer":
		return KindInt
	case "boolean":
		return KindBool
	case "array":
		if items, ok := p["items"].(map[string]any); ok && items["type"] == "string" {
			return KindList
		}
	}
	return ""
}

func annotationString(p map[string]any, key string) string {
	x, _ := p["x-can"].(map[string]any)
	v, _ := x[key].(string)
	return v
}

// factHint says where a setting's value comes from while it is empty.
func factHint(p map[string]any) string {
	x, _ := p["x-can"].(map[string]any)
	list, _ := x["fact"].([]any)
	var from []string
	for _, f := range list {
		if name, ok := f.(string); ok && modsettings.FactSource(name) != "" {
			from = append(from, modsettings.FactSource(name))
		}
	}
	return strings.Join(from, ", else ")
}

func valueAt(m map[string]any, path []string) (any, bool) {
	for i, k := range path {
		v, ok := m[k]
		if !ok {
			return nil, false
		}
		if i == len(path)-1 {
			return v, true
		}
		if m, ok = v.(map[string]any); !ok {
			return nil, false
		}
	}
	return nil, false
}

// shown is a value as a form field shows it.
func shown(kind string, v any) string {
	switch x := v.(type) {
	case nil, map[string]any:
		return ""
	case string:
		return x
	case time.Time: // YAML reads an unquoted date as a time
		if x.Equal(x.Truncate(24 * time.Hour)) {
			return x.UTC().Format(time.DateOnly)
		}
		return x.UTC().Format(time.RFC3339)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		if kind != KindList {
			return ""
		}
		lines := make([]string, 0, len(x))
		for _, e := range x {
			lines = append(lines, fmt.Sprint(e))
		}
		return strings.Join(lines, "\n")
	}
	return fmt.Sprint(v)
}

// BuildForm generates the form for the module's settings schema, filled from current. A module
// without a usable schema has an empty form.
func (e *Entry) BuildForm(current map[string]any) Form {
	var f Form
	if e.Schema == nil {
		return f
	}
	for _, l := range e.Schema.leaves() {
		p, name := l.doc, strings.Join(l.path, ".")
		if why := annotationString(p, "not_applied"); why != "" {
			if _, set := valueAt(current, l.path); set {
				why += " (set in company.yaml)"
			}
			f.NotApplied = append(f.NotApplied, NotApplied{Name: name, Reason: why})
			continue
		}
		k := fieldKind(p)
		if k == "" || len(f.Fields) >= MaxFields {
			f.Unsupported = append(f.Unsupported, name)
			continue
		}
		fl := Field{Name: name, Kind: k, Required: l.required, Fact: factHint(p)}
		fl.Description, _ = p["description"].(string)
		if k == KindList {
			fl.Description = strings.TrimSpace("One per line. " + fl.Description)
		}
		val, have := valueAt(current, l.path)
		if !have && fl.Fact == "" {
			val = p["default"]
		}
		switch k {
		case KindBool:
			b, _ := val.(bool)
			fl.Checked = b
		case KindEnum:
			if c, ok := p["const"].(string); ok {
				fl.Options = []string{"", c}
			} else {
				if _, def := p["default"]; !def || fl.Fact != "" {
					fl.Options = []string{""} // choosing nothing must be possible
				}
				for _, x := range p["enum"].([]any) {
					fl.Options = append(fl.Options, x.(string))
				}
			}
			fl.Value, _ = val.(string)
		case KindInt:
			fl.Value = shown(k, val)
			fl.Min, fl.Max = numStr(p["minimum"]), numStr(p["maximum"])
		default:
			fl.Value = shown(k, val)
			if m, ok := p["maxLength"].(json.Number); ok {
				fl.MaxLen, _ = strconv.Atoi(m.String())
			}
		}
		f.Fields = append(f.Fields, fl)
	}
	return f
}

// validSecretRef accepts the name of a Secret (a DNS subdomain) with an optional key, and refuses the
// Secrets can-core owns (`can-*`, `kubernetes.io*`), consecutive dots, and `.`/`..` keys.
func validSecretRef(v string) bool { return config.ValidSecretRef(v) }

// ParseSettings merges the submitted form into a copy of current and validates the result with the
// module's JSON Schema. The returned map is only meaningful when the error map is empty; errors are
// keyed by dotted property name (empty key: not tied to one field) with a short code: required,
// type, range, length, pattern, enum, invalid. A default that was only shown is not written.
func (e *Entry) ParseSettings(current map[string]any, form url.Values) (map[string]any, map[string]string) {
	errs := map[string]string{}
	if e.Schema == nil {
		return nil, map[string]string{"": "invalid"}
	}
	merged := deepCopy(current)
	docs := map[string]map[string]any{}
	for _, l := range e.Schema.leaves() {
		docs[strings.Join(l.path, ".")] = l.doc
	}
	for _, fl := range e.BuildForm(current).Fields {
		n, k, p := fl.Name, fl.Kind, docs[fl.Name]
		path := strings.Split(n, ".")
		stored, had := valueAt(current, path)
		def, hasDef := p["default"]
		if fl.Fact != "" {
			hasDef = false // empty means the company's fact
		}
		raw := strings.ReplaceAll(form.Get(FormPrefix+n), "\r\n", "\n")
		if k == KindBool {
			on := raw != ""
			d := hasDef && def == true
			switch {
			case !had && on == d:
			case on:
				setAt(merged, path, true)
			default:
				setAt(merged, path, false)
			}
			continue
		}
		// Unchanged stays as it was written (a YAML date is not a string; a text area cannot show a
		// final newline).
		if had && strings.TrimRight(raw, "\n") == strings.TrimRight(shown(k, stored), "\n") {
			continue
		}
		if k == KindText {
			raw = strings.TrimRight(raw, "\n")
		} else {
			raw = strings.TrimSpace(raw)
		}
		if strings.TrimSpace(raw) == "" {
			deleteAt(merged, current, path)
			continue
		}
		if !had && hasDef && raw == shown(k, jsonValue(def)) {
			continue
		}
		switch k {
		case KindInt:
			i, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				errs[n] = "type"
				continue
			}
			setAt(merged, path, i)
		case KindList:
			var list []any
			for _, line := range strings.Split(raw, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					list = append(list, line)
				}
			}
			setAt(merged, path, list)
		case KindSecretRef:
			if !validSecretRef(raw) {
				errs[n] = "pattern"
				continue
			}
			setAt(merged, path, raw)
		default:
			setAt(merged, path, raw)
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	if verr := e.NewProblems(current, merged); verr != nil {
		return nil, verr
	}
	return merged, nil
}

func jsonValue(v any) any {
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return v
}

func deepCopy(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if c, ok := v.(map[string]any); ok {
			v = deepCopy(c)
		}
		out[k] = v
	}
	return out
}

func setAt(m map[string]any, path []string, v any) {
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[path[len(path)-1]] = v
}

// deleteAt removes path, and a mapping it leaves empty unless that was written empty before.
func deleteAt(m, current map[string]any, path []string) {
	if len(path) == 1 {
		delete(m, path[0])
		return
	}
	child, ok := m[path[0]].(map[string]any)
	if !ok {
		return
	}
	was, existed := current[path[0]].(map[string]any)
	deleteAt(child, was, path[1:])
	if len(child) == 0 && (!existed || len(was) > 0) {
		delete(m, path[0])
	}
}

// NewProblems are what the module's JSON Schema says against next and did not already say about
// the same value of current: a company.yaml written before the schema was this strict can still be
// edited.
func (e *Entry) NewProblems(current, next map[string]any) map[string]string {
	if e.Schema == nil {
		return nil
	}
	errs := e.Schema.validate(next)
	for field, code := range e.Schema.validate(current) {
		was, _ := valueAt(current, strings.Split(field, "."))
		now, _ := valueAt(next, strings.Split(field, "."))
		if field != "" && errs[field] == code && fmt.Sprint(was) == fmt.Sprint(now) {
			delete(errs, field)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// Validate checks a complete settings object (for example one that a patch will write).
func (e *Entry) Validate(settings map[string]any) map[string]string {
	if e.Schema == nil {
		return nil
	}
	return e.Schema.validate(settings)
}

func (s *SettingsSchema) validate(v map[string]any) map[string]string {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]string{"": "invalid"}
	}
	inst, err := jsch.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return map[string]string{"": "invalid"}
	}
	err = s.compiled.Validate(inst)
	if err == nil {
		return nil
	}
	out := map[string]string{}
	var ve *jsch.ValidationError
	if !errors.As(err, &ve) {
		return map[string]string{"": "invalid"}
	}
	collect(ve, out)
	if len(out) == 0 {
		out[""] = "invalid"
	}
	return out
}

func collect(ve *jsch.ValidationError, out map[string]string) {
	if len(ve.Causes) > 0 {
		for _, c := range ve.Causes {
			collect(c, out)
		}
		return
	}
	var at []string // the property: array indexes are not part of a field's name
	for _, part := range ve.InstanceLocation {
		if _, err := strconv.Atoi(part); err == nil {
			break
		}
		at = append(at, part)
	}
	field := strings.Join(at, ".")
	code := "invalid"
	switch k := ve.ErrorKind.(type) {
	case *kind.AdditionalProperties:
		// Each unknown key under its own name, so that one that was there already is not new.
		for _, p := range k.Properties {
			out[strings.Join(append(slices.Clone(at), p), ".")] = code
		}
		return
	case *kind.Required:
		if len(k.Missing) > 0 {
			field = strings.Join(append(slices.Clone(at), k.Missing[0]), ".")
		}
		code = "required"
	case *kind.Type:
		code = "type"
	case *kind.Minimum, *kind.Maximum, *kind.ExclusiveMinimum, *kind.ExclusiveMaximum, *kind.MultipleOf:
		code = "range"
	case *kind.MinLength, *kind.MaxLength:
		code = "length"
	case *kind.Pattern, *kind.Format:
		code = "pattern"
	case *kind.Enum, *kind.Const:
		code = "enum"
	}
	if _, dup := out[field]; !dup {
		out[field] = code
	}
}

// Multi reports a field shown as a text area.
func (f Field) Multi() bool { return f.Kind == KindText || f.Kind == KindList }
