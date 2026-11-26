package contract

// Epic E16a: the jobs module (recruiting). Offline: manifest and schema contract tests, event
// and capability fixtures, the OpenAPI document against the routes the service really registers,
// events the service really emits validated against the published schemas, and erasure
// tombstones accepted by the real backup ledger.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
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

	"github.com/dodwmd/companyinacan/core/backup"
	"github.com/dodwmd/companyinacan/core/jobs"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/schemas"
	"github.com/dodwmd/companyinacan/schemas/capabilities"
	"github.com/dodwmd/companyinacan/schemas/events"
)

var jobsEvents = map[string][]byte{
	"job.opened.v1":                events.JobOpenedV1,
	"application.received.v1":      events.ApplicationReceivedV1,
	"application.stage_changed.v1": events.ApplicationStageChangedV1,
	"candidate.hired.v1":           events.CandidateHiredV1,
}

func TestJobsManifest(t *testing.T) {
	for _, p := range validateModule(filepath.Join(root, "modules", "jobs", "module.yaml")) {
		if p.Severity == problem.Error {
			t.Errorf("manifest error: %s", p)
		}
	}
	m := readYAML(t, "modules/jobs/module.yaml")
	if m["status"] != "experimental" {
		t.Errorf("status = %v: not verified on a cluster, so it must say experimental", m["status"])
	}
	if asMap(m["provider"])["type"] != "inhouse" {
		t.Error("jobs is in-house: candidate data is personal data")
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
	if strings.Join(caps, " ") != "ats=ats.v1 jobs=jobs.v1 summary=summary.v1" {
		t.Errorf("provides = %v", caps)
	}
	perms := map[string]bool{}
	for _, p := range asList(m["permissions"]) {
		perms[asStr(p)] = true
	}
	for role, ps := range asMap(m["roles"]) {
		for _, p := range asList(ps) {
			if !perms[asStr(p)] {
				t.Errorf("role %s grants undeclared permission %v", role, p)
			}
		}
	}
	classes := map[string]bool{}
	for _, c := range asList(m["data_classes"]) {
		classes[asStr(asMap(c)["class"])] = true
		if asStr(asMap(c)["retention"]) == "" || asMap(c)["sensitivity"] != "sensitive" {
			t.Errorf("data class %v needs a retention and sensitive handling", c)
		}
	}
	for _, c := range []string{jobs.ClassPII, jobs.ClassResumes, jobs.ClassEvaluations, jobs.ClassEEO} {
		if !classes[c] {
			t.Errorf("data class %s not declared (the erasure ledger uses these names)", c)
		}
	}
	pubs := asList(m["publishes"])
	if len(pubs) != len(jobsEvents) {
		t.Fatalf("publishes %d events, want %d", len(pubs), len(jobsEvents))
	}
	for _, p := range pubs {
		pm := asMap(p)
		name := asStr(pm["event"])
		want, ok := jobsEvents[name]
		if !ok {
			t.Errorf("publishes unknown event %s", name)
			continue
		}
		got, err := os.ReadFile(filepath.Join(root, "modules", "jobs", strings.TrimPrefix(asStr(pm["schema"]), "./")))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("modules/jobs/%s differs from schemas/events/%s.json", pm["schema"], name)
		}
	}
	res := asMap(m["resources"])
	if res["ram_mb"] != 30 || res["ram_limit_mb"] != 64 {
		t.Errorf("resources %v: keep in step with the measured footprint (specs/013-jobs-recruiting/research.md R8)", res)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "modules", "jobs", "settings.schema.json"))
	compileSchema(t, "settings.json", raw, "")
}

