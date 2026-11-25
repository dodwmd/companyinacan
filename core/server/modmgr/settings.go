package modmgr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	jsch "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// Field kinds of the settings-form renderer: the safe subset of JSON Schema it understands.
// Anything else (objects, arrays, numbers with fractions, oneOf ...) is not rendered; those settings
// are kept exactly as they are and edited in company.yaml.
const (
	KindString    = "string"
	KindInt       = "int"
	KindBool      = "bool"
	KindEnum      = "enum"
	KindSecretRef = "secret-ref"
)

// FormPrefix is the form-field name prefix of generated settings fields (`s.<property>`).
const FormPrefix = "s."

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
}

// Form is the generated settings form of a module.
type Form struct {
	Fields []Field
	// Unsupported lists the properties the renderer cannot show; their values are preserved.
	Unsupported []string
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

func (s *SettingsSchema) properties() (map[string]map[string]any, []string, map[string]bool) {
	props := map[string]map[string]any{}
	var names []string
	if p, ok := s.doc["properties"].(map[string]any); ok {
		for k, v := range p {
			if m, ok := v.(map[string]any); ok {
				props[k] = m
				names = append(names, k)
			}
		}
	}
	sort.Strings(names)
	req := map[string]bool{}
	if r, ok := s.doc["required"].([]any); ok {
		for _, x := range r {
			if n, ok := x.(string); ok {
				req[n] = true
			}
		}
	}
	return props, names, req
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
		return KindString
	case "integer":
		return KindInt
	case "boolean":
		return KindBool
	}
	return ""
}

// BuildForm generates the form for the module's settings schema, filled from current. A module
// without a usable schema has an empty form.
func (e *Entry) BuildForm(current map[string]any) Form {
	var f Form
	if e.Schema == nil {
		return f
	}
	props, names, req := e.Schema.properties()
	for _, n := range names {
		p := props[n]
		k := fieldKind(p)
		if k == "" || len(f.Fields) >= MaxFields {
			f.Unsupported = append(f.Unsupported, n)
			continue
		}
		fl := Field{Name: n, Kind: k, Required: req[n]}
		fl.Description, _ = p["description"].(string)
		val, have := current[n]
		if !have {
			val = p["default"]
		}
		switch k {
		case KindBool:
			b, _ := val.(bool)
			fl.Checked = b
		case KindEnum:
			for _, x := range p["enum"].([]any) {
				fl.Options = append(fl.Options, x.(string))
			}
			fl.Value, _ = val.(string)
		case KindInt:
			if val != nil {
				fl.Value = fmt.Sprint(val)
				if x, ok := val.(float64); ok {
					fl.Value = strconv.FormatFloat(x, 'f', -1, 64)
				}
			}
			fl.Min, fl.Max = numStr(p["minimum"]), numStr(p["maximum"])
		default:
			fl.Value, _ = val.(string)
			if m, ok := p["maxLength"].(json.Number); ok {
				fl.MaxLen, _ = strconv.Atoi(m.String())
			}
		}
		f.Fields = append(f.Fields, fl)
	}
	return f
}

var secretRefRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?(/[A-Za-z0-9._-]{1,253})?$`)

// validSecretRef accepts the name of a Secret (a DNS subdomain) with an optional key, and refuses the
// Secrets can-core owns (`can-*`, `kubernetes.io*`), consecutive dots, and `.`/`..` keys.
func validSecretRef(v string) bool {
	if !secretRefRe.MatchString(v) || len(v) > 253+1+253 {
		return false
	}
	name, key, _ := strings.Cut(v, "/")
	if len(name) > 253 || strings.Contains(name, "..") || strings.Contains(name, ".-") || strings.Contains(name, "-.") {
		return false
	}
	if strings.HasPrefix(name, "can-") || name == "can" || strings.HasPrefix(name, "kubernetes.io") {
		return false
	}
	return key != "." && key != ".." && !strings.Contains(key, "..")
}

// ParseSettings merges the submitted form into a copy of current and validates the result with the
// module's JSON Schema. The returned map is only meaningful when the error map is empty; errors are
// keyed by property name (empty key: not tied to one field) with a short code: required, type,
// range, length, pattern, enum, invalid.
func (e *Entry) ParseSettings(current map[string]any, form url.Values) (map[string]any, map[string]string) {
	errs := map[string]string{}
	if e.Schema == nil {
		return nil, map[string]string{"": "invalid"}
	}
	merged := map[string]any{}
	for k, v := range current {
		merged[k] = v
	}
	props, names, _ := e.Schema.properties()
	for _, n := range names {
		k := fieldKind(props[n])
		if k == "" {
			continue
		}
		raw := strings.TrimSpace(form.Get(FormPrefix + n))
		switch k {
		case KindBool:
			_, had := current[n]
			def, _ := props[n]["default"].(bool)
			switch {
			case form.Get(FormPrefix+n) != "":
				merged[n] = true
			case had || def:
				merged[n] = false
			default:
				delete(merged, n)
			}
			continue
		case KindInt:
			if raw == "" {
				delete(merged, n)
				continue
			}
			i, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				errs[n] = "type"
				continue
			}
			merged[n] = i
			continue
		case KindSecretRef:
			if raw != "" && !validSecretRef(raw) {
				errs[n] = "pattern"
				continue
			}
		}
		if raw == "" {
			delete(merged, n)
			continue
		}
		merged[n] = raw
	}
	if len(errs) > 0 {
		return nil, errs
	}
	if verr := e.Schema.validate(merged); verr != nil {
		return nil, verr
	}
	return merged, nil
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
	field := ""
	if len(ve.InstanceLocation) > 0 {
		field = ve.InstanceLocation[0]
	}
	code := "invalid"
	switch k := ve.ErrorKind.(type) {
	case *kind.Required:
		if len(k.Missing) > 0 {
			field = k.Missing[0]
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
