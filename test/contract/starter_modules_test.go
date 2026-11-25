package contract

// Epic E07: the starter modules (support, newsletter, analytics). Everything here is offline:
// manifests are validated with `can validate`'s code, charts are rendered with `helm template`
// when helm is installed (the structural assertions on values.yaml always run), and the
// capability and event schemas are exercised with fixtures.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	jsch "github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/licence"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/schemas"
	"github.com/dodwmd/companyinacan/schemas/capabilities"
	"github.com/dodwmd/companyinacan/schemas/events"
)

type starter struct {
	id        string
	licence   string
	database  string
	subdomain string
	capab     string // provided capability
	schema    string
	port      int // service port of the app
	event     string
	ramMB     int
	ramLimit  int
	status    string // module status marker
}

var starters = []starter{
	{"support", "AGPL-3.0-only", "mariadb", "support", "tickets", "tickets.v1", 8080, "ticket.created.v1", 250, 512, "not-deployable"},
	{"newsletter", "AGPL-3.0-only", "postgres", "newsletter", "newsletter", "newsletter.v1", 9000, "subscriber.added.v1", 50, 128, "experimental"},
	{"analytics", "EUPL-1.2", "sqlite", "stats", "analytics", "analytics.v1", 8080, "", 20, 96, "experimental"},
}

func readYAML(t *testing.T, rel string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return m
}

func asList(v any) []any { l, _ := v.([]any); return l }
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
func asStr(v any) string { s, _ := v.(string); return s }