func TestJobsChartAndImage(t *testing.T) {
	v := readYAML(t, "modules/jobs/chart/values.yaml")
	cfg := asMap(v["config"])
	if cfg["trust_proxy"] != false {
		t.Error("trust_proxy must default to false")
	}
	if asMap(cfg["apply"])["scanner_required"] != true {
		t.Error("resume uploads must stay off until a real scanner is wired (scanner_required: true)")
	}
	if asMap(cfg["apply"])["collect_eeo"] != false {
		t.Error("collecting diversity data must be off by default")
	}
	sc := asMap(v["securityContext"])
	if sc["runAsNonRoot"] != true || sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("securityContext %v", sc)
	}
	if !chartDigestOK(asStr(asMap(v["image"])["digest"])) {
		t.Error("image digest must be a sha256 or the REPLACE_WITH_ placeholder (`make release-check` rejects it), never a guess")
	}
	if kr := asMap(v["keyring"]); kr["enabled"] != true || kr["mountPath"] != "/etc/can-jobs/keyring" {
		t.Errorf("keyring %v", kr)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "modules/jobs/chart/templates/deployment.yaml")); !strings.Contains(string(b), "subPath: keyring") {
		t.Error("the keyring must be mounted with subPath (the loader refuses symlinks)")
	}
	if got := asList(asMap(v["ingress"])["pathPrefixes"]); len(got) != 2 || got[0] != "/careers" || got[1] != "/feeds" {
		t.Errorf("only the public careers paths may be routed, got %v", got)
	}
	df, err := os.ReadFile(filepath.Join(root, "deploy", "jobs", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"REPLACE_WITH_GOLANG_DIGEST", "REPLACE_WITH_DISTROLESS_STATIC_DIGEST", "USER 65532:65532", "CGO_ENABLED=0", "cmd/can-jobs", "/can-jobs"} {
		if !strings.Contains(string(df), want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
}

func TestNamesKnowJobsCapabilities(t *testing.T) {
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
	for _, n := range []string{"jobs", "ats"} {
		if !have[n] {
			t.Errorf("schemas/names.yaml lacks %s", n)
		}
	}
}

func TestJobsEventFixtures(t *testing.T) {
	for name, raw := range jobsEvents {
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

func TestJobsCapabilityFixtures(t *testing.T) {
	for name, raw := range map[string][]byte{"jobs.v1": capabilities.JobsV1, "ats.v1": capabilities.AtsV1} {
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

// newJobsServer starts a real service with a webhook receiver and returns helpers.
type jobsServer struct {
	t    *testing.T
	base string
	app  *jobs.App
	got  chan []byte
	hdr  chan http.Header
}

const (
	jobsAdmin  = "contract-admin-token-0123456789abcdefXYZ"
	jobsWHSec  = "whsec-0123456789abcdef"
	jobsKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
)

func startJobs(t *testing.T) *jobsServer {
	t.Helper()
	js := &jobsServer{t: t, got: make(chan []byte, 64), hdr: make(chan http.Header, 64)}
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		js.got <- b
		js.hdr <- r.Header.Clone()
	}))
	t.Cleanup(recv.Close)
	cfg := jobs.DefaultConfig()
	dir := t.TempDir()
	cfg.DBPath = filepath.Join(dir, "j.db")
	cfg.ResumeDir = filepath.Join(dir, "r")
	cfg.Company.Name, cfg.Company.BaseURL = "Example Co", "https://example.test"
	cfg.Webhook.URL = recv.URL
	cfg.Apply.MinFillSeconds = 0
	kr, err := jobs.ParseKeyring("k1 " + jobsKeyHex + "\n")
	if err != nil {
		t.Fatal(err)
	}
	app, err := jobs.NewApp(cfg, jobs.Secrets{AdminToken: jobsAdmin, WebhookSecret: jobsWHSec, HashKey: "hash-key-0123456789abcdef0123456789abcdef"},
		jobs.Options{Logger: slog.New(slog.DiscardHandler), Keyring: kr})
	if err != nil {
		t.Fatal(err)
	}
	js.app = app
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	})
	js.base = "http://" + ln.Addr().String()
	return js
}

func (j *jobsServer) call(method, path string, body any) map[string]any {
	j.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, j.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+jobsAdmin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		j.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		j.t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, b)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func (j *jobsServer) apply(slug, email string) {
	j.t.Helper()
	page, err := http.Get(j.base + "/careers/" + slug)
	if err != nil {
		j.t.Fatal(err)
	}
	pb, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	mm := regexp.MustCompile(`name="_t" value="([^"]+)"`).FindSubmatch(pb)
	if mm == nil {
		j.t.Fatalf("no form token: %s", pb)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"_t": string(mm[1]), "name": "Ada Example", "email": email, "consent": "on"} {
		_ = w.WriteField(k, v)
	}
	_ = w.Close()
	req, _ := http.NewRequest("POST", j.base+"/careers/"+slug+"/apply", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		j.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 303 {
		j.t.Fatalf("apply = %d", resp.StatusCode)
	}
}

// What a running service really sends must satisfy the published schemas and verify.
func TestJobsLiveEventsMatchSchemas(t *testing.T) {
	js := startJobs(t)
	job := js.call("POST", "/v1/jobs", map[string]any{"title": "Engineer", "remote": "remote", "applicant_countries": []string{"US"}, "employment_type": "full_time",
		"sections": []map[string]string{{"heading": "A", "body": "B"}}})
	id, slug := job["id"].(string), job["slug"].(string)
	js.call("POST", "/v1/jobs/"+id+"/transitions", map[string]any{"to": "open"})
	js.apply(slug, "ada@example.com")
	apps := js.call("GET", "/v1/applications?job_id="+id, nil)["items"].([]any)
	aid := apps[0].(map[string]any)["id"].(string)
	js.call("POST", "/v1/applications/"+aid+"/move", map[string]any{"stage": "offer"})
	js.call("POST", "/v1/applications/"+aid+"/hire", nil)

	seen := map[string]int{}
	for len(seen) < 4 {
		select {
		case body := <-js.got:
			hdr := <-js.hdr
			var ev struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(body, &ev); err != nil {
				t.Fatal(err)
			}
			schema, ok := jobsEvents[ev.Type]
			if !ok {
				t.Fatalf("unexpected event type %s", ev.Type)
			}
			if err := validateJSON(t, compileSchema(t, ev.Type+".json", schema, ""), body); err != nil {
				t.Errorf("%s does not match its schema: %v\n%s", ev.Type, err, body)
			}
			if strings.Contains(string(body), "Ada") || strings.Contains(string(body), "ada@example.com") {
				t.Errorf("%s carries candidate data: %s", ev.Type, body)
			}
			if hdr.Get("X-Can-Event-Id") != ev.ID {
				t.Error("event id header")
			}
			var sec int64
			for _, ch := range hdr.Get("X-Can-Timestamp") {
				sec = sec*10 + int64(ch-'0')
			}
			if hdr.Get("X-Can-Signature") != jobs.Sign([]byte(jobsWHSec), sec, body) {
				t.Errorf("bad signature on %s", ev.Type)
			}
			seen[ev.Type]++
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for events, saw %v", seen)
		}
	}
}

// The erasure tombstones must be accepted by the real E08 ledger.
func TestJobsErasureTombstonesFitTheBackupLedger(t *testing.T) {
	js := startJobs(t)
	job := js.call("POST", "/v1/jobs", map[string]any{"title": "Engineer", "remote": "remote", "applicant_countries": []string{"US"}, "employment_type": "full_time",
		"sections": []map[string]string{{"heading": "A", "body": "B"}}})
	js.call("POST", "/v1/jobs/"+job["id"].(string)+"/transitions", map[string]any{"to": "open"})
	js.apply(job["slug"].(string), "erase.me@example.com")
	cs := js.call("GET", "/v1/candidates", nil)["items"].([]any)
	cid := cs[0].(map[string]any)["id"].(string)
	js.call("POST", "/v1/candidates/"+cid+"/erasure", nil)
	ents := js.call("GET", "/v1/erasures", nil)["items"].([]any)
	if len(ents) != 3 {
		t.Fatalf("tombstones = %v", ents)
	}
	l := &backup.Ledger{Path: filepath.Join(t.TempDir(), "ledger.jsonl")}
	for _, e := range ents {
		m := e.(map[string]any)
		if _, added, err := l.Append(m["subject"].(string), m["class"].(string)); err != nil || !added {
			t.Fatalf("ledger rejected %v: added=%v err=%v", m, added, err)
		}
	}
	es, err := l.Read()
	if err != nil || len(es) != 3 || backup.VerifyChain(es) != nil {
		t.Fatalf("ledger = %v, %v", es, err)
	}
}

var jobsRouteRE = regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) (/[^"]*)"`)

func jobsOpenAPI(t *testing.T) (ops map[string]bool, doc map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "specs", "013-jobs-recruiting", "contracts", "openapi.yaml"))
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
		for method, op := range asMap(item) {
			switch method {
			case "get", "post", "put", "patch", "delete":
				ops[strings.ToUpper(method)+" "+path] = true
				if strings.HasPrefix(path, "/v1/") && asMap(op)["x-roles"] == nil {
					t.Errorf("%s %s has no x-roles", method, path)
				}
			}
		}
	}
	return ops, doc
}

// OpenAPI and the router describe the same operations, so a route added without a contract
// entry (or the reverse) fails here.
func TestJobsOpenAPIMatchesRouter(t *testing.T) {
	ops, _ := jobsOpenAPI(t)
	routes := map[string]bool{}
	for _, f := range []string{"api.go", "public.go", "app.go"} {
		src, err := os.ReadFile(filepath.Join(root, "core", "jobs", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range jobsRouteRE.FindAllStringSubmatch(string(src), -1) {
			routes[m[1]+" "+m[2]] = true
		}
	}
	if len(routes) < 45 {
		t.Fatalf("found only %d routes; did the registration style change?", len(routes))
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

func TestJobsOpenAPIRefsResolveAndCapabilityVerbsExist(t *testing.T) {
	ops, doc := jobsOpenAPI(t)
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
	for name, raw := range map[string][]byte{"jobs.v1": capabilities.JobsV1, "ats.v1": capabilities.AtsV1} {
		var top map[string]any
		if err := json.Unmarshal(raw, &top); err != nil {
			t.Fatal(err)
		}
		for _, v := range asList(top["x-verbs"]) {
			vm := asMap(v)
			key := asStr(vm["method"]) + " " + asStr(vm["path"])
			if !ops[key] {
				t.Errorf("%s verb %v (%s) is not in openapi.yaml", name, vm["verb"], key)
			}
		}
	}
}

func TestJobsDocsCarryTheDisclaimersAndHonestLimits(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "docs", "recruiting.md"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ToLower(string(b))
	for _, want := range []string{"not legal", "linkedin", "indeed", "glassdoor", "partner", "retention", "erasure"} {
		if !strings.Contains(s, want) {
			t.Errorf("docs/recruiting.md lacks %q", want)
		}
	}
}
