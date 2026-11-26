package installer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/files"
	"github.com/dodwmd/companyinacan/core/finance"
	"github.com/dodwmd/companyinacan/core/forms"
	"github.com/dodwmd/companyinacan/core/jobs"
	"github.com/dodwmd/companyinacan/core/people"
	"github.com/dodwmd/companyinacan/core/provision"
	"github.com/dodwmd/companyinacan/core/work"
)

// The in-house modules' charts (epic E03c) go through the same installer as the starter modules:
// allowlisted kinds, ClusterIP Services, own-namespace NetworkPolicy and IngressRoute only. These
// tests render them with the real renderer against the fake API; nothing here runs a pod.
type inhouse struct {
	id, svcs  string // service names, comma separated
	ramMB     int
	limitMB   int
	port      string // port of the liveness healthcheck
	parseConf func(b []byte) error
}

// binary is the binary and image name: can-<id>, except where the module id differs.
func (m inhouse) binary() string {
	if m.id == "provisioning" {
		return "can-provision"
	}
	return "can-" + m.id
}

// subdomains of the modules that have public routes.
var inhouseSubdomains = map[string]string{"jobs": "careers", "provisioning": "scim", "files": "files"}

func inhouseModules() []inhouse {
	return []inhouse{
		{"forms", "forms,forms-api", 15, 32, "8080", func(b []byte) error { _, err := forms.ParseConfig(b); return err }},
		{"work", "work", 28, 64, "8080", func(b []byte) error {
			return writeLoad(b, func(p string) error { _, err := work.LoadConfig(p, true); return err })
		}},
		{"people", "people-api", 30, 64, "8081", func(b []byte) error { _, err := people.ParseConfig(b); return err }},
		{"jobs", "jobs", 30, 64, "8080", func(b []byte) error {
			return writeLoad(b, func(p string) error { _, err := jobs.LoadConfig(p, true); return err })
		}},
		{"finance", "finance", 28, 64, "8080", func(b []byte) error {
			return writeLoad(b, func(p string) error { _, err := finance.LoadConfig(p, true); return err })
		}},
		{"provisioning", "provisioning", 20, 64, "8090", func(b []byte) error { _, err := provision.ParseConfig(b); return err }},
		{"files", "files", 20, 64, "8095", func(b []byte) error { _, err := files.ParseConfig(b); return err }},
	}
}

func writeLoad(b []byte, f func(path string) error) error {
	p := filepath.Join(os.TempDir(), fmt.Sprintf("modcfg-%d.yaml", os.Getpid()))
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(p) }()
	return f(p)
}

func enableInhouse(t *testing.T, id string, mutate func(*Installer)) (*Installer, map[string]map[string]any, string) {
	t.Helper()
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir = "../../../modules"
	in.cfg.Domain = "acme.example"
	if mutate != nil {
		mutate(in)
	}
	spec.ID, spec.Namespace, spec.ServiceAccount = id, "mod-"+id, id
	spec.Manifest = loadManifest(t, "../../../modules/"+id+"/module.yaml")
	spec.Manifest.Requires.Database = "none"
	if sub, ok := inhouseSubdomains[id]; ok {
		spec.Manifest.Runtime.Subdomain = sub
	}
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatalf("%s chart does not install: %v", id, err)
	}
	out := map[string]map[string]any{}
	for k, o := range api.Objects {
		out[k] = o
	}
	return in, out, spec.Namespace
}