func mi(t *testing.T, s string) int {
	t.Helper()
	if !strings.HasSuffix(s, "Mi") {
		t.Fatalf("memory %q must be in Mi", s)
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "Mi"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStarterManifests(t *testing.T) {
	for _, s := range starters {
		t.Run(s.id, func(t *testing.T) {
			rel := "modules/" + s.id + "/module.yaml"
			if ps := onlyUnexpected(validateModule(filepath.Join(root, rel))); len(ps) != 0 {
				t.Fatalf("%s:\n%s", rel, join(ps))
			}
			m := readYAML(t, rel)
			if m["status"] != s.status {
				t.Errorf("status = %v, want %s (not enable-ready until verified on a cluster)", m["status"], s.status)
			}
			// planned must track the chart: a capability is available only when the adapter is enabled.
			adapterOn := asMap(asMap(readYAML(t, "modules/"+s.id+"/chart/values.yaml"))["adapter"])["enabled"] == true
			for _, p := range asList(m["provides"]) {
				if (asMap(p)["status"] != "planned") != adapterOn {
					t.Errorf("provides %v status and chart adapter.enabled=%v drifted apart", asMap(p)["capability"], adapterOn)
				}
			}
			for _, p := range asList(m["subscribes"]) {
				if (asMap(p)["status"] != "planned") != adapterOn {
					t.Errorf("subscribes %v status and chart adapter.enabled=%v drifted apart", asMap(p)["event"], adapterOn)
				}
			}
			prov := asMap(m["provider"])
			if prov["type"] != "oss-wrapper" || prov["licence"] != s.licence || prov["licence_class"] != "osi" {
				t.Errorf("provider = %v, want oss-wrapper %s osi", prov, s.licence)
			}
			if asStr(prov["upstream"]) == "" || asStr(prov["upstream_version"]) == "" {
				t.Error("upstream and upstream_version are required for the footprint and upgrade notes")
			}
			if asMap(m["requires"])["database"] != s.database {
				t.Errorf("database = %v, want %s", asMap(m["requires"])["database"], s.database)
			}
			rt := asMap(m["runtime"])
			if rt["subdomain"] != s.subdomain {
				t.Errorf("subdomain = %v, want %s", rt["subdomain"], s.subdomain)
			}
			ui := asMap(rt["ui"])
			if ui["service"] != s.id || ui["port"] != s.port {
				t.Errorf("runtime.ui = %v, want service %s port %d", ui, s.id, s.port)
			}
			res := asMap(m["resources"])
			if res["ram_mb"] != s.ramMB || res["ram_limit_mb"] != s.ramLimit {
				t.Errorf("resources = %v, want ram %d / limit %d", res, s.ramMB, s.ramLimit)
			}
			if asStr(m["healthcheck"]) == "" {
				t.Error("healthcheck is required")
			}
			prs := asList(m["provides"])
			if len(prs) != 1 || asMap(prs[0])["capability"] != s.capab || asMap(prs[0])["schema"] != s.schema || asMap(prs[0])["service"] != s.id+"-adapter" || asMap(prs[0])["port"] != 8081 {
				t.Errorf("provides = %v", prs)
			}
			if _, err := os.Stat(filepath.Join(root, "schemas", "capabilities", s.schema+".json")); err != nil {
				t.Errorf("capability schema missing: %v", err)
			}
			if len(asList(m["data_classes"])) == 0 {
				t.Error("data_classes with retention are required")
			}
			for _, dc := range asList(m["data_classes"]) {
				if asMap(dc)["sensitivity"] == "sensitive" {
					t.Error("starter modules must not hold sensitive classes")
				}
			}
			hooks := asMap(m["hooks"])
			for _, h := range []string{"backup", "restore"} {
				p := asStr(hooks[h])
				if p == "" {
					t.Errorf("hooks.%s missing", h)
					continue
				}
				script := filepath.Join(root, "modules", s.id, p)
				if out, err := exec.Command("sh", "-n", script).CombinedOutput(); err != nil {
					t.Errorf("%s does not parse: %v\n%s", p, err, out)
				}
				b, _ := os.ReadFile(script)
				if !bytes.HasPrefix(b, []byte("#!/bin/sh\n")) || !bytes.Contains(b, []byte("set -eu")) {
					t.Errorf("%s must be a POSIX sh script with `set -eu`", p)
				}
				for _, want := range []string{"-H @", "--connect-timeout", "--max-time", "umask 077"} {
					if !bytes.Contains(b, []byte(want)) {
						t.Errorf("%s lacks %q (token out of argv, bounded transfers)", p, want)
					}
				}
				if bytes.Contains(b, []byte("Bearer $(")) {
					t.Errorf("%s puts the bearer token on curl's command line", p)
				}
				if !bytes.Contains(b, []byte("/v1/dumps/")) || !bytes.Contains(b, []byte("CAN_TOKEN_FILE")) {
					t.Errorf("%s must move the dump through the can-core dump endpoint with the projected token", p)
				}
			}
			if asStr(hooks["image"]) == "" {
				t.Error("hooks.image is required: upstream images carry neither curl nor the database clients")
			}
			if s.event != "" {
				pub := asList(m["publishes"])
				if len(pub) != 1 || asMap(pub[0])["event"] != s.event {
					t.Fatalf("publishes = %v", pub)
				}
				local, err := os.ReadFile(filepath.Join(root, "modules", s.id, "events", s.event+".json"))
				if err != nil {
					t.Fatal(err)
				}
				shared, err := os.ReadFile(filepath.Join(root, "schemas", "events", s.event+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(local, shared) {
					t.Errorf("modules/%s/events/%s.json differs from schemas/events/%s.json", s.id, s.event, s.event)
				}
			}
			if s.id != "analytics" {
				if !containsAny(asList(asMap(m["requires"])["capabilities"]), "email.send") {
					t.Error("requires.capabilities must contain email.send (relay credentials injection, SPEC 4.3)")
				}
			}
		})
	}
}

// onlyUnexpected drops the warnings the status markers are meant to raise and keeps the rest.
func onlyUnexpected(ps []problem.Problem) []problem.Problem {
	var out []problem.Problem
	for _, p := range ps {
		if p.Severity == problem.Warning && (p.Rule == "semantic.module-status" || p.Rule == "semantic.provides-adapter-disabled") {
			continue
		}
		out = append(out, p)
	}
	return out
}

func containsAny(l []any, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestStatusMarkersRaiseWarnings(t *testing.T) {
	for _, s := range starters {
		got := map[string]int{}
		for _, p := range validateModule(filepath.Join(root, "modules", s.id, "module.yaml")) {
			if p.Severity != problem.Warning {
				t.Errorf("%s: unexpected %s", s.id, p)
			}
			got[p.Rule]++
		}
		if got["semantic.module-status"] != 1 || got["semantic.provides-adapter-disabled"] != 1 {
			t.Errorf("%s: warnings %v, want one module-status and one provides-adapter-disabled", s.id, got)
		}
	}
}

func TestNewsletterWiring(t *testing.T) {
	m := readYAML(t, "modules/newsletter/module.yaml")
	if !containsAny(asList(m["consumes"]), "leads") {
		t.Error("newsletter must consume leads")
	}
	subs := asList(m["subscribes"])
	if len(subs) != 1 {
		t.Fatalf("subscribes = %v", subs)
	}
	sub := asMap(subs[0])
	if sub["event"] != "lead.created.v1" || sub["optional"] != true || sub["webhook"] == "" {
		t.Errorf("subscribes = %v", sub)
	}
	if asStr(readYAML(t, "modules/support/module.yaml")["name"]) == "" {
		t.Error("support manifest unreadable")
	}
}

func TestAnalyticsIsTheLighterChoice(t *testing.T) {
	// SPEC 6.1: Umami ~100 MB, GoatCounter ~20 MB, and Umami needs Postgres while GoatCounter
	// runs on SQLite. The choice is recorded in specs/007-starter-modules/research.md.
	m := readYAML(t, "modules/analytics/module.yaml")
	if asMap(m["provider"])["name"] != "GoatCounter" {
		t.Errorf("provider = %v", asMap(m["provider"]))
	}
	if asMap(m["resources"])["ram_mb"].(int) >= 100 {
		t.Error("the analytics module must be lighter than Umami (~100 MB)")
	}
}

func TestAcceptedLicencesHaveStarterLicences(t *testing.T) {
	acc := licence.DefaultAccepted()
	for _, l := range []string{"AGPL-3.0-only", "EUPL-1.2"} {
		if !acc.Has(l) {
			t.Errorf("%s is not accepted", l)
		}
	}
	entries, ps := licence.Collect(filepath.Join(root, "modules"), acc)
	if len(ps) != 0 {
		t.Fatalf("%s", join(ps))
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Module.ID] = true
		for _, p := range e.Result.Problems {
			t.Errorf("%s: %s", e.Module.ID, p)
		}
	}
	for _, s := range starters {
		if !got[s.id] {
			t.Errorf("%s is missing from the licence collection", s.id)
		}
	}
}

func TestNetworkCopyleftDocumented(t *testing.T) {
	for _, f := range []string{"modules/README.md", "modules/support/README.md", "modules/newsletter/README.md", "modules/analytics/README.md"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ToLower(string(b))
		if !strings.Contains(s, "network") || !strings.Contains(s, "footprint") && f == "modules/support/README.md" {
			t.Errorf("%s must mention footprint and the network-copyleft implication", f)
		}
		if !strings.Contains(s, "copyleft") {
			t.Errorf("%s must mention the network-copyleft implication", f)
		}
	}
}

func TestEmailPolicyDoc(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "docs", "email-policy.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"can-smtp", "IMAP", "inbound-parse", "SPF", "DKIM", "DMARC", "email.send", "587", "bulk"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("docs/email-policy.md does not mention %q", w)
		}
	}
}

// ---- charts ------------------------------------------------------------------------------

var imageRef = regexp.MustCompile(`^[a-z0-9./-]+$`)

func TestChartValues(t *testing.T) {
	for _, s := range starters {
		t.Run(s.id, func(t *testing.T) {
			base := "modules/" + s.id + "/chart/"
			chart := readYAML(t, base+"Chart.yaml")
			if chart["name"] != s.id || chart["apiVersion"] != "v2" || chart["type"] != "application" {
				t.Errorf("Chart.yaml = %v", chart)
			}
			v := readYAML(t, base+"values.yaml")
			img := asMap(v["image"])
			if !imageRef.MatchString(asStr(img["repository"])) || asStr(img["tag"]) == "" || strings.Contains(asStr(img["tag"]), "latest") {
				t.Errorf("image = %v: need a repository and a fixed tag", img)
			}
			if d := asStr(img["digest"]); d != "sha256:REPLACE_WITH_DIGEST" && !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(d) {
				t.Errorf("image.digest %q must be a sha256 digest or the documented placeholder", d)
			}
			sc := asMap(v["securityContext"])
			if sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false || asMap(sc["capabilities"]) == nil || asMap(v["podSecurityContext"])["runAsNonRoot"] != true {
				t.Errorf("security contexts are not restricted-profile compliant: %v", sc)
			}
			if asMap(asMap(v["podSecurityContext"])["seccompProfile"])["type"] != "RuntimeDefault" {
				t.Error("seccompProfile RuntimeDefault required")
			}
			res := asMap(v["resources"])
			req, lim := mi(t, asStr(asMap(res["requests"])["memory"])), mi(t, asStr(asMap(res["limits"])["memory"]))
			ad := asMap(v["adapter"])
			adr := asMap(ad["resources"])
			areq, alim := mi(t, asStr(asMap(adr["requests"])["memory"])), mi(t, asStr(asMap(adr["limits"])["memory"]))
			if req+areq > s.ramMB || lim+alim > s.ramLimit {
				t.Errorf("app+adapter requests %d / limits %d exceed the manifest's %d / %d", req+areq, lim+alim, s.ramMB, s.ramLimit)
			}
			if ad["enabled"] != false || ad["port"] != 8081 {
				t.Errorf("adapter must be off by default on port 8081: %v", ad)
			}
			if !strings.Contains(asStr(asMap(ad["image"])["digest"]), "REPLACE_WITH_") {
				t.Error("adapter image digest must be the documented placeholder until the adapter is built")
			}
			secrets := asMap(v["secrets"])
			for k, want := range map[string]string{"database": "can-database", "smtp": "can-smtp", "module": "can-module"} {
				if secrets[k] != want {
					t.Errorf("secrets.%s = %v, want %s", k, secrets[k], want)
				}
			}
			if asMap(v["probes"])["readiness"] == nil || asMap(v["probes"])["liveness"] == nil {
				t.Error("readiness and liveness probes required")
			}
			if asMap(v["ingress"])["host"] != "" {
				t.Error("ingress.host is supplied by can-core and must default to empty")
			}
		})
	}
}

func TestImagePinCheckCoversModuleCharts(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "modules", "x", "chart")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts", "check-image-pins.sh")
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("image:\n  digest: sha256:REPLACE_WITH_DIGEST\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", script, tmp).CombinedOutput(); err == nil || !strings.Contains(string(out), "REPLACE_WITH_") {
		t.Fatalf("placeholder in a module chart not detected: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("image:\n  digest: sha256:abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", script, tmp).CombinedOutput(); err != nil {
		t.Fatalf("pinned chart rejected: %v %s", err, out)
	}
}

func helmRender(t *testing.T, id string, sets ...string) ([]obj, error) {
	t.Helper()
	args := []string{"template", id, filepath.Join(root, "modules", id, "chart"), "--namespace", "mod-" + id}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, stderr.String())
	}
	dec := yaml.NewDecoder(bytes.NewReader(out))
	var docs []obj
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			break
		}
		if m != nil {
			docs = append(docs, obj(m))
		}
	}
	return docs, nil
}

func TestChartsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; values.yaml structure is still checked by TestChartValues")
	}
	for _, s := range starters {
		t.Run(s.id, func(t *testing.T) {
			if _, err := helmRender(t, s.id); err == nil {
				t.Error("render without ingress.host must fail (can-core supplies <subdomain>.<domain>)")
			}
			host := s.subdomain + ".example.com"
			base := []string{"ingress.host=" + host}
			for _, withAdapter := range []bool{false, true} {
				sets := append([]string{}, base...)
				if withAdapter {
					sets = append(sets, "adapter.enabled=true", "inbound.mode=webhook")
				}
				docs, err := helmRender(t, s.id, sets...)
				if err != nil {
					t.Fatal(err)
				}
				checkRendered(t, s, docs, host, withAdapter)
			}
			if s.id == "support" {
				docs, err := helmRender(t, s.id, "ingress.host="+host, "adapter.enabled=true", "inbound.mode=imap")
				if err != nil {
					t.Fatal(err)
				}
				if f := mustJSON(ofKind(docs, "IngressRoute")[0].get("spec")); strings.Contains(f, "/inbound") {
					t.Errorf("imap mode must not expose a public inbound route: %s", f)
				}
			}
			// local dev: plain HTTP, no certificate resolver
			docs, err := helmRender(t, s.id, "ingress.host="+host, "ingress.tls=false")
			if err != nil {
				t.Fatal(err)
			}
			ir := ofKind(docs, "IngressRoute")[0]
			if mustJSON(ir.get("spec", "tls")) != "null\n" || !strings.Contains(mustJSON(ir.get("spec", "entryPoints")), "web") {
				t.Errorf("ingress.tls=false must use the web entry point without tls: %s", mustJSON(ir.get("spec")))
			}
		})
	}
}

