package contract

// Epic E14a: the work tracker. Everything here is offline: the manifest is validated with
// `can validate`'s code, event schemas are exercised with fixtures and with events a real
// can-work instance emits, and the OpenAPI contract is checked against the router and the
// capability schemas so none of the three can drift.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/work"
	"github.com/dodwmd/companyinacan/schemas"
	"github.com/dodwmd/companyinacan/schemas/capabilities"
	"github.com/dodwmd/companyinacan/schemas/events"
)

var workEvents = map[string][]byte{
	"work.issue.created.v1":      events.WorkIssueCreatedV1,
	"work.issue.updated.v1":      events.WorkIssueUpdatedV1,
	"work.issue.transitioned.v1": events.WorkIssueTransitionedV1,
	"work.comment.created.v1":    events.WorkCommentCreatedV1,
}

func TestWorkManifest(t *testing.T) {
	ps := validateModule(filepath.Join(root, "modules", "work", "module.yaml"))
	for _, p := range ps {
		if p.Severity == problem.Error {
			t.Errorf("manifest error: %s", p)
		}
	}
	m := readYAML(t, "modules/work/module.yaml")
	if m["status"] != "experimental" {
		t.Errorf("status = %v: the module is not verified on a cluster, so it must say experimental", m["status"])
	}
	if asMap(m["provider"])["type"] != "inhouse" {
		t.Error("work is in-house")
	}
	var caps []string
	for _, p := range asList(m["provides"]) {
		pm := asMap(p)
		caps = append(caps, asStr(pm["capability"])+"="+asStr(pm["schema"]))
		if _, err := os.Stat(filepath.Join(root, "schemas", "capabilities", asStr(pm["schema"])+".json")); err != nil {
			t.Errorf("provided schema %v has no file: %v", pm["schema"], err)
		}
	}
	sort.Strings(caps)
	if strings.Join(caps, " ") != "projects=projects.v1 work=work.v1" {
		t.Errorf("provides = %v", caps)
	}
	perms := map[string]bool{}
	for _, p := range asList(m["permissions"]) {
		perms[asStr(p)] = true
	}
	for _, p := range []string{"work.read", "work.write", "work.admin"} {
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
	if len(asList(m["data_classes"])) == 0 {
		t.Error("data_classes with retention are required")
	}
	pubs := asList(m["publishes"])
	if len(pubs) != len(workEvents) {
		t.Fatalf("publishes %d events, want %d", len(pubs), len(workEvents))
	}
	for _, p := range pubs {
		pm := asMap(p)
		name := asStr(pm["event"])
		want, ok := workEvents[name]
		if !ok {
			t.Errorf("publishes unknown event %s", name)
			continue
		}
		got, err := os.ReadFile(filepath.Join(root, "modules", "work", strings.TrimPrefix(asStr(pm["schema"]), "./")))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("modules/work/%s differs from schemas/events/%s.json", pm["schema"], name)
		}
	}
	res := asMap(m["resources"])
	if res["ram_mb"] != 28 || res["ram_limit_mb"] != 64 {
		t.Errorf("resources %v: keep in step with the measured footprint (modules/work/README.md, specs/023-work-ui/plan.md)", res)
	}
}

