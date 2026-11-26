package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jsch "github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/forms"
	"github.com/dodwmd/companyinacan/schemas/events"
)

func compileLeadCreated(t *testing.T) *jsch.Schema {
	t.Helper()
	doc, err := jsch.UnmarshalJSON(bytes.NewReader(events.LeadCreatedV1))
	if err != nil {
		t.Fatal(err)
	}
	c := jsch.NewCompiler()
	c.DefaultDraft(jsch.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("lead.created.v1.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("lead.created.v1.json")
	if err != nil {
		t.Fatalf("schema does not compile: %v", err)
	}
	return s
}

func validateJSON(t *testing.T, s *jsch.Schema, raw []byte) error {
	t.Helper()
	v, err := jsch.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return s.Validate(v)
}

func TestLeadCreatedFixtures(t *testing.T) {
	s := compileLeadCreated(t)
	for kind, wantValid := range map[string]bool{"valid": true, "invalid": false} {
		files, _ := filepath.Glob(filepath.Join(root, "testdata", "events", "lead.created.v1", kind+"-*.json"))
		if len(files) == 0 {
			t.Fatalf("no %s fixtures", kind)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			err = validateJSON(t, s, raw)
			if wantValid && err != nil {
				t.Errorf("%s should be valid: %v", filepath.Base(f), err)
			}
			if !wantValid && err == nil {
				t.Errorf("%s should be rejected", filepath.Base(f))
			}
		}
	}
}

// What the gateway really emits must satisfy the published schema.
func TestGatewayEventMatchesSchema(t *testing.T) {
	s := compileLeadCreated(t)
	cases := map[string]*forms.Lead{
		"full":             {ID: "ld_0123456789abcdef01234567", FormID: "contact", Email: "ada@example.com", Name: "Ada", Fields: map[string]string{"email": "ada@example.com", "name": "Ada", "message": "Hi\nthere"}, CreatedAt: time.Now()},
		"no email or name": {ID: "ld_0123456789abcdef01234567", FormID: "waitlist", Fields: map[string]string{"company": "Acme"}, CreatedAt: time.Now()},
		"nil fields":       {ID: "ld_0123456789abcdef01234567", FormID: "x", CreatedAt: time.Now()},
	}
	for name, l := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(forms.NewLeadCreated(l, time.Now()))
			if err != nil {
				t.Fatal(err)
			}
			if err := validateJSON(t, s, raw); err != nil {
				t.Fatalf("%v\n%s", err, raw)
			}
		})
	}
}

