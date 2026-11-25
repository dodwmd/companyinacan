package contract

// Epic E17b: the board and entity modules (one binary, can-board). Offline: manifest and schema contract tests,
// fixtures, the OpenAPI document against the routes the service really registers, every event the service really
// emits validated against the published schemas, and the erasure tombstones accepted by the E08 ledger.
// A tool, not legal advice.

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

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/board"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/schemas/capabilities"
	"github.com/dodwmd/companyinacan/schemas/events"
)

var boardEvents = map[string]struct {
	raw    []byte
	module string
}{
	"board.meeting.scheduled.v1": {events.BoardMeetingScheduledV1, "board"},
	"board.resolution.passed.v1": {events.BoardResolutionPassedV1, "board"},
	"entity.filing.due.v1":       {events.EntityFilingDueV1, "entity"},
}

var boardCaps = map[string][]byte{
	"board.v1":       capabilities.BoardV1,
	"resolutions.v1": capabilities.ResolutionsV1,
	"entity.v1":      capabilities.EntityV1,
}

func boardManifest(t *testing.T, module string, wantClasses []string, wantRoles []string, wantCaps map[string]string) map[string]any {
	t.Helper()
	rel := "modules/" + module + "/module.yaml"
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
	res := asMap(m["resources"])
	if res["ram_mb"] != 20 || res["ram_limit_mb"] != 64 {
		t.Errorf("resources = %v", res)
	}
	classes := map[string]string{}
	defaults := board.DefaultRetention()
	for _, dc := range asList(m["data_classes"]) {
		d := asMap(dc)
		classes[asStr(d["class"])] = asStr(d["sensitivity"])
		r, err := board.ParseRetention(asStr(d["retention"]))
		if err != nil || r != defaults[asStr(d["class"])] {
			t.Errorf("class %v: manifest retention %v differs from code default %v (%v)", d["class"], d["retention"], defaults[asStr(d["class"])], err)
		}
	}
	var man []string
	for c := range classes {
		man = append(man, c)
	}
	want := append([]string(nil), wantClasses...)
	sort.Strings(man)
	sort.Strings(want)
	if strings.Join(man, ",") != strings.Join(want, ",") {
		t.Errorf("manifest data classes %v differ from core/board %v", man, want)
	}
	for c, s := range classes {
		if !strings.HasSuffix(c, "-audit") && !strings.HasSuffix(c, "-filings") && s != "sensitive" {
			t.Errorf("class %s must be sensitive", c)
		}
	}
	got := map[string]string{}
	for _, p := range asList(m["provides"]) {
		pm := asMap(p)
		got[asStr(pm["capability"])] = asStr(pm["schema"])
		if pm["port"] != 8082 || pm["service"] != "board-api" {
			t.Errorf("provides %v", pm)
		}
		if _, err := os.Stat(filepath.Join(root, "schemas", "capabilities", asStr(pm["schema"])+".json")); err != nil {
			t.Error(err)
		}
	}
	if len(got) != len(wantCaps) {
		t.Errorf("provides = %v, want %v", got, wantCaps)
	}
	for c, s := range wantCaps {
		if got[c] != s {
			t.Errorf("provides = %v, want %v", got, wantCaps)
		}
	}
	prefix := module + "."
	declared := map[string]bool{}
	for _, p := range asList(m["permissions"]) {
		declared[asStr(p)] = true
		if !strings.HasPrefix(asStr(p), prefix) {
			t.Errorf("permission %v is outside the module namespace", p)
		}
	}
	roles := asMap(m["roles"])
	for role, perms := range roles {
		for _, p := range asList(perms) {
			if !declared[asStr(p)] {
				t.Errorf("role %s grants undeclared permission %v", role, p)
			}
		}
	}
	for _, r := range wantRoles {
		if roles[r] == nil {
			t.Errorf("role %s missing", r)
		}
	}
	for _, r := range []string{"owner", "operator", "auditor"} {
		if roles[r] != nil {
			t.Errorf("role %s is reserved for can-core", r)
		}
	}
	pub := map[string]bool{}
	for _, p := range asList(m["publishes"]) {
		name := asStr(asMap(p)["event"])
		pub[name] = true
		local, err := os.ReadFile(filepath.Join(root, "modules", module, "events", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		shared, err := os.ReadFile(filepath.Join(root, "schemas", "events", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(local, shared) {
			t.Errorf("modules/%s/events/%s.json differs from schemas/events/%s.json", module, name, name)
		}
	}
	for name, e := range boardEvents {
		if e.module == module && !pub[name] {
			t.Errorf("event %s is not listed in publishes", name)
		}
	}
	return m
}

func TestBoardManifest(t *testing.T) {
	m := boardManifest(t, "board", board.BoardClasses,
		[]string{"director", "officer", "secretary", "admin", "observer", "audit-viewer", "employee", "integration"},
		map[string]string{"board": "board.v1", "resolutions": "resolutions.v1"})
	roles := asMap(m["roles"])
	// board materials are confidential: the admin sees structure only, the integration role asks for resolutions and nothing else
	for _, p := range []string{"board.sensitive-read", "board.write", "board.vote", "board.approve", "board.minutes"} {
		if containsAny(asList(roles["admin"]), p) {
			t.Errorf("admin must not hold %s", p)
		}
	}
	if l := asList(roles["integration"]); len(l) != 1 || asStr(l[0]) != "board.integrate" {
		t.Errorf("integration = %v", l)
	}
	for _, r := range []string{"employee", "observer", "officer"} {
		for _, p := range []string{"board.write", "board.vote", "board.approve", "board.minutes", "board.admin"} {
			if containsAny(asList(roles[r]), p) {
				t.Errorf("role %s must not hold %s", r, p)
			}
		}
	}
	types := map[string][]any{}
	for _, a := range asList(m["approvals"]) {
		am := asMap(a)
		types[asStr(am["type"])] = asList(am["default_chain"])
	}
	if len(types["resolution"]) == 0 {
		t.Errorf("approvals = %v: the `resolution` type (board.resolution) must be declared", types)
	}
	// every verb of both capabilities names a permission the manifest declares
	declared := map[string]bool{}
	for _, p := range asList(m["permissions"]) {
		declared[asStr(p)] = true
	}
	for _, name := range []string{"board.v1", "resolutions.v1"} {
		var top map[string]any
		if err := yaml.Unmarshal(boardCaps[name], &top); err != nil {
			t.Fatal(err)
		}
		for _, v := range asList(top["x-verbs"]) {
			if p := asStr(asMap(v)["permission"]); !declared[p] {
				t.Errorf("%s verb %v needs undeclared permission %s", name, asMap(v)["verb"], p)
			}
		}
	}
}

func TestEntityManifest(t *testing.T) {
	m := boardManifest(t, "entity", board.EntityClasses,
		[]string{"secretary", "admin", "officer", "audit-viewer", "director"},
		map[string]string{"entity": "entity.v1"})
	if containsAny(asList(asMap(m["roles"])["admin"]), "entity.sensitive-read") {
		t.Error("admin must not read identifiers or register detail")
	}
	declared := map[string]bool{}
	for _, p := range asList(m["permissions"]) {
		declared[asStr(p)] = true
	}
	var top map[string]any
	if err := yaml.Unmarshal(capabilities.EntityV1, &top); err != nil {
		t.Fatal(err)
	}
	for _, v := range asList(top["x-verbs"]) {
		if p := asStr(asMap(v)["permission"]); !declared[p] {
			t.Errorf("entity verb %v needs undeclared permission %s", asMap(v)["verb"], p)
		}
	}
}

func TestBoardAndEntityClassesAreTheCodeClasses(t *testing.T) {
	all := append(append([]string(nil), board.BoardClasses...), board.EntityClasses...)
	sort.Strings(all)
	code := append([]string(nil), board.Classes...)
	sort.Strings(code)
	if strings.Join(all, ",") != strings.Join(code, ",") {
		t.Errorf("module classes %v differ from Classes %v", all, code)
	}
	for _, c := range []string{"board-minutes", "board-resolutions", "board-audit", "entity-register", "entity-filings", "entity-audit"} {
		if !board.DefaultRetention()[c].Legal {
			t.Errorf("%s must default to legal retention", c)
		}
	}
}

func TestBoardAndEntitySensitiveClassesRequireInhouse(t *testing.T) {
	for _, mod := range []string{"board", "entity"} {
		dir := filepath.Join(t.TempDir(), mod)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		src, err := os.ReadFile(filepath.Join(root, "modules", mod, "module.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		wrapped := strings.Replace(string(src), "type: inhouse\n  name: can-board", "type: oss-wrapper\n  name: Some Board App\n  licence: AGPL-3.0-only\n  licence_class: osi\n  upstream: https://example.com/board", 1)
		if wrapped == string(src) {
			t.Fatalf("test fixture did not change the provider of %s", mod)
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
			t.Errorf("a wrapped app holding %s data must be rejected", mod)
		}
	}
}

func TestBoardEventFixtures(t *testing.T) {
	for name, e := range boardEvents {
		t.Run(name, func(t *testing.T) {
			s := compileSchema(t, name+".json", e.raw, "")
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

func TestBoardCapabilityFixtures(t *testing.T) {
	for name, raw := range boardCaps {
		t.Run(name, func(t *testing.T) {
			compileSchema(t, name+".json", raw, "")
			var top map[string]any
			if err := yaml.Unmarshal(raw, &top); err != nil {
				t.Fatal(err)
			}
			verbs, defs := asList(top["x-verbs"]), asMap(top["$defs"])
			if len(verbs) == 0 || asStr(top["x-capability"]) != strings.TrimSuffix(name, ".v1") {
				t.Fatal("x-capability and x-verbs are required")
			}
			desc := strings.ToLower(asStr(top["description"]))
			if !strings.Contains(desc, "not legal advice") || !strings.Contains(desc, "not implemented") {
				t.Error("the capability description must carry the disclaimer and say jurisdiction rules are not implemented")
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

// ---- a real flow through the service --------------------------------------------------------------

func boardDrive(t *testing.T) (*board.Service, board.Actor) {
	t.Helper()
	kr, err := board.ParseKeyring("k1 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n")
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	s, err := board.Open(filepath.Join(t.TempDir(), "b.db"), board.Options{Keyring: kr, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	const (
		p1 = "pers_00000000000000000000d001"
		p2 = "pers_00000000000000000000d002"
		p3 = "pers_00000000000000000000d003"
	)
	sec := board.Actor{Subject: "sec-sam", Roles: []string{board.RoleSecretary}, PersonID: "pers_00000000000000000000c001"}
	adm := board.Actor{Subject: "svc-admin", Roles: []string{board.RoleAdmin}}
	d1 := board.Actor{Subject: "sso-d1", Roles: []string{board.RoleDirector}, PersonID: p1}
	d2 := board.Actor{Subject: "sso-d2", Roles: []string{board.RoleDirector}, PersonID: p2}
	d3 := board.Actor{Subject: "sso-d3", Roles: []string{board.RoleDirector}, PersonID: p3}
	integ := board.Actor{Subject: "svc-can-core", Roles: []string{board.RoleIntegration}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := s.CreateBody(ctx, sec, board.BodyInput{Name: "Board of Directors", Kind: "board", NoticeDays: 7})
	must(err)
	for i, p := range []string{p1, p2, p3} {
		role := "member"
		if i == 0 {
			role = "chair"
		}
		_, err := s.AddMember(ctx, sec, b.ID, board.MemberInput{PersonID: p, Role: role, StartDate: "2025-01-01"})
		must(err)
	}
	mt, err := s.ScheduleMeeting(ctx, sec, b.ID, board.MeetingInput{Title: "Q1 board meeting", StartsOn: "2026-03-10", NoticeGivenOn: "2026-02-20"})
	must(err)
	for _, p := range []string{p1, p2, p3} {
		_, err := s.RecordAttendance(ctx, sec, mt.ID, p, "present")
		must(err)
	}
	// a resolution requested by another module (the equity seam), run and passed
	o, err := s.RequestResolution(ctx, integ, board.ResolutionRequest{Type: "equity.grant", Ref: "grt_0123456789abcdef01234567", BodyID: b.ID, Requester: "hr-alice"})
	must(err)
	_, err = s.AmendResolution(ctx, sec, o.ResolutionID, board.AmendInput{Title: "Approve option grant", Text: "RESOLVED that the grant is approved.", MeetingID: mt.ID})
	must(err)
	_, err = s.ProposeResolution(ctx, sec, o.ResolutionID)
	must(err)
	_, err = s.OpenVote(ctx, d1, o.ResolutionID)
	must(err)
	for _, a := range []board.Actor{d1, d2, d3} {
		_, err = s.CastVote(ctx, a, o.ResolutionID, board.VoteInput{Choice: "for"})
		must(err)
	}
	r, err := s.CloseVote(ctx, d1, o.ResolutionID)
	must(err)
	if r.Status != board.ResPassed {
		t.Fatalf("resolution = %+v", r)
	}
	// a filing that comes due
	le, err := s.CreateLegalEntity(ctx, sec, board.LegalEntityInput{Name: "Example Pty Ltd", RegNumber: "000 111 222"})
	must(err)
	_, err = s.CreateObligation(ctx, sec, le.ID, board.ObligationInput{Name: "Annual return", FirstDueOn: "2026-03-31", Recurrence: "annual", LeadDays: []int{30, 7, 0}})
	must(err)
	if n, err := s.ApplyDue(ctx); err != nil || n != 1 {
		t.Fatalf("ApplyDue = %d, %v", n, err)
	}
	return s, adm
}

// Every event the service really emits must satisfy its published schema.
func TestBoardServiceEventsMatchSchemas(t *testing.T) {
	s, _ := boardDrive(t)
	evs, err := s.AllEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, ev := range evs {
		e, ok := boardEvents[ev.Type]
		if !ok {
			t.Errorf("service emits %s, which has no schema in this test", ev.Type)
			continue
		}
		seen[ev.Type]++
		b, _ := json.Marshal(ev)
		if err := validateJSON(t, compileSchema(t, ev.Type+".json", e.raw, ""), b); err != nil {
			t.Errorf("%s: %v\n%s", ev.Type, err, b)
		}
		if ev.Source != e.module {
			t.Errorf("%s has source %q, want %q", ev.Type, ev.Source, e.module)
		}
	}
	for name := range boardEvents {
		if seen[name] == 0 {
			t.Errorf("the flow never produced %s", name)
		}
	}
}

// ---- OpenAPI ------------------------------------------------------------------------------------------

func TestBoardOpenAPIMatchesRoutes(t *testing.T) {
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
	b, err := os.ReadFile(filepath.Join(root, "specs/029-board-entity/contracts/board-api.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", doc.OpenAPI)
	}
	d := strings.ToLower(doc.Info.Description)
	if !strings.Contains(d, "not legal advice") || !strings.Contains(d, "not implemented") {
		t.Error("the API description must carry the disclaimer")
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
	api := &board.API{Svc: nil, Auth: nil, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
	for _, ref := range regexp.MustCompile(`#/components/schemas/([A-Za-z]+)`).FindAllStringSubmatch(string(b), -1) {
		if _, ok := doc.Comp.Schemas[ref[1]]; !ok {
			t.Errorf("unresolved schema reference %s", ref[1])
		}
	}
	if !strings.Contains(string(b), "Idempotency-Key") {
		t.Error("the create-resolution route must document Idempotency-Key")
	}
	for _, code := range []string{"401", "403", "404", "409", "429"} {
		if !strings.Contains(string(b), `"`+code+`"`) {
			t.Errorf("error response %s is not documented", code)
		}
	}
}

// Every verb of the capability contracts names a route the service serves.
func TestBoardCapabilityVerbsAreServed(t *testing.T) {
	api := &board.API{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_ = api.Handler()
	have := map[string]bool{}
	for _, r := range api.Routes() {
		have[r] = true
	}
	verbRoutes := map[string]bool{}
	for name, raw := range boardCaps {
		var top map[string]any
		if err := yaml.Unmarshal(raw, &top); err != nil {
			t.Fatal(err)
		}
		for _, v := range asList(top["x-verbs"]) {
			vm := asMap(v)
			r := asStr(vm["method"]) + " " + asStr(vm["path"])
			verbRoutes[r] = true
			if !have[r] {
				t.Errorf("%s verb %v is not served (%s)", name, vm["verb"], r)
			}
		}
	}
	// and every served route belongs to a capability
	for r := range have {
		if r != "GET /healthz" && !verbRoutes[r] {
			t.Errorf("route %q is in no capability contract", r)
		}
	}
}

// ---- docs, chart ----------------------------------------------------------------------------------------

func TestBoardChartValues(t *testing.T) {
	for _, mod := range []string{"board", "entity"} {
		v := readYAML(t, "modules/"+mod+"/chart/values.yaml")
		sc := asMap(v["securityContext"])
		if sc["runAsNonRoot"] != true || sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
			t.Errorf("%s securityContext = %v", mod, sc)
		}
		if asMap(asMap(v["podSecurityContext"])["seccompProfile"])["type"] != "RuntimeDefault" {
			t.Errorf("%s: seccompProfile RuntimeDefault required", mod)
		}
		res := asMap(v["resources"])
		if mi(t, asStr(asMap(res["requests"])["memory"])) != 20 || mi(t, asStr(asMap(res["limits"])["memory"])) != 64 {
			t.Errorf("%s resources = %v do not match the manifest", mod, res)
		}
		if asStr(asMap(v["image"])["repository"]) != "ghcr.io/dodwmd/can-board" {
			t.Errorf("%s image = %v", mod, v["image"])
		}
		b, _ := os.ReadFile(filepath.Join(root, "modules", mod, "chart/values.yaml"))
		if regexp.MustCompile(`(?i)(password|token):\s*["']?[A-Za-z0-9]{12,}`).Match(b) {
			t.Errorf("%s values.yaml appears to contain a secret value", mod)
		}
	}
}

func TestBoardNamesAndCatalogue(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "schemas/names.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"board", "resolutions", "entity"} {
		if !strings.Contains(string(b), "name: "+n+" ") && !strings.Contains(string(b), "name: "+n+"}") {
			t.Errorf("schemas/names.yaml lacks %s", n)
		}
	}
}

// The erasure tombstones must be accepted by the real E08 ledger.
func TestBoardErasureTombstonesFitTheBackupLedger(t *testing.T) {
	s, admin := boardDrive(t)
	ctx := context.Background()
	const person = "pers_00000000000000000000e001"
	if _, err := s.RequestErasure(ctx, admin, person); err != nil {
		t.Fatal(err)
	}
	tombs, err := s.ListErasures(ctx, admin, 0)
	if err != nil || len(tombs) != len(board.PurgeClasses) {
		t.Fatalf("tombstones = %v, %v", tombs, err)
	}
	l := &backup.Ledger{Path: filepath.Join(t.TempDir(), "ledger.jsonl")}
	for _, e := range tombs {
		if _, added, err := l.Append(e.Subject, e.Class); err != nil || !added {
			t.Fatalf("ledger rejected %+v: added=%v err=%v", e, added, err)
		}
	}
	es, err := l.Read()
	if err != nil || len(es) != len(tombs) || backup.VerifyChain(es) != nil {
		t.Fatalf("ledger = %v, %v", es, err)
	}
}