func checkRendered(t *testing.T, s starter, docs []obj, host string, adapter bool) {
	t.Helper()
	ns := "mod-" + s.id
	for _, d := range docs {
		if d.ns() != ns {
			t.Errorf("%s/%s has namespace %q, want %s", d.kind(), d.name(), d.ns(), ns)
		}
		switch d.kind() {
		case "Deployment", "Service", "IngressRoute", "NetworkPolicy", "PersistentVolumeClaim":
		default:
			t.Errorf("unexpected kind %s: charts may only create namespaced workload, network and storage objects", d.kind())
		}
	}
	deps := ofKind(docs, "Deployment")
	want := 1
	if adapter {
		want = 2
	}
	if len(deps) != want {
		t.Fatalf("%d Deployments, want %d", len(deps), want)
	}
	var reqSum, limSum int
	for _, d := range deps {
		if d.get("spec", "template", "spec", "automountServiceAccountToken") != false {
			t.Errorf("%s: automountServiceAccountToken must be false (adapters use a projected audience token)", d.name())
		}
		if d.get("spec", "template", "spec", "hostNetwork") != nil || d.get("spec", "template", "spec", "hostPID") != nil {
			t.Errorf("%s: host namespaces are not allowed", d.name())
		}
		if asStr(d.get("spec", "strategy", "type")) != "Recreate" && d.name() == s.id {
			t.Errorf("%s: ReadWriteOnce data needs strategy Recreate", d.name())
		}
		pod := asMap(d.get("spec", "template", "spec"))
		psc := asMap(pod["securityContext"])
		if psc["runAsNonRoot"] != true || asMap(psc["seccompProfile"])["type"] != "RuntimeDefault" {
			t.Errorf("%s: pod securityContext not restricted: %v", d.name(), psc)
		}
		for _, v := range asList(pod["volumes"]) {
			if asMap(v)["hostPath"] != nil {
				t.Errorf("%s: hostPath volumes are not allowed", d.name())
			}
		}
		var all []any
		all = append(all, asList(pod["initContainers"])...)
		all = append(all, asList(pod["containers"])...)
		for _, c := range all {
			cm := asMap(c)
			csc := asMap(cm["securityContext"])
			if csc["allowPrivilegeEscalation"] != false || csc["privileged"] == true || strings.Join(strs(asMap(csc["capabilities"])["drop"]), ",") != "ALL" {
				t.Errorf("%s/%s: container securityContext not restricted: %v", d.name(), cm["name"], csc)
			}
			r := asMap(cm["resources"])
			if asMap(r["requests"])["memory"] == nil || asMap(r["limits"])["memory"] == nil || asMap(r["requests"])["cpu"] == nil {
				t.Errorf("%s/%s: cpu and memory requests and a memory limit are required", d.name(), cm["name"])
			}
			if strings.Contains(asStr(cm["image"]), "@sha256:REPLACE_WITH_") || strings.HasSuffix(asStr(cm["image"]), ":latest") {
				t.Errorf("%s/%s: image %q", d.name(), cm["name"], cm["image"])
			}
		}
		for _, c := range asList(pod["containers"]) {
			cm := asMap(c)
			r := asMap(cm["resources"])
			reqSum += mi(t, asStr(asMap(r["requests"])["memory"]))
			limSum += mi(t, asStr(asMap(r["limits"])["memory"]))
			if cm["readinessProbe"] == nil || cm["livenessProbe"] == nil {
				t.Errorf("%s/%s: probes missing", d.name(), cm["name"])
			}
		}
	}
	if reqSum > s.ramMB || limSum > s.ramLimit {
		t.Errorf("container memory requests %d / limits %d exceed the manifest's %d / %d", reqSum, limSum, s.ramMB, s.ramLimit)
	}

	svcPorts := map[string]int{}
	for _, sv := range ofKind(docs, "Service") {
		if sv.get("spec", "type") != nil && sv.str("spec", "type") != "ClusterIP" {
			t.Errorf("service %s must be ClusterIP", sv.name())
		}
		for _, p := range asList(sv.get("spec", "ports")) {
			svcPorts[sv.name()] = asMap(p)["port"].(int)
		}
	}
	if svcPorts[s.id] != s.port {
		t.Errorf("service %s port %d, manifest says %d (%v)", s.id, svcPorts[s.id], s.port, svcPorts)
	}
	if _, ok := svcPorts[s.id+"-adapter"]; ok != adapter {
		t.Errorf("adapter service presence = %v, want %v", ok, adapter)
	} else if adapter && svcPorts[s.id+"-adapter"] != 8081 {
		t.Errorf("adapter service port %d, manifest provides 8081", svcPorts[s.id+"-adapter"])
	}

	irs := ofKind(docs, "IngressRoute")
	if len(irs) != 1 {
		t.Fatalf("%d IngressRoutes", len(irs))
	}
	flat := mustJSON(irs[0].get("spec"))
	if !strings.Contains(flat, "Host(`"+host+"`)") || !strings.Contains(flat, "certResolver: letsencrypt") || !strings.Contains(flat, "websecure") {
		t.Errorf("IngressRoute does not route %s over TLS: %s", host, flat)
	}
	if adapter && s.id == "support" && !strings.Contains(flat, "PathPrefix(`/inbound`)") {
		t.Errorf("support must route the inbound-parse webhook to the adapter: %s", flat)
	}
	if !adapter && strings.Contains(flat, "PathPrefix") {
		t.Errorf("no extra public route without the adapter: %s", flat)
	}

	pvcs := ofKind(docs, "PersistentVolumeClaim")
	if len(pvcs) != 1 || pvcs[0].name() != s.id+"-data" {
		t.Errorf("want one PVC %s-data, got %d", s.id, len(pvcs))
	}

	pols := ofKind(docs, "NetworkPolicy")
	if len(pols) != 0 && !adapter {
		t.Errorf("no extra NetworkPolicy needed with a single pod (base template covers Traefik, can-core, DNS, DB, SMTP): %d", len(pols))
	}
	if adapter {
		if len(pols) != 1 || pols[0].name() != "allow-intra-namespace" {
			t.Fatalf("want one allow-intra-namespace policy, got %d", len(pols))
		}
		flat := mustJSON(pols[0].get("spec"))
		if !strings.Contains(flat, "Ingress") || !strings.Contains(flat, "Egress") || strings.Contains(flat, "ipBlock") || strings.Contains(flat, "namespaceSelector") {
			t.Errorf("intra-namespace policy must allow only same-namespace pods, both directions (module-egress excludes the pod CIDR): %s", flat)
		}
	}
}

