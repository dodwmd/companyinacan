// Package modsettings maps modules.<id>.settings of company.yaml, and the company's own facts, onto
// a module's chart values (specs/064-module-settings).
//
// The mapping is declared next to each property in the module's settings.schema.json, under the
// keyword "x-can" (JSON Schema ignores it). One reading of it serves `can validate`, the
// reconciler (what is an upgrade, and which upgrade waits for approval), the installer and the
// admin UI, so that they cannot disagree about what a setting does.
package modsettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	jsch "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/dodwmd/companyinacan/core/config"
)

// MaxSchemaBytes bounds a settings schema file.
const MaxSchemaBytes = 64 << 10

// Annotation is "x-can" of one property. Exactly one of Config, Value, Handled and NotApplied says
// where the setting goes.
type Annotation struct {
	// Config is a key of the module's configuration file (chart value config.<Config>).
	Config string `json:"config,omitempty"`
	// Value is a chart value outside config; only those under app. are allowed.
	Value string `json:"value,omitempty"`
	// Handled says which other part of can-core or of the chart reads the setting.
	Handled string `json:"handled,omitempty"`
	// NotApplied says why the setting reaches nothing.
	NotApplied string `json:"not_applied,omitempty"`
	// Check validates the value of a Handled setting here (others are validated by their reader).
	Check bool `json:"check,omitempty"`
	// Fact are company facts used, in this order, while the setting is not written.
	Fact []string `json:"fact,omitempty"`
	// Scale multiplies an integer (MiB to bytes).
	Scale int64 `json:"scale,omitempty"`
	// AlsoWhenTrue are further config keys set while a boolean setting is true.
	AlsoWhenTrue map[string]any `json:"also_when_true,omitempty"`
	// Widens says which change of the value makes an upgrade wait for approval (see widens).
	Widens string `json:"widens,omitempty"`
	// Unset is what the module runs with while nothing is mapped, in the target's units.
	Unset any `json:"unset,omitempty"`
	// Risk says, to the person approving, what the change does.
	Risk string `json:"risk,omitempty"`
}

// Target is the chart value path the annotation maps to, "" when it maps to none.
func (a Annotation) Target() string {
	if a.Config != "" {
		return "config." + a.Config
	}
	return a.Value
}

// How a value may widen what a module does.
const (
	widensTrue      = "becomes_true"
	widensFalse     = "becomes_false"
	widensValue     = "becomes:" // followed by the value
	widensChanged   = "changed"
	widensAdded     = "added"     // a list gains an entry
	widensRemoved   = "removed"   // a list loses an entry
	widensLower     = "lower"     // a smaller number
	widensHigher    = "higher"    // a larger number
	widensKeepLess  = "keep_less" // days or years kept, 0 meaning for ever (or never delete)
	widensLimit     = "limit"     // a limit, 0 meaning none
	widensRetention = "retention" // 30d, 12m, 7y, 0 (at once) or legal (for ever)
)

var widenKinds = []string{widensTrue, widensFalse, widensChanged, widensAdded, widensRemoved, widensLower, widensHigher, widensKeepLess, widensLimit, widensRetention}

// platformKeys are configuration keys can-core and the charts own: no setting may be mapped to one
// or to anything below it.
var platformKeys = []string{
	"listen", "api_listen", "db_path", "resume_dir", "staging_dir", "keyring_path", "keyring_file", "path_prefix",
	"ui.enabled", "ui.insecure_cookies", "ui.force_secure", "ui.token_login", "ui.share_links",
	"share.base_url", "share.published", "company.base_url", "sign_base_url",
	"backend", "webhook", "introspect", "directory", "connectors", "email",
}

var (
	secretName = regexp.MustCompile(`(?i)(passw|secret|token|api[_-]?key|private[_-]?key|access[_-]?key|credential)`)
	targetRe   = regexp.MustCompile(`^(config|app)(\.[A-Za-z0-9_-]+)+$`)
)

// SecretName reports whether a key reads like the name of a secret.
func SecretName(k string) bool { return secretName.MatchString(k) }

type node struct {
	path     []string
	children map[string]*node // nil: a leaf
	closed   bool             // additionalProperties: false
	ann      *Annotation
	schema   *jsch.Schema
	doc      map[string]any
}

// Schema is a module's settings schema with its mapping. A nil Schema (a module without one) is
// usable.
type Schema struct {
	root *node
	// annotated: the schema declares a mapping. Without one (a module of the operator's own) the
	// chart gets .Values.settings as ever and nothing is said about what is applied.
	annotated bool
}