// The store really produces ids in the shape the schema promises.
func TestGeneratedIDsMatchSchemaPatterns(t *testing.T) {
	s, err := forms.OpenStore(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	l := &forms.Lead{FormID: "contact", Fields: map[string]string{"a": "b"}, CreatedAt: time.Now(), RetentionUntil: time.Now().Add(time.Hour)}
	if err := s.Insert(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(forms.NewLeadCreated(l, time.Now()))
	if err := validateJSON(t, compileLeadCreated(t), raw); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
}

func TestFormsModuleManifest(t *testing.T) {
	file := filepath.Join(root, "modules", "forms", "module.yaml")
	if ps := validateModule(file); len(ps) != 0 {
		t.Fatalf("modules/forms/module.yaml: %s", join(ps))
	}
	doc, ps := config.Load(file)
	if doc == nil {
		t.Fatal(join(ps))
	}
	m, err := config.DecodeModule(doc)
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider.Type != "inhouse" {
		t.Errorf("provider type %q, want inhouse", m.Provider.Type)
	}
	if len(m.DataClasses) == 0 {
		t.Fatal("data classes with retention must be declared")
	}
	for _, dc := range m.DataClasses {
		if dc.Retention == "" || dc.Sensitivity == "sensitive" {
			t.Errorf("data class %+v: retention required and not sensitive", dc)
		}
	}
	if m.Resources.RAMMB != 15 {
		t.Errorf("idle RAM target is 15 MB (SPEC section 6.1), manifest says %d", m.Resources.RAMMB)
	}
	var provides, publishes bool
	for _, p := range m.Provides {
		provides = provides || p.Capability == "leads"
	}
	for _, p := range m.Publishes {
		publishes = publishes || p.Event == "lead.created.v1"
	}
	if !provides || !publishes {
		t.Errorf("must provide leads and publish lead.created.v1 (provides=%v publishes=%v)", provides, publishes)
	}
	if !containsStr(m.Requires.Capabilities, "email.send") {
		t.Error("must require email.send")
	}
}

// The manifest's relative schema path must resolve to the schema the gateway is tested against.
func TestFormsManifestEventSchemaIsTheShippedOne(t *testing.T) {
	a, err := os.ReadFile(filepath.Join(root, "modules", "forms", "events", "lead.created.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, events.LeadCreatedV1) {
		t.Fatal("modules/forms/events/lead.created.v1.json differs from schemas/events/lead.created.v1.json")
	}
	raw, _ := os.ReadFile(filepath.Join(root, "modules", "forms", "module.yaml"))
	var m struct {
		Publishes []struct{ Schema string } `yaml:"publishes"`
		Settings  string                    `yaml:"settings_schema"`
	}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, rel := range append([]string{m.Settings}, m.Publishes[0].Schema) {
		if _, err := os.Stat(filepath.Join(root, "modules", "forms", rel)); err != nil {
			t.Errorf("manifest references %s: %v", rel, err)
		}
	}
}

// The module declares the ports the service defaults to.
func TestFormsManifestPortsMatchServiceDefaults(t *testing.T) {
	cfg, err := forms.ParseConfig([]byte("forms:\n  - {id: a, name: A, fields: [{name: n, type: text}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "modules", "forms", "module.yaml"))
	if !strings.Contains(string(raw), "port: "+strings.TrimPrefix(cfg.APIListen, ":")) {
		t.Errorf("module.yaml must expose the API on port %s", cfg.APIListen)
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// lead.erased.v1 carries ids and a time, nothing else, and the module ships and publishes it.
func TestLeadErasedSchema(t *testing.T) {
	doc, err := jsch.UnmarshalJSON(bytes.NewReader(events.LeadErasedV1))
	if err != nil {
		t.Fatal(err)
	}
	c := jsch.NewCompiler()
	c.DefaultDraft(jsch.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("lead.erased.v1.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("lead.erased.v1.json")
	if err != nil {
		t.Fatalf("schema does not compile: %v", err)
	}
	valid := `{"id":"evt_0123456789abcdef01234567","type":"lead.erased.v1","time":"2026-11-26T10:00:00Z","source":"forms","data":{"lead_id":"ld_0123456789abcdef01234567","form_id":"contact","erased_at":"2026-11-26T10:00:00Z"}}`
	if err := validateJSON(t, s, []byte(valid)); err != nil {
		t.Errorf("valid event rejected: %v", err)
	}
	if err := validateJSON(t, s, []byte(strings.Replace(valid, `"form_id"`, `"email":"a@b.example","form_id"`, 1))); err == nil {
		t.Error("personal data must not fit the schema")
	}
	shipped, err := os.ReadFile(filepath.Join(root, "modules", "forms", "events", "lead.erased.v1.json"))
	if err != nil || !bytes.Equal(shipped, events.LeadErasedV1) {
		t.Errorf("modules/forms/events/lead.erased.v1.json differs from schemas/events: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "modules", "forms", "module.yaml"))
	for _, want := range []string{"event: lead.erased.v1", "leads.erase"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("modules/forms/module.yaml does not declare %s", want)
		}
	}
}

// The image tag follows the module version: with an unchanged tag an upgraded release would keep
// the running pod, and an unchanged module version leaves the recorded manifest as it was.
func TestFormsVersionAndImageTagMoveTogether(t *testing.T) {
	version := asStr(readYAML(t, "modules/forms/module.yaml")["version"])
	chart := readYAML(t, "modules/forms/chart/Chart.yaml")
	image := asMap(readYAML(t, "modules/forms/chart/values.yaml")["image"])
	if version == "0.1.0" || asStr(chart["version"]) != version || asStr(chart["appVersion"]) != version || asStr(image["tag"]) != version {
		t.Errorf("module %s, chart %v/%v, image tag %v", version, chart["version"], chart["appVersion"], image["tag"])
	}
	for _, f := range []string{".github/workflows/restore-e2e.yml", ".github/workflows/module-routing.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, f))
		if err != nil || !strings.Contains(string(raw), "can-forms:"+version) {
			t.Errorf("%s must import can-forms:%s (%v)", f, version, err)
		}
		// what is imported has to be what is built: the build loop takes its tag from the chart
		if !strings.Contains(string(raw), `"modules/$m/chart/values.yaml"`) || !strings.Contains(string(raw), `can-$m:$tag"`) {
			t.Errorf("%s builds the module images under a tag of its own", f)
		}
	}
}
