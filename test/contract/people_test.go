package contract

// Epic E13a: the people module (directory + performance). Offline: manifest and schema contract
// tests, fixtures, the OpenAPI document against the routes the service really registers, and
// every event the service really emits validated against the published schemas.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/people"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/schemas/capabilities"
	"github.com/dodwmd/companyinacan/schemas/events"
)

var peopleEvents = map[string][]byte{
	"person.created.v1":           events.PersonCreatedV1,
	"person.status_changed.v1":    events.PersonStatusChangedV1,
	"person.offboarded.v1":        events.PersonOffboardedV1,
	"person.erased.v1":            events.PersonErasedV1,
	"oneonone.scheduled.v1":       events.OneOnOneScheduledV1,
	"goal.updated.v1":             events.GoalUpdatedV1,
	"checklist.task_completed.v1": events.ChecklistTaskCompletedV1,
}

func TestPeopleManifest(t *testing.T) {
	const rel = "modules/people/module.yaml"
	for _, p := range validateModule(filepath.Join(root, rel)) {
		if p.Severity != problem.Warning || p.Rule != "semantic.module-status" {
			t.Errorf("unexpected problem: %s", p)
		}
	}
	m := readYAML(t, rel)
	if m["status"] != "experimental" {
		t.Errorf("status = %v: the module is not verified on a cluster and must say so", m["status"])
	}
	if p := asMap(m["provider"]); p["type"] != "inhouse" {
		t.Errorf("provider = %v: sensitive data classes require an in-house provider", p)
	}
	// data classes: the manifest and the code must agree, sensitive ones must be marked.
	classes := map[string]string{}
	for _, dc := range asList(m["data_classes"]) {
		d := asMap(dc)
		classes[asStr(d["class"])] = asStr(d["sensitivity"])
		if asStr(d["retention"]) == "" {
			t.Errorf("class %v has no retention", d["class"])
		}
	}
	var code []string
	code = append(code, people.Classes...)
	sort.Strings(code)
	var man []string
	for c := range classes {
		man = append(man, c)
	}
	sort.Strings(man)
	if strings.Join(code, ",") != strings.Join(man, ",") {
		t.Errorf("manifest data classes %v differ from core/people Classes %v", man, code)
	}
	for _, c := range []string{"people-sensitive", "performance-oneonone", "performance-goals"} {
		if classes[c] != "sensitive" {
			t.Errorf("class %s must be sensitive", c)
		}
	}
	// manifest retention defaults equal the code defaults
	defaults := people.DefaultRetention()
	for _, dc := range asList(m["data_classes"]) {
		d := asMap(dc)
		r, err := people.ParseRetention(asStr(d["retention"]))
		if err != nil || r != defaults[asStr(d["class"])] {
			t.Errorf("class %v: manifest retention %v differs from code default %v (%v)", d["class"], d["retention"], defaults[asStr(d["class"])], err)
		}
	}
	// provides
	got := map[string]string{}
	for _, p := range asList(m["provides"]) {
		pm := asMap(p)
		got[asStr(pm["capability"])] = asStr(pm["schema"])
		if pm["port"] != 8081 || pm["service"] != "people-api" {
			t.Errorf("provides %v", pm)
		}
		if _, err := os.Stat(filepath.Join(root, "schemas", "capabilities", asStr(pm["schema"])+".json")); err != nil {
			t.Error(err)
		}
	}
	if got["directory"] != "directory.v1" || got["performance"] != "performance.v1" {
		t.Errorf("provides = %v", got)
	}
	// published events: each has a schema, the module copy equals the shared one
	pub := map[string]bool{}
	for _, p := range asList(m["publishes"]) {
		name := asStr(asMap(p)["event"])
		pub[name] = true
		local, err := os.ReadFile(filepath.Join(root, "modules", "people", "events", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		shared, err := os.ReadFile(filepath.Join(root, "schemas", "events", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(local, shared) {
			t.Errorf("modules/people/events/%s.json differs from schemas/events/%s.json", name, name)
		}
	}
	for name := range peopleEvents {
		if !pub[name] {
			t.Errorf("event %s is not listed in publishes", name)
		}
	}
	res := asMap(m["resources"])
	if res["ram_mb"] != 30 || res["ram_limit_mb"] != 64 {
		t.Errorf("resources = %v", res)
	}
	// every role grants only declared permissions
	declared := map[string]bool{}
	for _, p := range asList(m["permissions"]) {
		declared[asStr(p)] = true
	}
	for role, perms := range asMap(m["roles"]) {
		for _, p := range asList(perms) {
			if !declared[asStr(p)] {
				t.Errorf("role %s grants undeclared permission %v", role, p)
			}
		}
	}
	for _, r := range []string{"employee", "manager", "hr", "admin"} {
		if asMap(m["roles"])[r] == nil {
			t.Errorf("role %s missing", r)
		}
	}
	if len(asList(asMap(m["roles"])["manager"])) == 0 || containsAny(asList(asMap(m["roles"])["manager"]), "people.sensitive-read") {
		t.Error("managers must not be granted sensitive reads")
	}
}

func TestSensitiveClassesRequireInhouseStillEnforced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "people")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(root, "modules/people/module.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wrapped := strings.Replace(string(src), "type: inhouse\n  name: can-people", "type: oss-wrapper\n  name: Some HR App\n  licence: AGPL-3.0-only\n  licence_class: osi\n  upstream: https://example.com/hr", 1)
	if wrapped == string(src) {
		t.Fatal("test fixture did not change the provider")
	}
	f := filepath.Join(dir, "module.yaml")
	if err := os.WriteFile(f, []byte(wrapped), 0o644); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range validateModule(f) {
		if p.Rule == "semantic.sensitive-requires-inhouse" {
			found = true
		}
	}
	if !found {
		t.Error("a wrapped app holding people-sensitive data must be rejected")
	}
}

func TestPeopleEventFixtures(t *testing.T) {
	for name, raw := range peopleEvents {
		t.Run(name, func(t *testing.T) {
			s := compileSchema(t, name+".json", raw, "")
			counts := map[string]int{}
			for kind, wantValid := range map[string]bool{"valid": true, "invalid": false} {
				files, _ := filepath.Glob(filepath.Join(root, "testdata", "events", name, kind+"-*.json"))
				for _, f := range files {
					counts[kind]++
					b, err := os.ReadFile(f)
					if err != nil {
						t.Fatal(err)
					}
					err = validateJSON(t, s, b)
					if wantValid && err != nil {
						t.Errorf("%s should be valid: %v", filepath.Base(f), err)
					}
					if !wantValid && err == nil {
						t.Errorf("%s should be rejected", filepath.Base(f))
					}
				}
			}
			if counts["valid"] < 1 || counts["invalid"] < 8 {
				t.Errorf("fixtures: %v", counts)
			}
		})
	}
}

func TestPeopleCapabilityFixtures(t *testing.T) {
	for name, raw := range map[string][]byte{"directory.v1": capabilities.DirectoryV1, "performance.v1": capabilities.PerformanceV1} {
		t.Run(name, func(t *testing.T) {
			compileSchema(t, name+".json", raw, "")
			var top map[string]any
			if err := yaml.Unmarshal(raw, &top); err != nil {
				t.Fatal(err)
			}
			verbs, defs := asList(top["x-verbs"]), asMap(top["$defs"])
			if len(verbs) == 0 || asStr(top["x-capability"]) == "" {
				t.Fatal("x-capability and x-verbs are required")
			}
			for _, v := range verbs {
				vm := asMap(v)
				for _, k := range []string{"request", "response"} {
					if d := asStr(vm[k]); d != "" && defs[d] == nil {
						t.Errorf("verb %v names unknown definition %q", vm["verb"], d)
					}
				}
				if asStr(vm["method"]) != "GET" && vm["write"] != true {
					t.Errorf("verb %v: non-GET verbs must be flagged write", vm["verb"])
				}
				if asStr(vm["permission"]) == "" {
					t.Errorf("verb %v has no permission", vm["verb"])
				}
			}
			files, _ := filepath.Glob(filepath.Join(root, "testdata", "capabilities", name, "*.json"))
			seenValid := map[string]bool{}
			for _, f := range files {
				parts := strings.SplitN(strings.TrimSuffix(filepath.Base(f), ".json"), "-", 3)
				s := compileSchema(t, name+".json", raw, "#/$defs/"+parts[1])
				b, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				err = validateJSON(t, s, b)
				if parts[0] == "valid" {
					seenValid[parts[1]] = true
					if err != nil {
						t.Errorf("%s should be valid: %v", filepath.Base(f), err)
					}
				} else if err == nil {
					t.Errorf("%s should be rejected", filepath.Base(f))
				}
			}
			for _, v := range verbs {
				for _, k := range []string{"request", "response"} {
					if d := asStr(asMap(v)[k]); d != "" && !seenValid[d] {
						t.Errorf("definition %s has no valid fixture", d)
					}
				}
			}
		})
	}
}

// Every event the service really emits must satisfy its published schema.
func TestServiceEventsMatchSchemas(t *testing.T) {
	kr, err := people.ParseKeyring("k1 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n")
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	s, err := people.Open(filepath.Join(t.TempDir(), "p.db"), people.Options{Keyring: kr, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	hr := people.Actor{Subject: "svc", Roles: []string{people.RoleHR, people.RoleAdmin}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	tm, err := s.CreateOrgUnit(ctx, hr, "teams", people.OrgInput{Name: "Platform"})
	must(err)
	_, err = s.CreateTemplate(ctx, hr, people.TemplateInput{Name: "onboard", Kind: "onboarding", AutoStatus: "onboarding", Items: []people.TemplateItem{{Title: "accounts", AssigneeRole: "hr", Action: "identity.provision"}}})
	must(err)
	mgr, err := s.CreatePerson(ctx, hr, people.PersonInput{Kind: "employee", Status: people.StActive, GivenName: "Max", FamilyName: "Example", SSOSubject: "sso-max", TeamID: tm.ID})
	must(err)
	rep, err := s.CreatePerson(ctx, hr, people.PersonInput{Kind: "employee", GivenName: "Rae", FamilyName: "Example", SSOSubject: "sso-rae", ManagerID: mgr.ID})
	must(err)
	for _, st := range []people.Status{people.StOnboarding, people.StActive} {
		_, err = s.ChangeStatus(ctx, hr, rep.ID, st, "", "")
		must(err)
	}
	mgrA := people.Actor{Subject: "sso-max", Roles: []string{people.RoleEmployee, people.RoleManager}, PersonID: mgr.ID, HasPerson: true, Status: people.StActive}
	repA := people.Actor{Subject: "sso-rae", Roles: []string{people.RoleEmployee}, PersonID: rep.ID, HasPerson: true, Status: people.StActive}
	ser, err := s.CreateSeries(ctx, mgrA, rep.ID, "weekly")
	must(err)
	_, err = s.ScheduleMeeting(ctx, mgrA, ser.ID, clock.Add(24*time.Hour))
	must(err)
	co, err := s.CreateGoal(ctx, hr, people.GoalInput{Scope: "company", Title: "Ship"})
	must(err)
	g, err := s.CreateGoal(ctx, repA, people.GoalInput{Scope: "person", Title: "Learn", ParentID: co.ID})
	must(err)
	kr1, err := s.AddKeyResult(ctx, repA, g.ID, people.KeyResultInput{Title: "k", Start: 0, Target: 10})
	must(err)
	_, err = s.RecordProgress(ctx, repA, kr1.ID, 5, "")
	must(err)
	cls, err := s.ListChecklists(ctx, hr, rep.ID)
	must(err)
	if len(cls) != 1 {
		t.Fatalf("auto checklist missing: %v", cls)
	}
	_, err = s.CompleteTask(ctx, hr, cls[0].Tasks[0].ID)
	must(err)
	for _, st := range []people.Status{people.StOffboarding, people.StFormer} {
		_, err = s.ChangeStatus(ctx, hr, rep.ID, st, "", "")
		must(err)
	}
	_, err = s.RequestErasure(ctx, hr, rep.ID)
	must(err)
	evs, err := s.AllEvents(ctx)
	must(err)
	seen := map[string]int{}
	for _, ev := range evs {
		raw, ok := peopleEvents[ev.Type]
		if !ok {
			t.Errorf("service emits %s, which has no schema in this test", ev.Type)
			continue
		}
		seen[ev.Type]++
		b, _ := json.Marshal(ev)
		if err := validateJSON(t, compileSchema(t, ev.Type+".json", raw, ""), b); err != nil {
			t.Errorf("%s: %v\n%s", ev.Type, err, b)
		}
	}
	for name := range peopleEvents {
		if seen[name] == 0 {
			t.Errorf("the flow never produced %s", name)
		}
	}
}

// ---- OpenAPI ---------------------------------------------------------------------------------

func TestOpenAPIMatchesRoutes(t *testing.T) {
	var doc struct {
		OpenAPI string                          `yaml:"openapi"`
		Paths   map[string]map[string]yaml.Node `yaml:"paths"`
		Info    struct{ Description string }    `yaml:"info"`
		Sec     []map[string][]string           `yaml:"security"`
		Comp    struct {
			Schemas         map[string]yaml.Node `yaml:"schemas"`
			SecuritySchemes map[string]yaml.Node `yaml:"securitySchemes"`
		} `yaml:"components"`
	}
	b, err := os.ReadFile(filepath.Join(root, "specs/012-people-directory/contracts/people-api.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", doc.OpenAPI)
	}
	if !strings.Contains(strings.ToLower(doc.Info.Description), "not legal") {
		t.Error("the API description must carry the not-legal/HR/payroll-advice disclaimer")
	}
	if len(doc.Sec) == 0 || len(doc.Comp.SecuritySchemes) == 0 {
		t.Error("a global security requirement is needed (deny by default)")
	}
	want := map[string]bool{}
	for p, ops := range doc.Paths {
		for m := range ops {
			if m == "parameters" {
				continue
			}
			want[strings.ToUpper(m)+" "+p] = true
		}
	}
	api := &people.API{Svc: nil, Auth: nil, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_ = api.Handler()
	have := map[string]bool{}
	for _, r := range api.Routes() {
		have[r] = true
	}
	for r := range have {
		if !want[r] {
			t.Errorf("route %q is not documented in the OpenAPI contract", r)
		}
	}
	for r := range want {
		if !have[r] {
			t.Errorf("OpenAPI documents %q, which the service does not serve", r)
		}
	}
}

// ---- docs, chart, image ------------------------------------------------------------------------

func TestPeopleDocsCarryTheDisclaimer(t *testing.T) {
	for _, f := range []string{"docs/people.md", "modules/people/README.md", "specs/012-people-directory/spec.md"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ToLower(string(b))
		if !strings.Contains(s, "not legal") || !strings.Contains(s, "payroll") {
			t.Errorf("%s must say this is a tool, not legal, HR or payroll advice", f)
		}
	}
}

func TestPeopleChartValues(t *testing.T) {
	v := readYAML(t, "modules/people/chart/values.yaml")
	sc := asMap(v["securityContext"])
	if sc["runAsNonRoot"] != true || sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("securityContext = %v", sc)
	}
	if asMap(asMap(v["podSecurityContext"])["seccompProfile"])["type"] != "RuntimeDefault" {
		t.Error("seccompProfile RuntimeDefault required")
	}
	res := asMap(v["resources"])
	if mi(t, asStr(asMap(res["requests"])["memory"])) != 30 || mi(t, asStr(asMap(res["limits"])["memory"])) != 64 {
		t.Errorf("resources = %v do not match the manifest", res)
	}
	if !chartDigestOK(asStr(asMap(v["image"])["digest"])) {
		t.Error("image digest must be a sha256 or the REPLACE_WITH_ placeholder, never a guess")
	}
	if asMap(v["ingress"])["enabled"] != false {
		t.Error("people data is never routed from the internet")
	}
	b, _ := os.ReadFile(filepath.Join(root, "modules/people/chart/values.yaml"))
	if regexp.MustCompile(`(?i)(password|token):\s*["']?[A-Za-z0-9]{12,}`).Match(b) {
		t.Error("values.yaml appears to contain a secret value")
	}
}

func TestPeopleDockerfileFragment(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "deploy/people/Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"REPLACE_WITH_GOLANG_DIGEST", "REPLACE_WITH_DISTROLESS_STATIC_DIGEST", "CGO_ENABLED=0", "USER 65532:65532", "./cmd/can-people", "core/people.Version"} {
		if !strings.Contains(s, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	if strings.Contains(s, ":latest") {
		t.Error("Dockerfile uses a floating tag")
	}
}

func TestNamesKnowPeopleCapabilities(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "schemas/names.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"directory", "performance"} {
		if !strings.Contains(string(b), "name: "+n+" ") && !strings.Contains(string(b), "name: "+n+"}") {
			t.Errorf("schemas/names.yaml lacks %s", n)
		}
	}
}