type noLoader struct{}

func (noLoader) Load(u string) (any, error) {
	return nil, fmt.Errorf("external reference %q is not allowed", u)
}

const schemaURL = "settings.schema.json"

// Load compiles a settings schema and checks its annotations.
func Load(raw []byte) (*Schema, error) {
	if len(raw) > MaxSchemaBytes {
		return nil, fmt.Errorf("settings schema is larger than %d KiB", MaxSchemaBytes>>10)
	}
	doc, err := jsch.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("settings schema is not valid JSON")
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("settings schema is not an object")
	}
	c := jsch.NewCompiler()
	c.DefaultDraft(jsch.Draft2020)
	c.UseLoader(noLoader{})
	if err := c.AddResource(schemaURL, doc); err != nil {
		return nil, err
	}
	if _, err := c.Compile(schemaURL); err != nil {
		return nil, err
	}
	s := &Schema{}
	if s.root, err = s.build(c, m, nil, "", 0); err != nil {
		return nil, err
	}
	if s.root.children == nil {
		s.root.children = map[string]*node{}
	}
	return s, nil
}

func (s *Schema) build(c *jsch.Compiler, doc map[string]any, p []string, pointer string, depth int) (*node, error) {
	if depth > 6 {
		return nil, errors.New("settings schema is nested too deeply")
	}
	n := &node{path: p, doc: doc, closed: doc["additionalProperties"] == false}
	if raw, ok := doc["x-can"]; ok {
		a, err := annotation(raw)
		if err != nil {
			return nil, fmt.Errorf("x-can of %s: %w", strings.Join(p, "."), err)
		}
		n.ann, s.annotated = a, true
	}
	props, _ := doc["properties"].(map[string]any)
	if n.ann == nil && props != nil {
		n.children = map[string]*node{}
		for k, v := range props {
			sub, ok := v.(map[string]any)
			if !ok {
				continue
			}
			esc := strings.NewReplacer("~", "~0", "/", "~1").Replace(k)
			child, err := s.build(c, sub, append(slices.Clone(p), k), pointer+"/properties/"+esc, depth+1)
			if err != nil {
				return nil, err
			}
			n.children[k] = child
		}
		return n, nil
	}
	if len(p) > 0 {
		sch, err := c.Compile(schemaURL + "#" + pointer)
		if err != nil {
			return nil, fmt.Errorf("settings schema of %s: %w", strings.Join(p, "."), err)
		}
		n.schema = sch
	}
	return n, nil
}

func annotation(raw any) (*Annotation, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var a Annotation
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, err
	}
	kinds := 0
	for _, v := range []string{a.Config, a.Value, a.Handled, a.NotApplied} {
		if v != "" {
			kinds++
		}
	}
	if kinds != 1 {
		return nil, errors.New("needs exactly one of config, value, handled and not_applied")
	}
	var targets []string
	if t := a.Target(); t != "" {
		targets = append(targets, t)
	}
	for k := range a.AlsoWhenTrue {
		targets = append(targets, "config."+k)
	}
	for _, t := range targets {
		if err := checkTarget(t); err != nil {
			return nil, err
		}
	}
	for _, f := range a.Fact {
		if factNames[f] == "" {
			return nil, fmt.Errorf("unknown company fact %q", f)
		}
	}
	switch w := a.Widens; {
	case w == "":
	case a.Target() == "":
		return nil, errors.New("widens needs a mapped value")
	case a.Risk == "":
		return nil, errors.New("widens needs a risk")
	case !strings.HasPrefix(w, widensValue) && !slices.Contains(widenKinds, w):
		return nil, fmt.Errorf("unknown widens %q", w)
	}
	a.Unset = normal(a.Unset)
	return &a, nil
}

func checkTarget(t string) error {
	if !targetRe.MatchString(t) {
		return fmt.Errorf("a setting may be mapped to config.* or app.* only, not %q", t)
	}
	key := strings.TrimPrefix(t, "config.")
	for _, p := range platformKeys {
		if key == p || strings.HasPrefix(key, p+".") {
			return fmt.Errorf("%q is set by can-core, not by a setting", t)
		}
	}
	for _, part := range strings.Split(key, ".") {
		if secretName.MatchString(part) {
			return fmt.Errorf("a secret is never a setting: %q", t)
		}
	}
	return nil
}