func TestWorkChartAndImage(t *testing.T) {
	v := readYAML(t, "modules/work/chart/values.yaml")
	cfg := asMap(v["config"])
	if cfg["trust_proxy"] != false {
		t.Error("trust_proxy must default to false")
	}
	if _, ok := v["env"]; ok {
		t.Error("secrets are not chart values: can-core injects the can-module Secret")
	}
	sc := asMap(v["securityContext"])
	if sc["runAsNonRoot"] != true || sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("securityContext %v", sc)
	}
	if !chartDigestOK(asStr(asMap(v["image"])["digest"])) {
		t.Error("image digest must be a sha256 or the REPLACE_WITH_ placeholder (`make release-check` rejects it), never a guess")
	}
	df, err := os.ReadFile(filepath.Join(root, "deploy", "work", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"REPLACE_WITH_GOLANG_DIGEST", "REPLACE_WITH_DISTROLESS_STATIC_DIGEST", "USER 65532:65532", "CGO_ENABLED=0"} {
		if !strings.Contains(string(df), want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	var settings map[string]any
	raw, _ := os.ReadFile(filepath.Join(root, "modules", "work", "settings.schema.json"))
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	compileSchema(t, "settings.json", raw, "")
}

func TestNamesKnowWorkCapabilities(t *testing.T) {
	var names struct {
		Capabilities []struct {
			Name string `yaml:"name"`
		} `yaml:"capabilities"`
	}
	if err := yaml.Unmarshal(schemas.Names, &names); err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, c := range names.Capabilities {
		have[c.Name] = true
	}
	for _, n := range []string{"work", "projects"} {
		if !have[n] {
			t.Errorf("schemas/names.yaml lacks %s", n)
		}
	}
}

func TestWorkEventFixtures(t *testing.T) {
	for name, raw := range workEvents {
		t.Run(name, func(t *testing.T) {
			s := compileSchema(t, name+".json", raw, "")
			for kind, wantValid := range map[string]bool{"valid": true, "invalid": false} {
				files, _ := filepath.Glob(filepath.Join(root, "testdata", "events", name, kind+"-*.json"))
				if len(files) < 2 {
					t.Fatalf("need %s fixtures for %s, have %d", kind, name, len(files))
				}
				for _, f := range files {
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
		})
	}
}

// What a running tracker really sends must satisfy the published schemas, and the
// signature must verify.
func TestLiveEventsMatchSchemas(t *testing.T) {
	type call struct {
		body []byte
		hdr  http.Header
	}
	got := make(chan call, 16)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- call{b, r.Header.Clone()}
	}))
	defer recv.Close()

	cfg := work.DefaultConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "w.db")
	cfg.Webhook.URL = recv.URL
	const secret = "whsec-0123456789abcdef"
	const tok = "contract-token-0123456789abcdefXYZ"
	app, err := work.NewApp(cfg, work.Secrets{AdminToken: tok, WebhookSecret: secret}, work.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, ln) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	}()

	do := func(method, path string, body any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, "http://"+ln.Addr().String()+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, b)
		}
	}
	do("POST", "/v1/projects", map[string]any{"key": "ACME", "name": "Acme"})
	do("POST", "/v1/projects/ACME/issues", map[string]any{"type": "epic", "title": "Launch"})
	do("POST", "/v1/projects/ACME/issues", map[string]any{"type": "story", "title": "Signup", "parent": "ACME-1", "assignee": "bob@example.com", "priority": "P1"})
	do("PATCH", "/v1/issues/ACME-2", map[string]any{"priority": "P0", "description": "d", "due_date": "2026-12-01", "estimate": 3, "labels": []string{"x"}, "assignee": nil})
	do("POST", "/v1/issues/ACME-2/transitions", map[string]any{"to": "in_progress"})
	do("POST", "/v1/issues/ACME-2/comments", map[string]any{"body": "hello"})

	seen := map[string]int{}
	for len(seen) < 4 || seen["work.issue.created.v1"] < 2 {
		select {
		case c := <-got:
			var ev struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(c.body, &ev); err != nil {
				t.Fatal(err)
			}
			schema, ok := workEvents[ev.Type]
			if !ok {
				t.Fatalf("unexpected event type %s", ev.Type)
			}
			if err := validateJSON(t, compileSchema(t, ev.Type+".json", schema, ""), c.body); err != nil {
				t.Errorf("%s does not match its schema: %v\n%s", ev.Type, err, c.body)
			}
			if c.hdr.Get("X-Can-Event-Id") != ev.ID {
				t.Error("event id header")
			}
			var sec int64
			for _, ch := range c.hdr.Get("X-Can-Timestamp") {
				sec = sec*10 + int64(ch-'0')
			}
			if c.hdr.Get("X-Can-Signature") != work.Sign([]byte(secret), sec, c.body) {
				t.Errorf("bad signature on %s", ev.Type)
			}
			seen[ev.Type]++
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for events, saw %v", seen)
		}
	}
}

var routeRE = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) (/[^"]*)"`)

func openapiOps(t *testing.T) (ops map[string]bool, doc map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "specs", "011-work-tracker", "contracts", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(asStr(doc["openapi"]), "3.1") {
		t.Fatalf("openapi = %v, want 3.1.x", doc["openapi"])
	}
	ops = map[string]bool{}
	for path, item := range asMap(doc["paths"]) {
		for method := range asMap(item) {
			switch method {
			case "get", "post", "put", "patch", "delete":
				ops[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	return ops, doc
}

// OpenAPI and the router describe the same operations, with {param} names compared as
// placeholders, so a route added without a contract entry (or the reverse) fails here.
func TestOpenAPIMatchesRouter(t *testing.T) {
	ops, _ := openapiOps(t)
	src, err := os.ReadFile(filepath.Join(root, "core", "work", "api.go"))
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]bool{}
	for _, m := range routeRE.FindAllStringSubmatch(string(src), -1) {
		routes[m[1]+" "+m[2]] = true
	}
	if len(routes) < 30 {
		t.Fatalf("found only %d routes in api.go; did the registration style change?", len(routes))
	}
	for r := range routes {
		if !ops[r] {
			t.Errorf("route %q is not in openapi.yaml", r)
		}
	}
	for o := range ops {
		if !routes[o] {
			t.Errorf("openapi.yaml documents %q but the router has no such route", o)
		}
	}
}

func TestOpenAPIRefsResolveAndCapabilityVerbsExist(t *testing.T) {
	ops, doc := openapiOps(t)
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
				var cur any = doc
				for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					cur = asMap(cur)[part]
				}
				if cur == nil {
					t.Errorf("unresolved $ref %s", ref)
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(doc)
	for name, raw := range map[string][]byte{"work.v1": capabilities.WorkV1, "projects.v1": capabilities.ProjectsV1} {
		var top map[string]any
		if err := json.Unmarshal(raw, &top); err != nil {
			t.Fatal(err)
		}
		for _, v := range asList(top["x-verbs"]) {
			vm := asMap(v)
			key := asStr(vm["method"]) + " " + regexp.MustCompile(`\{project\}`).ReplaceAllString(asStr(vm["path"]), "{key}")
			if !ops[key] {
				t.Errorf("%s verb %v (%s) is not in openapi.yaml", name, vm["verb"], key)
			}
		}
	}
}