func TestInhouseModuleChartsInstall(t *testing.T) {
	for _, m := range inhouseModules() {
		t.Run(m.id, func(t *testing.T) {
			_, objs, ns := enableInhouse(t, m.id, nil)
			d := objs["Deployment/"+ns+"/"+m.id]
			if d == nil {
				t.Fatalf("no Deployment %s; have %v", m.id, keys(objs))
			}
			if dig(d, "spec", "strategy", "type") != "Recreate" {
				t.Errorf("SQLite on a ReadWriteOnce volume needs strategy Recreate: %v", dig(d, "spec", "strategy"))
			}
			ps := dig(d, "spec", "template", "spec").(map[string]any)
			c := ps["containers"].([]any)[0].(map[string]any)
			if img, _ := c["image"].(string); !strings.HasPrefix(img, "ghcr.io/dodwmd/"+m.binary()+":") {
				t.Errorf("image %q is not taken from values", img)
			}
			sc := c["securityContext"].(map[string]any)
			if sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false || sc["runAsNonRoot"] != true {
				t.Errorf("securityContext not restricted: %v", sc)
			}
			if toJSON(sc["capabilities"]) != `{"drop":["ALL"]}` {
				t.Errorf("capabilities: %v", sc["capabilities"])
			}
			lv := toJSON(dig(c, "livenessProbe", "exec", "command"))
			want := fmt.Sprintf(`["/%s","healthcheck","-addr","127.0.0.1:%s"]`, m.binary(), m.port)
			if lv != want {
				t.Errorf("liveness %s, want the binary's own healthcheck %s", lv, want)
			}
			if p := dig(c, "readinessProbe", "httpGet", "path"); p != "/readyz" {
				t.Errorf("readiness path %v, want /readyz (liveness stays the binary's own /healthz check)", p)
			}
			if !strings.Contains(toJSON(c["env"]), fmt.Sprintf(`{"name":"CAN_RAM_DECLARED_MB","value":"%d"}`, m.ramMB)) && m.id != "equity" {
				t.Errorf("declared RAM env missing or not the manifest's ram_mb %d: %s", m.ramMB, toJSON(c["env"]))
			}
			res := c["resources"].(map[string]any)
			if got := toJSON(dig(res, "limits", "memory")); got != fmt.Sprintf(`"%dMi"`, m.limitMB) {
				t.Errorf("memory limit %s, manifest ram_limit_mb is %d", got, m.limitMB)
			}
			if got := toJSON(dig(res, "requests", "memory")); got != fmt.Sprintf(`"%dMi"`, m.ramMB) {
				t.Errorf("memory request %s, manifest ram_mb is %d", got, m.ramMB)
			}
			if strings.Contains(toJSON(ps["volumes"]), "hostPath") {
				t.Error("hostPath volume")
			}
			if !strings.Contains(toJSON(c["env"]), "CAN_"+strings.ToUpper(m.id)+"_CONFIG") {
				t.Errorf("the config path env is not set: %s", toJSON(c["env"]))
			}
			for _, s := range strings.Split(m.svcs, ",") {
				svc := objs["Service/"+ns+"/"+s]
				if svc == nil {
					t.Fatalf("no Service %s", s)
				}
				if tp := dig(svc, "spec", "type"); tp != "ClusterIP" {
					t.Errorf("service %s type %v", s, tp)
				}
			}
			pvc := objs["PersistentVolumeClaim/"+ns+"/"+m.id+"-data"]
			if pvc == nil || dig(pvc, "metadata", "annotations", "helm.sh/resource-policy") != "keep" {
				t.Errorf("data PVC missing or not kept: %v", pvc)
			}
			cm := objs["ConfigMap/"+ns+"/"+m.id+"-config"]
			if cm == nil {
				t.Fatalf("no ConfigMap %s-config", m.id)
			}
			raw, _ := dig(cm, "data", m.id+".yaml").(string)
			if raw == "" {
				t.Fatalf("ConfigMap lacks %s.yaml", m.id)
			}
			if err := m.parseConf([]byte(raw)); err != nil {
				t.Errorf("the module's own loader rejects the rendered config: %v\n%s", err, raw)
			}
			if strings.Contains(raw, "webhook:\n  url: http") {
				t.Error("events go to can-core through the module API, not a webhook URL")
			}
			for k := range objs {
				if strings.HasPrefix(k, "NetworkPolicy/"+ns+"/allow-intra") {
					t.Errorf("single-pod module must not add a NetworkPolicy by default: %s", k)
				}
			}
			routes := 0
			for k, o := range objs {
				if strings.HasPrefix(k, "IngressRoute/") && dig(o, "metadata", "labels", "can.dev/route") == nil {
					routes++
				}
			}
			if _, routed := inhouseSubdomains[m.id]; routed != (routes >= 1) {
				t.Errorf("%d IngressRoutes for %s (only modules with a subdomain are routed)", routes, m.id)
			}
		})
	}
}

func TestJobsIngressRouteIsScopedToPublicPaths(t *testing.T) {
	_, objs, ns := enableInhouse(t, "jobs", nil)
	r := objs["IngressRoute/"+ns+"/jobs"]
	if r == nil {
		t.Fatal("no IngressRoute")
	}
	js := toJSON(r["spec"])
	if !strings.Contains(js, "Host(`careers.acme.example`)") {
		t.Errorf("host: %s", js)
	}
	if strings.Contains(js, "/v1") {
		t.Errorf("the internal API must not be routed: %s", js)
	}
	for _, p := range []string{"/careers", "/feeds", "/robots.txt"} {
		if !strings.Contains(js, p) {
			t.Errorf("public path %s not routed: %s", p, js)
		}
	}
}

func TestInhouseChartsRefuseForeignRoutes(t *testing.T) {
	src, err := os.ReadFile("../../../modules/jobs/chart/templates/ingressroute.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.ReplaceAll(string(src), "Host(`{{ $host }}`)", "Host(`evil.example`)")
	if tampered == string(src) {
		t.Fatal("the template no longer has the Host(`{{ $host }}`) form this test tampers with")
	}
	dir := t.TempDir()
	copyTree(t, "../../../modules/jobs", filepath.Join(dir, "jobs"))
	if err := os.WriteFile(filepath.Join(dir, "jobs", "chart", "templates", "ingressroute.yaml"), []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	in, _, _, spec := fixture(t, nil)
	in.cfg.ModulesDir, in.cfg.Domain = dir, "acme.example"
	spec.ID, spec.Namespace, spec.ServiceAccount = "jobs", "mod-jobs", "jobs"
	spec.Manifest = loadManifest(t, "../../../modules/jobs/module.yaml")
	spec.Manifest.Requires.Database = "none"
	spec.Manifest.Runtime.Subdomain = "careers"
	if err := in.Enable(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("a route for a foreign host was accepted: %v", err)
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestProvisioningIngressRouteIsScopedToSCIM(t *testing.T) {
	_, objs, ns := enableInhouse(t, "provisioning", nil)
	r := objs["IngressRoute/"+ns+"/provisioning"]
	if r == nil {
		t.Fatal("no IngressRoute")
	}
	js := toJSON(r["spec"])
	if !strings.Contains(js, "Host(`scim.acme.example`)") || !strings.Contains(js, "/scim/v2") {
		t.Errorf("route: %s", js)
	}
	if strings.Contains(js, "/v1") || strings.Contains(js, "/healthz") {
		t.Errorf("the provisioning API must stay cluster-internal: %s", js)
	}
}