// LoadModule loads the schema a manifest names, in the module's own directory. A module that names
// none has a nil Schema.
func LoadModule(module fs.FS, rel string) (*Schema, error) {
	if rel == "" {
		return nil, nil
	}
	name := strings.TrimPrefix(path.Clean("/"+rel), "/") // cannot leave the module
	fi, err := fs.Stat(module, name)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("settings schema file not found")
	}
	if fi.Size() > MaxSchemaBytes {
		return nil, fmt.Errorf("settings schema is larger than %d KiB", MaxSchemaBytes>>10)
	}
	b, err := fs.ReadFile(module, name)
	if err != nil {
		return nil, errors.New("settings schema cannot be read")
	}
	return Load(b)
}

// Property is one leaf of the schema.
type Property struct {
	Path []string
	// Annotation is nil for a schema that declares no mapping.
	Annotation *Annotation
	Doc        map[string]any
}

// Name is the dotted path.
func (p Property) Name() string { return strings.Join(p.Path, ".") }

// Properties lists the leaves, ordered by path.
func (s *Schema) Properties() []Property {
	if s == nil {
		return nil
	}
	var out []Property
	var walk func(*node)
	walk = func(n *node) {
		if n.children == nil {
			out = append(out, Property{Path: n.path, Annotation: n.ann, Doc: n.doc})
			return
		}
		for _, k := range sortedKeys(n.children) {
			walk(n.children[k])
		}
	}
	for _, k := range sortedKeys(s.root.children) {
		walk(s.root.children[k])
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Facts are what company.yaml says about the company itself.
type Facts struct {
	Name      string // company.yaml name
	LegalName string
	Address   string
	TaxID     string
	Currency  string
	Timezone  string
}

// FactsOf reads them.
func FactsOf(name string, o config.Organisation) Facts {
	return Facts{Name: name, LegalName: o.LegalName, Address: o.Address, TaxID: o.TaxID, Currency: o.Currency, Timezone: o.Timezone}
}

// factNames are the names an annotation may use, and where each is written in company.yaml.
var factNames = map[string]string{
	"name": "name", "legal_name": "organisation.legal_name", "address": "organisation.address",
	"tax_id": "organisation.tax_id", "currency": "organisation.currency", "timezone": "organisation.timezone",
}

// FactSource is where company.yaml holds a fact, "" for an unknown one.
func FactSource(name string) string { return factNames[name] }

func (f Facts) get(name string) string {
	switch name {
	case "name":
		return f.Name
	case "legal_name":
		return f.LegalName
	case "address":
		return f.Address
	case "tax_id":
		return f.TaxID
	case "currency":
		return f.Currency
	case "timezone":
		return f.Timezone
	}
	return ""
}

// Kinds of Issue.
const (
	Unknown    = "unknown"
	Invalid    = "invalid"
	NotApplied = "not-applied"
	Secret     = "secret"
	BadFact    = "fact"
)

// Issue is a setting (or a company fact) that does not reach the module. Messages never quote a
// value: what is wrongly written there may be a password.
type Issue struct {
	Path    []string // below settings
	Kind    string
	Message string
}

// Name is the dotted path.
func (i Issue) Name() string { return strings.Join(i.Path, ".") }

// Result is what the settings of one module come to.
type Result struct {
	// Values are the mapped chart values by dotted path.
	Values map[string]any
	Issues []Issue
}

// Nested is Values as chart values.
func (r Result) Nested() map[string]any {
	out := map[string]any{}
	for _, t := range sortedKeys(r.Values) {
		parts := strings.Split(t, ".")
		m := out
		for _, p := range parts[:len(parts)-1] {
			next, ok := m[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[p] = next
			}
			m = next
		}
		m[parts[len(parts)-1]] = r.Values[t]
	}
	return out
}

// Map applies the schema to settings. A value that is unknown or invalid is left out, so that the
// module runs with the company fact or its default instead, and is reported.
func (s *Schema) Map(settings map[string]any, facts Facts) Result {
	r := Result{Values: map[string]any{}}
	if s == nil {
		for _, k := range sortedKeys(settings) {
			r.issue([]string{k}, Unknown, "this module has no settings schema: nothing is known to apply the setting")
		}
		return r
	}
	r.unknown(s.root, settings)
	for _, p := range s.Properties() {
		n := s.at(p.Path)
		v, set := lookup(settings, p.Path)
		a := n.ann
		if a == nil {
			if set && !n.valid(v) {
				r.issue(p.Path, Invalid, "the value does not match the module's settings schema")
			}
			if set && s.annotated {
				r.issue(p.Path, NotApplied, "the module's schema does not say how to apply it")
			}
			continue
		}
		if set && a.NotApplied != "" {
			r.issue(p.Path, NotApplied, "accepted but not applied: "+a.NotApplied)
		}
		if set && (a.Target() != "" || a.Check || a.NotApplied != "") && !n.valid(v) {
			r.issue(p.Path, Invalid, "the value does not match the module's settings schema, so it is not applied")
			set = false
		}
		if a.Target() == "" {
			continue
		}
		if !set {
			var fact string
			for _, name := range a.Fact {
				if fact = facts.get(name); fact != "" {
					if !n.valid(fact) {
						r.issue(p.Path, BadFact, FactSource(name)+" cannot be used by this module here, which runs with its default")
						fact = ""
					}
					break
				}
			}
			if fact == "" {
				continue
			}
			v = fact
		}
		v = normal(v)
		if i, ok := v.(int64); ok && a.Scale > 0 {
			v = i * a.Scale
		}
		r.Values[a.Target()] = v
		if v == true {
			for k, x := range a.AlsoWhenTrue {
				r.Values["config."+k] = normal(x)
			}
		}
	}
	return r
}

func (r *Result) issue(p []string, kind, msg string) {
	r.Issues = append(r.Issues, Issue{Path: p, Kind: kind, Message: msg})
}

// unknown reports keys the schema does not declare, and mappings written as something else.
func (r *Result) unknown(n *node, v map[string]any) {
	for _, k := range sortedKeys(v) {
		p := append(slices.Clone(n.path), k)
		c := n.children[k]
		switch {
		case c == nil:
			if holdsSecretName(k, v[k], 0) {
				r.issue(p, Secret, "this reads like a secret, which is never a setting and is not applied: remove it from company.yaml (and rotate it if the file was shared)")
			} else if n.closed {
				r.issue(p, Unknown, "unknown setting, which is not applied")
			}
		case c.children != nil && v[k] != nil:
			if m, ok := v[k].(map[string]any); ok {
				r.unknown(c, m)
			} else {
				r.issue(p, Invalid, "a mapping is expected here, so nothing below it is applied")
			}
		}
	}
}

func holdsSecretName(k string, v any, depth int) bool {
	if secretName.MatchString(k) {
		return true
	}
	if m, ok := v.(map[string]any); ok && depth < 5 {
		for ck, cv := range m {
			if holdsSecretName(ck, cv, depth+1) {
				return true
			}
		}
	}
	return false
}

func (s *Schema) at(p []string) *node {
	n := s.root
	for _, k := range p {
		n = n.children[k]
	}
	return n
}

func lookup(m map[string]any, p []string) (any, bool) {
	for i, k := range p {
		v, ok := m[k]
		if !ok || v == nil {
			return nil, false
		}
		if i == len(p)-1 {
			return v, true
		}
		if m, ok = v.(map[string]any); !ok {
			return nil, false
		}
	}
	return nil, false
}

// valid checks one value against its subschema, and what a pattern cannot say.
func (n *node) valid(v any) bool {
	if n.schema == nil {
		return true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	inst, err := jsch.UnmarshalJSON(bytes.NewReader(b))
	if err != nil || n.schema.Validate(inst) != nil {
		return false
	}
	if x, ok := v.(string); ok {
		if strings.ContainsRune(x, 0) {
			return false
		}
		// The modules count bytes where a schema counts characters.
		if max, ok := n.doc["maxBytes"].(json.Number); ok {
			if m, err := max.Int64(); err == nil && int64(len(x)) > m {
				return false
			}
		}
		if n.doc["format"] == "timezone" {
			return ValidTimezone(x)
		}
	}
	return true
}

// ValidTimezone reports whether name is an IANA time zone this system knows.
func ValidTimezone(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// normal makes a value comparable and encodable the same way wherever it came from: integers are
// int64 (YAML gives int, JSON float64).
func normal(v any) any {
	switch x := v.(type) {
	case int:
		return int64(x)
	case uint64:
		return int64(x) //nolint:gosec // schema-bounded settings
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return normal(f)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normal(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normal(e)
		}
		return out
	}
	return v
}

// Normalise is normal for values read back from JSON.
func Normalise(v map[string]any) map[string]any {
	if v == nil {
		return nil
	}
	return normal(v).(map[string]any)
}
