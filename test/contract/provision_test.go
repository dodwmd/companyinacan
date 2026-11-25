package contract

// Epic E13d: the provisioning module (identity.provision, SCIM server). Offline: manifest and schema
// contract tests, fixtures, the OpenAPI document against the routes the service really registers,
// and every event and run the engine really produces validated against the published schemas.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/provision"
	"github.com/dodwmd/companyinacan/schemas/capabilities"
	"github.com/dodwmd/companyinacan/schemas/events"
)

var provisionEvents = map[string][]byte{
	"provision.step.completed.v1": events.ProvisionStepCompletedV1,
	"provision.failed.v1":         events.ProvisionFailedV1,
}

func TestProvisioningManifest(t *testing.T) {
	const rel = "modules/provisioning/module.yaml"
	for _, p := range validateModule(filepath.Join(root, rel)) {
		if p.Severity != problem.Warning || p.Rule != "semantic.module-status" {
			t.Errorf("unexpected problem: %s", p)
		}
	}
	m := readYAML(t, rel)
	if m["status"] != "experimental" {
		t.Errorf("status = %v: nothing here has run against a real service or cluster and the manifest must say so", m["status"])
	}
	if asMap(m["provider"])["type"] != "inhouse" {
		t.Errorf("provider = %v", m["provider"])
	}
	prov := asList(m["provides"])
	if len(prov) != 1 || asMap(prov[0])["capability"] != "identity.provision" || asMap(prov[0])["schema"] != "identity.provision.v1" || asMap(prov[0])["port"] != 8090 {
		t.Errorf("provides = %v", prov)
	}
	if _, err := os.Stat(filepath.Join(root, "schemas", "capabilities", "identity.provision.v1.json")); err != nil {
		t.Error(err)
	}
	// published events: a schema each, the module copy equals the shared one
	pub := map[string]bool{}
	for _, p := range asList(m["publishes"]) {
		name := asStr(asMap(p)["event"])
		pub[name] = true
		local, err := os.ReadFile(filepath.Join(root, "modules", "provisioning", "events", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		shared, err := os.ReadFile(filepath.Join(root, "schemas", "events", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(local, shared) {
			t.Errorf("modules/provisioning/events/%s.json differs from schemas/events/%s.json", name, name)
		}
	}
	for name := range provisionEvents {
		if !pub[name] {
			t.Errorf("event %s is not listed in publishes", name)
		}
	}
	// the workflows start from people events only; candidate.hired is NOT subscribed (it carries no person id)
	subs := map[string]bool{}
	for _, s := range asList(m["subscribes"]) {
		sm := asMap(s)
		subs[asStr(sm["event"])] = true
		if sm["webhook"] != "/v1/can/events" || sm["port"] != 8090 {
			t.Errorf("subscription %v", sm)
		}
	}
	for _, e := range []string{"person.created.v1", "person.status_changed.v1", "person.offboarded.v1", "checklist.task_completed.v1", "approval.decided.v1"} {
		if !subs[e] {
			t.Errorf("not subscribed to %s", e)
		}
	}
	if subs["candidate.hired.v1"] {
		t.Error("candidate.hired.v1 must not start a joiner: the person is created by the hire hand-off, and person.created.v1 starts it")
	}
	// approval types the engine requests must be declared, or can-core refuses the request
	declared := map[string]bool{}
	for _, a := range asList(m["approvals"]) {
		declared[asStr(asMap(a)["type"])] = true
	}
	for _, ty := range []string{"privileged-access", "privileged-leaver"} {
		if !declared[ty] {
			t.Errorf("approval type %s is not declared", ty)
		}
	}
	res := asMap(m["resources"])
	if res["ram_mb"] != 20 || res["ram_limit_mb"] != 64 {
		t.Errorf("resources = %v", res)
	}
	perms := map[string]bool{}
	for _, p := range asList(m["permissions"]) {
		perms[asStr(p)] = true
	}
	for _, p := range []string{"provision.read", "provision.write", "provision.admin", "scim.read", "scim.write"} {
		if !perms[p] {
			t.Errorf("permission %s missing", p)
		}
	}
	for role, ps := range asMap(m["roles"]) {
		for _, p := range asList(ps) {
			if !perms[asStr(p)] {
				t.Errorf("role %s grants undeclared permission %v", role, p)
			}
		}
	}
	if containsAny(asList(asMap(m["roles"])["it-reader"]), "provision.write") || containsAny(asList(asMap(m["roles"])["it-reader"]), "provision.admin") {
		t.Error("the read-only role must not write")
	}
	for _, dc := range asList(m["data_classes"]) {
		if asStr(asMap(dc)["retention"]) == "" {
			t.Errorf("class %v has no retention", asMap(dc)["class"])
		}
	}
	if asMap(m["runtime"])["subdomain"] != "scim" {
		t.Errorf("runtime = %v: the SCIM endpoint needs a public host", m["runtime"])
	}
	names := readYAML(t, "schemas/names.yaml")
	found := false
	for _, c := range asList(names["capabilities"]) {
		found = found || asMap(c)["name"] == "identity.provision"
	}
	if !found {
		t.Error("identity.provision is not in schemas/names.yaml")
	}
}

func TestProvisionEventFixtures(t *testing.T) {
	for name, raw := range provisionEvents {
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
			if counts["valid"] < 2 || counts["invalid"] < 12 {
				t.Errorf("fixtures: %v", counts)
			}
		})
	}
}

// The offboarded event gained an optional subject_ref (a hash prefix, never the subject).
func TestOffboardedEventSubjectRefFixtures(t *testing.T) {
	s := compileSchema(t, "person.offboarded.v1.json", events.PersonOffboardedV1, "")
	env := func(ref any) []byte {
		d := map[string]any{"party_id": "pers_0123456789abcdef01234567", "kind": "employee", "effective_date": "2026-06-10"}
		if ref != nil {
			d["subject_ref"] = ref
		}
		b, _ := json.Marshal(map[string]any{"id": "evt_0123456789abcdef01234567", "type": "person.offboarded.v1", "time": "2026-06-10T09:00:00Z", "source": "people", "data": d})
		return b
	}
	for _, ok := range []any{nil, strings.Repeat("a", 32)} {
		if err := validateJSON(t, s, env(ok)); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range []any{"user:sso-alice", strings.Repeat("a", 31), strings.Repeat("A", 32), strings.Repeat("a", 64), 7} {
		if err := validateJSON(t, s, env(bad)); err == nil {
			t.Errorf("subject_ref %v accepted", bad)
		}
	}
}

func TestProvisionCapabilityFixtures(t *testing.T) {
	const name = "identity.provision.v1"
	raw := capabilities.IdentityProvisionV1
	compileSchema(t, name+".json", raw, "")
	var top map[string]any
	if err := yaml.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	verbs, defs := asList(top["x-verbs"]), asMap(top["$defs"])
	if len(verbs) == 0 || asStr(top["x-capability"]) != "identity.provision" {
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
}

// ---- real engine output against the schemas ----------------------------------------------------

type ctDir map[string]provision.Person

func (d ctDir) Get(_ context.Context, id string) (provision.Person, error) {
	if p, ok := d[id]; ok {
		return p, nil
	}
	return provision.Person{}, provision.ErrNotFound
}
func (d ctDir) List(context.Context, provision.ListQuery) ([]provision.Person, string, error) {
	return nil, "", nil
}
func (d ctDir) Update(context.Context, string, provision.PersonPatch) (provision.Person, error) {
	return provision.Person{}, provision.ErrNotFound
}

type ctConn struct {
	name string
	fail bool
}

func (c *ctConn) Name() string   { return c.name }
func (c *ctConn) Verified() bool { return false }
func (c *ctConn) CreateAccount(_ context.Context, _ string, id provision.Identity) (string, error) {
	return "ext-" + id.PartyID[5:9], nil
}
func (c *ctConn) UpdateAccount(context.Context, string, string, provision.Identity) error { return nil }
func (c *ctConn) DisableAccount(context.Context, string, string) error                    { return nil }
func (c *ctConn) DeleteAccount(context.Context, string, string) error                     { return nil }
func (c *ctConn) AddToGroup(_ context.Context, _, _, _ string) error {
	if c.fail {
		return fmt.Errorf("nope")
	}
	return nil
}
func (c *ctConn) RemoveFromGroup(context.Context, string, string, string) error { return nil }

func TestEngineOutputMatchesSchemas(t *testing.T) {
	const alice = "pers_aaaaaaaaaaaaaaaaaaaaaaaa"
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	st, err := provision.OpenStore(":memory:", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfg, err := provision.ParseConfig([]byte(`
directory: { url: "http://people.example:8081", allow_insecure_http: true }
retry: { max_attempts: 1 }
connectors:
  - { name: wiki, type: scim-client, url: "https://wiki.example.com/scim/v2", token_env: T, default_groups: [all-staff] }
`))
	if err != nil {
		t.Fatal(err)
	}
	pol, _ := cfg.Policy()
	dir := ctDir{alice: {ID: alice, Kind: "employee", Status: "active", GivenName: "Alice", FamilyName: "Ng", WorkEmail: "alice@example.com"}}
	cs, _ := provision.NewConnectorSet(&ctConn{name: "wiki", fail: true})
	eng := &provision.Engine{Store: st, Dir: dir, Conns: cs, Policy: pol, Now: func() time.Time { return now }}
	run, _, err := eng.Start(context.Background(), provision.StartRequest{Kind: provision.KindJoiner, PartyID: alice, Trigger: "api", Key: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := eng.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// a created account, then a dead-lettered group add: both events are published
	items, err := st.PendingEvents(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, it := range items {
		seen[it.Type] = true
		raw, ok := provisionEvents[it.Type]
		if !ok {
			t.Fatalf("unexpected event type %s", it.Type)
		}
		env, _ := json.Marshal(map[string]any{"id": it.ID, "type": it.Type, "time": now.Format(time.RFC3339), "source": "provisioning", "data": json.RawMessage(it.Data)})
		if err := validateJSON(t, compileSchema(t, it.Type+".json", raw, ""), env); err != nil {
			t.Errorf("%s: %v\n%s", it.Type, err, env)
		}
		for _, bad := range []string{"alice@example.com", "Alice", "all-staff"} {
			if strings.Contains(string(it.Data), bad) {
				t.Errorf("%s carries %q", it.Type, bad)
			}
		}
	}
	for ty := range provisionEvents {
		if !seen[ty] {
			t.Errorf("the engine never produced %s", ty)
		}
	}
	// the run as the API returns it satisfies the capability's Run definition
	full, err := st.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(full)
	if err := validateJSON(t, compileSchema(t, "identity.provision.v1.json", capabilities.IdentityProvisionV1, "#/$defs/Run"), b); err != nil {
		t.Errorf("run: %v\n%s", err, b)
	}
	plan, err := eng.Plan(context.Background(), provision.StartRequest{Kind: provision.KindMover, PartyID: alice})
	_ = plan
	_ = err // no account yet: a conflict is the right answer, the shape of a real plan is checked in core/provision
}

// ---- OpenAPI ------------------------------------------------------------------------------------

func TestProvisionOpenAPIMatchesRoutes(t *testing.T) {
	var doc struct {
		OpenAPI string                          `yaml:"openapi"`
		Paths   map[string]map[string]yaml.Node `yaml:"paths"`
		Info    struct{ Description string }    `yaml:"info"`
		Sec     []map[string][]string           `yaml:"security"`
	}
	b, err := os.ReadFile(filepath.Join(root, "specs/028-identity-provision/contracts/openapi.yaml"))
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
		t.Error("the API description must carry the not-legal/security-advice disclaimer")
	}
	if len(doc.Sec) == 0 {
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
	have := map[string]bool{}
	for _, r := range provision.Routes() {
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
	// every provisioning route (except health and the signed event endpoint) names its permission
	for p, ops := range doc.Paths {
		if !strings.HasPrefix(p, "/v1/provision/") {
			continue
		}
		for m, node := range ops {
			if m == "parameters" {
				continue
			}
			var op struct {
				Perm string `yaml:"x-permission"`
			}
			_ = node.Decode(&op)
			if op.Perm == "" {
				t.Errorf("%s %s has no x-permission", m, p)
			}
		}
	}
}

// ---- docs, chart, image ------------------------------------------------------------------------

func TestProvisioningDocsAreHonest(t *testing.T) {
	for _, f := range []string{"docs/provisioning.md", "modules/provisioning/README.md", "specs/028-identity-provision/spec.md"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ToLower(string(b))
		if !strings.Contains(s, "not legal") || !strings.Contains(s, "unverified") {
			t.Errorf("%s must say this is a tool, not legal or security advice, and list what is unverified", f)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, "docs/provisioning.md"))
	for _, want := range []string{"google-workspace", "github", "slack", "NetworkPolicy", "candidate.hired", "/v1/identity/revoke"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("docs/provisioning.md does not mention %s", want)
		}
	}
}

func TestProvisioningChartValues(t *testing.T) {
	v := readYAML(t, "modules/provisioning/chart/values.yaml")
	sc := asMap(v["securityContext"])
	if sc["runAsNonRoot"] != true || sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("securityContext = %v", sc)
	}
	if asMap(asMap(v["podSecurityContext"])["seccompProfile"])["type"] != "RuntimeDefault" {
		t.Error("seccompProfile RuntimeDefault required")
	}
	res := asMap(v["resources"])
	if mi(t, asStr(asMap(res["requests"])["memory"])) != 20 || mi(t, asStr(asMap(res["limits"])["memory"])) != 64 {
		t.Errorf("resources = %v do not match the manifest", res)
	}
	cfg := asMap(v["config"])
	if asMap(cfg["workflows"])["dry_run"] != true {
		t.Error("the chart must start in dry-run: a first install must not touch downstream accounts")
	}
	ing := asMap(v["ingress"])
	for _, p := range asList(ing["pathPrefixes"]) {
		if asStr(p) != "/scim/v2" {
			t.Errorf("ingress routes %v: only /scim/v2 may be public", p)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, "deploy/provisioning/Dockerfile"))
	if !strings.Contains(string(b), "USER 65532:65532") || !strings.Contains(string(b), "REPLACE_WITH_GOLANG_DIGEST") {
		t.Error("the Dockerfile must run as 65532 and ship digest placeholders")
	}
}