func strs(v any) []string {
	var out []string
	for _, x := range asList(v) {
		out = append(out, asStr(x))
	}
	return out
}

// ---- capability and event schemas ----------------------------------------------------------

func compileSchema(t *testing.T, name string, raw []byte, fragment string) *jsch.Schema {
	t.Helper()
	doc, err := jsch.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsch.NewCompiler()
	c.DefaultDraft(jsch.Draft2020)
	c.AssertFormat()
	if err := c.AddResource(name, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(name + fragment)
	if err != nil {
		t.Fatalf("%s%s does not compile: %v", name, fragment, err)
	}
	return s
}

func TestStarterEventFixtures(t *testing.T) {
	for name, raw := range map[string][]byte{"ticket.created.v1": events.TicketCreatedV1, "subscriber.added.v1": events.SubscriberAddedV1} {
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
			if counts["valid"] < 2 || counts["invalid"] < 5 {
				t.Errorf("need at least 2 valid and 5 invalid fixtures, have %v", counts)
			}
		})
	}
}

// Fixture names are <valid|invalid>-<Definition>-<case>.json and are checked against
// #/$defs/<Definition> of the capability schema.
func TestCapabilityFixtures(t *testing.T) {
	for name, raw := range map[string][]byte{"tickets.v1": capabilities.TicketsV1, "newsletter.v1": capabilities.NewsletterV1, "analytics.v1": capabilities.AnalyticsV1, "work.v1": capabilities.WorkV1, "projects.v1": capabilities.ProjectsV1} {
		t.Run(name, func(t *testing.T) {
			compileSchema(t, name+".json", raw, "")
			var top map[string]any
			if err := yaml.Unmarshal(raw, &top); err != nil {
				t.Fatal(err)
			}
			verbs := asList(top["x-verbs"])
			if len(verbs) == 0 || asStr(top["x-capability"]) == "" {
				t.Fatal("x-capability and x-verbs are required")
			}
			defs := asMap(top["$defs"])
			wrote := false
			for _, v := range verbs {
				vm := asMap(v)
				for _, k := range []string{"request", "response"} {
					if d := asStr(vm[k]); d != "" && defs[d] == nil {
						t.Errorf("verb %v names unknown definition %q", vm["verb"], d)
					}
				}
				if m := asStr(vm["method"]); m != "GET" && vm["write"] != true {
					t.Errorf("verb %v: non-GET verbs must be flagged write: true (SPEC 4.3 lists write verbs explicitly)", vm["verb"])
				}
				wrote = wrote || vm["write"] == true
			}
			if !wrote {
				t.Error("no write verbs declared")
			}
			files, _ := filepath.Glob(filepath.Join(root, "testdata", "capabilities", name, "*.json"))
			if len(files) < 6 {
				t.Fatalf("need fixtures, have %d", len(files))
			}
			seenValid := map[string]bool{}
			for _, f := range files {
				parts := strings.SplitN(strings.TrimSuffix(filepath.Base(f), ".json"), "-", 3)
				if len(parts) != 3 {
					t.Fatalf("fixture %s must be <valid|invalid>-<Definition>-<case>.json", f)
				}
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

func TestNamesKnowStarterCapabilities(t *testing.T) {
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
	for _, n := range []string{"tickets", "newsletter", "analytics"} {
		if !have[n] {
			t.Errorf("schemas/names.yaml lacks %s", n)
		}
	}
}
