package contract

// Epic E13e: the files module (the `files` capability). Offline: manifest and schema contract
// tests, fixtures, the OpenAPI document against the routes the service really registers, and every
// event and view the service really produces validated against the published schemas.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/files"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/schemas/capabilities"
	"github.com/dodwmd/companyinacan/schemas/events"
)

var fileEvents = map[string][]byte{
	"file.uploaded.v1": events.FileUploadedV1,
	"file.shared.v1":   events.FileSharedV1,
	"file.deleted.v1":  events.FileDeletedV1,
	"file.infected.v1": events.FileInfectedV1,
}

func TestFilesManifest(t *testing.T) {
	const rel = "modules/files/module.yaml"
	for _, p := range validateModule(filepath.Join(root, rel)) {
		if p.Severity != problem.Warning || p.Rule != "semantic.module-status" {
			t.Errorf("unexpected problem: %s", p)
		}
	}
	m := readYAML(t, rel)
	if m["status"] != "experimental" {
		t.Errorf("status = %v: nothing here has run on a cluster or against a real S3 or ClamAV and the manifest must say so", m["status"])
	}
	if asMap(m["provider"])["type"] != "inhouse" {
		t.Errorf("provider = %v", m["provider"])
	}
	prov := asList(m["provides"])
	if len(prov) != 1 || asMap(prov[0])["capability"] != "files" || asMap(prov[0])["schema"] != "files.v1" || asMap(prov[0])["port"] != 8095 {
		t.Errorf("provides = %v", prov)
	}
	if _, err := os.Stat(filepath.Join(root, "schemas", "capabilities", "files.v1.json")); err != nil {
		t.Error(err)
	}
	pub := map[string]bool{}
	for _, p := range asList(m["publishes"]) {
		name := asStr(asMap(p)["event"])
		pub[name] = true
		local, err := os.ReadFile(filepath.Join(root, "modules", "files", "events", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		shared, err := os.ReadFile(filepath.Join(root, "schemas", "events", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(local, shared) {
			t.Errorf("modules/files/events/%s.json differs from schemas/events/%s.json", name, name)
		}
	}
	for name := range fileEvents {
		if !pub[name] {
			t.Errorf("event %s is not listed in publishes", name)
		}
	}
	if len(asList(m["subscribes"])) != 0 {
		t.Error("the files module subscribes to no event: nothing here starts a file operation from an event")
	}
	res := asMap(m["resources"])
	if res["ram_mb"] != 20 || res["ram_limit_mb"] != 64 {
		t.Errorf("resources = %v", res)
	}
	perms := map[string]bool{}
	for _, p := range asList(m["permissions"]) {
		perms[asStr(p)] = true
	}
	for _, p := range []string{"files.read", "files.write", "files.admin"} {
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
		if role == "admin" || role == "owner" || strings.Contains(role, ":") {
			t.Errorf("role %q collides with a built-in or namespaced role name", role)
		}
	}
	if containsAny(asList(asMap(m["roles"])["files-reader"]), "files.write") || containsAny(asList(asMap(m["roles"])["files-reader"]), "files.admin") {
		t.Error("the read-only role must not write")
	}
	classes := map[string]bool{}
	for _, dc := range asList(m["data_classes"]) {
		classes[asStr(asMap(dc)["class"])] = true
		if asStr(asMap(dc)["retention"]) == "" {
			t.Errorf("class %v has no retention", asMap(dc)["class"])
		}
	}
	// the classes the service writes into the erasure ledger are the ones the manifest declares
	for _, c := range files.Classes {
		if !classes[c] {
			t.Errorf("class %s is written to the erasure ledger but not declared in data_classes", c)
		}
	}
	if asMap(m["runtime"])["subdomain"] != "files" {
		t.Errorf("runtime = %v: share links need a public host", m["runtime"])
	}
}

func TestFilesEventFixtures(t *testing.T) {
	for name, raw := range fileEvents {
		t.Run(name, func(t *testing.T) {
			s := compileSchema(t, name+".json", raw, "")
			counts := map[string]int{}
			for kind, wantValid := range map[string]bool{"valid": true, "invalid": false} {
				fs, _ := filepath.Glob(filepath.Join(root, "testdata", "events", name, kind+"-*.json"))
				for _, f := range fs {
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

func TestFilesCapabilityFixtures(t *testing.T) {
	const name = "files.v1"
	raw := capabilities.FilesV1
	compileSchema(t, name+".json", raw, "")
	var top map[string]any
	if err := yaml.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	verbs, defs := asList(top["x-verbs"]), asMap(top["$defs"])
	if len(verbs) == 0 || asStr(top["x-capability"]) != "files" {
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
	fs, _ := filepath.Glob(filepath.Join(root, "testdata", "capabilities", name, "*.json"))
	seenValid := map[string]bool{}
	for _, f := range fs {
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
	for d := range defs {
		if !seenValid[d] && d != "Version" && d != "Share" {
			t.Errorf("definition %s has no valid fixture", d)
		}
	}
}

// ---- real service output against the schemas ---------------------------------------------------

type capture struct{ envs [][]byte }

func (c *capture) PublishEnvelope(_ context.Context, e []byte) error {
	c.envs = append(c.envs, append([]byte(nil), e...))
	return nil
}

func TestFilesServiceOutputMatchesSchemas(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	kr, err := files.ParseKeyring("k1 " + strings.Repeat("0123456789abcdef", 4) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	be, err := files.NewLocalBackend(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	f := false
	cfg := files.DefaultConfig()
	cfg.Scanner.Required = &f
	st, err := files.OpenStore(":memory:", files.StoreOptions{Keyring: kr, Backend: be, Scanner: &files.FakeScanner{InfectedMarker: "EICAR-TEST"},
		LinkSecret: []byte(strings.Repeat("L", 40)), Config: cfg, Now: func() time.Time { return now }, EventsOn: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	alice := &files.Principal{Subject: "user:alice", Perms: []string{files.PermWrite}}
	adm := &files.Principal{Subject: "user:root", Perms: []string{files.PermAdmin}}
	check := func(def string, v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if err := validateJSON(t, compileSchema(t, "files.v1.json", capabilities.FilesV1, "#/$defs/"+def), b); err != nil {
			t.Errorf("%s: %v\n%s", def, err, b)
		}
	}
	sp, err := st.CreateSpace(ctx, alice, "Contracts")
	if err != nil {
		t.Fatal(err)
	}
	check("Space", sp)
	pg, _ := st.ListSpaces(ctx, alice, "", 0)
	check("SpaceList", pg)
	if err := st.SetMember(ctx, alice, sp.ID, files.Member{Kind: "group", ID: "grp-eng", Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	ms, _ := st.ListMembers(ctx, alice, sp.ID)
	check("MemberList", map[string]any{"items": ms})
	fo, err := st.CreateFolder(ctx, alice, sp.ID, "", "2026")
	if err != nil {
		t.Fatal(err)
	}
	check("Folder", fo)
	fos, _ := st.ListFolders(ctx, alice, sp.ID)
	check("FolderList", map[string]any{"items": fos})
	pdf := []byte("%PDF-1.7\nsecret-body")
	up, err := st.Upload(ctx, alice, files.UploadRequest{SpaceID: sp.ID, FolderID: fo.ID, Name: "layoffs.pdf", Description: "confidential words", Ref: "board:pack-7",
		Subject: "cand_0123456789abcdef", Body: bytes.NewReader(pdf)})
	if err != nil {
		t.Fatal(err)
	}
	check("File", up)
	if up2, err := st.UploadVersion(ctx, alice, up.ID, bytes.NewReader(append(pdf, '!'))); err != nil {
		t.Fatal(err)
	} else {
		check("File", up2)
	}
	fl, _ := st.ListFiles(ctx, alice, files.ListQuery{SpaceID: sp.ID})
	check("FileList", fl)
	vs, _ := st.ListVersions(ctx, alice, up.ID)
	check("VersionList", map[string]any{"items": vs})
	tok, sh, err := st.CreateShare(ctx, alice, up.ID, files.ShareRequest{TTL: time.Hour, Label: "for Dana"})
	if err != nil {
		t.Fatal(err)
	}
	check("Share", sh)
	check("ShareCreated", map[string]any{"token": tok, "url": "https://files.example.com/s/" + tok, "share": sh})
	shs, _ := st.ListShares(ctx, alice, up.ID)
	check("ShareList", map[string]any{"items": shs})
	if _, err := st.Upload(ctx, alice, files.UploadRequest{SpaceID: sp.ID, Name: "bad.txt", Body: strings.NewReader("EICAR-TEST")}); err == nil {
		t.Fatal("the infected upload was accepted")
	}
	if err := st.DeleteFile(ctx, alice, up.ID); err != nil {
		t.Fatal(err)
	}
	er, err := st.EraseSubject(ctx, adm, "cand_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	check("Erasure", er)
	es, _ := st.ListErasures(ctx, adm, 0)
	check("ErasureList", map[string]any{"items": es})

	// every event the service queued, as it would be published
	cap := &capture{}
	d := &files.Dispatcher{Store: st, Pub: cap, Log: slog.Default()}
	if n, err := d.DeliverDue(ctx); err != nil || n == 0 {
		t.Fatalf("deliver: %d %v", n, err)
	}
	seen := map[string]bool{}
	for _, env := range cap.envs {
		var h struct{ Type string }
		_ = json.Unmarshal(env, &h)
		seen[h.Type] = true
		raw, ok := fileEvents[h.Type]
		if !ok {
			t.Fatalf("unexpected event type %s", h.Type)
		}
		if err := validateJSON(t, compileSchema(t, h.Type+".json", raw, ""), env); err != nil {
			t.Errorf("%s: %v\n%s", h.Type, err, env)
		}
		for _, bad := range []string{"layoffs", "confidential", "Contracts", "for Dana", "secret-body", "cand_0123456789abcdef", "bad.txt"} {
			if strings.Contains(string(env), bad) {
				t.Errorf("%s carries %q", h.Type, bad)
			}
		}
	}
	for ty := range fileEvents {
		if !seen[ty] {
			t.Errorf("the service never produced %s", ty)
		}
	}
}

// ---- OpenAPI ------------------------------------------------------------------------------------

func TestFilesOpenAPIMatchesRoutes(t *testing.T) {
	var doc struct {
		OpenAPI string                          `yaml:"openapi"`
		Paths   map[string]map[string]yaml.Node `yaml:"paths"`
		Info    struct{ Description string }    `yaml:"info"`
		Sec     []map[string][]string           `yaml:"security"`
	}
	b, err := os.ReadFile(filepath.Join(root, "specs/034-files/contracts/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", doc.OpenAPI)
	}
	if !strings.Contains(strings.ToLower(doc.Info.Description), "not legal advice") {
		t.Error("the API description must carry the not-legal-advice disclaimer")
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
	for _, r := range files.Routes() {
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
	// every /v1 route names its permission; the two public routes name none and opt out of security
	for p, ops := range doc.Paths {
		for m, node := range ops {
			if m == "parameters" {
				continue
			}
			var op struct {
				Perm string               `yaml:"x-permission"`
				Sec  []map[string]any     `yaml:"security"`
				Resp map[string]yaml.Node `yaml:"responses"`
			}
			_ = node.Decode(&op)
			if strings.HasPrefix(p, "/v1/") && op.Perm == "" {
				t.Errorf("%s %s has no x-permission", m, p)
			}
			if !strings.HasPrefix(p, "/v1/") && op.Perm != "" {
				t.Errorf("%s %s is public and must not claim a permission", m, p)
			}
			if strings.HasPrefix(p, "/v1/") {
				if _, ok := op.Resp["401"]; !ok {
					t.Errorf("%s %s does not document 401", m, p)
				}
			}
		}
	}
	// every capability verb is a documented route with the same permission
	var cap struct {
		Verbs []struct {
			Verb, Method, Path, Permission string
		} `yaml:"x-verbs"`
	}
	if err := yaml.Unmarshal(capabilities.FilesV1, &cap); err != nil {
		t.Fatal(err)
	}
	for _, v := range cap.Verbs {
		ops, ok := doc.Paths[v.Path]
		if !ok {
			t.Errorf("verb %s: path %s is not in the OpenAPI document", v.Verb, v.Path)
			continue
		}
		node, ok := ops[strings.ToLower(v.Method)]
		if !ok {
			t.Errorf("verb %s: %s %s is not documented", v.Verb, v.Method, v.Path)
			continue
		}
		var op struct {
			Perm string `yaml:"x-permission"`
		}
		_ = node.Decode(&op)
		if op.Perm != v.Permission {
			t.Errorf("verb %s: permission %q in the capability, %q in the OpenAPI document", v.Verb, v.Permission, op.Perm)
		}
	}
}

// ---- docs, chart, image ------------------------------------------------------------------------

func TestFilesDocsAreHonest(t *testing.T) {
	for _, f := range []string{"docs/files.md", "modules/files/README.md", "specs/034-files/spec.md"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ToLower(string(b))
		if !strings.Contains(s, "not legal") || !strings.Contains(s, "unverified") && !strings.Contains(s, "not verified") {
			t.Errorf("%s must say this is a tool, not legal advice, and list what is not verified", f)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, "docs/files.md"))
	for _, want := range []string{"clamd", "ClamAV", "NetworkPolicy", "e-sign", "encrypted at rest", "groups", "scanner.required", "file.infected.v1"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("docs/files.md does not mention %s", want)
		}
	}
}

func TestFilesChartValues(t *testing.T) {
	v := readYAML(t, "modules/files/chart/values.yaml")
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
	if asMap(cfg["scanner"])["required"] != true {
		t.Error("the chart must keep scanner.required on: no upload without a real scanner")
	}
	if asMap(cfg["download"])["inline"] == true {
		t.Error("the chart must not enable inline display")
	}
	ing := asMap(v["ingress"])
	for _, p := range asList(ing["pathPrefixes"]) {
		if asStr(p) != "/s/" {
			t.Errorf("ingress routes %v: only /s/ may be public", p)
		}
	}
	if len(asList(ing["paths"])) != 0 {
		t.Errorf("ingress paths %v", ing["paths"])
	}
	b, _ := os.ReadFile(filepath.Join(root, "deploy/files/Dockerfile"))
	if !strings.Contains(string(b), "USER 65532:65532") || !strings.Contains(string(b), "REPLACE_WITH_GOLANG_DIGEST") {
		t.Error("the Dockerfile must run as 65532 and ship digest placeholders")
	}
	// the values the chart ships are accepted by the service's own strict loader
	cb, _ := yaml.Marshal(cfg)
	if _, err := files.ParseConfig(cb); err != nil {
		t.Errorf("chart config is refused by the service: %v", err)
	}
}
