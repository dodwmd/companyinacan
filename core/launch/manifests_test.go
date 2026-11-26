package launch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func decodeDocs(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var out []map[string]any
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("not valid yaml: %v\n%s", err, b)
		}
		if m != nil {
			out = append(out, m)
		}
	}
}

func kinds(docs []map[string]any) []string {
	var k []string
	for _, d := range docs {
		k = append(k, d["kind"].(string))
	}
	return k
}

func TestBaseManifestsFollowKustomization(t *testing.T) {
	b, err := BaseManifests(BaseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(kinds(decodeDocs(t, b)), ",")
	for _, want := range []string{"Namespace", "NetworkPolicy", "HelmChartConfig", "ValidatingAdmissionPolicy", "ResourceQuota"} {
		if !strings.Contains(got, want) {
			t.Errorf("base is missing %s: %s", want, got)
		}
	}
}

func TestBaseManifestsAddACMEEmailOnlyWhenGiven(t *testing.T) {
	plain, _ := BaseManifests(BaseOptions{})
	if strings.Contains(string(plain), "email:") {
		t.Fatal("no email unless asked for")
	}
	b, err := BaseManifests(BaseOptions{ACMEEmail: "ops@acme.example"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "email: ops@acme.example") {
		t.Fatalf("email not in the traefik values:\n%s", b)
	}
	if _, err := BaseManifests(BaseOptions{ACMEEmail: "x\ninjected: true"}); err == nil {
		t.Fatal("an email with a newline must be refused")
	}
}

func envOf(t *testing.T, doc map[string]any) map[string]string {
	t.Helper()
	spec := doc["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	c := spec["containers"].([]any)[0].(map[string]any)
	out := map[string]string{}
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		if v, ok := m["value"].(string); ok {
			out[m["name"].(string)] = v
		}
	}
	return out
}

func deployment(t *testing.T, docs []map[string]any, name string) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == "Deployment" && d["metadata"].(map[string]any)["name"] == name {
			return d
		}
	}
	t.Fatalf("no Deployment %s", name)
	return nil
}

func TestCoreManifestsPatchImageAndEnv(t *testing.T) {
	b, err := CoreManifests(CoreOptions{Image: "ghcr.io/dodwmd/can-core@" + goodDigest, CompanyName: "Acme Co", Domain: "acme.example", CookieSecure: false})
	if err != nil {
		t.Fatal(err)
	}
	docs := decodeDocs(t, b)
	d := deployment(t, docs, "can-core")
	c := d["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if c["image"] != "ghcr.io/dodwmd/can-core@"+goodDigest {
		t.Fatalf("image %v", c["image"])
	}
	env := envOf(t, d)
	if env["CAN_COOKIE_SECURE"] != "false" || env["CAN_COMPANY_NAME"] != "Acme Co" || env["CAN_COMPANY_DOMAIN"] != "acme.example" {
		t.Fatalf("env %v", env)
	}
	if env["CAN_INSTALLER"] != "kubernetes" || env["CAN_TOKEN_REVIEW"] != "kubernetes" {
		t.Fatalf("the production settings must stay: %v", env)
	}
	b2, _ := CoreManifests(CoreOptions{Image: "can-core:dev", CookieSecure: true})
	if envOf(t, deployment(t, decodeDocs(t, b2), "can-core"))["CAN_COOKIE_SECURE"] != "true" {
		t.Fatal("secure cookies when TLS is on")
	}
	if _, err := CoreManifests(CoreOptions{Image: "bad image;rm"}); err == nil {
		t.Fatal("an invalid image reference must be refused")
	}
}

func TestSiteManifestsStampArchiveDigest(t *testing.T) {
	b, err := SiteManifests("can-static:dev", strings.Repeat("a", 64), "")
	if err != nil {
		t.Fatal(err)
	}
	d := deployment(t, decodeDocs(t, b), "site")
	tpl := d["spec"].(map[string]any)["template"].(map[string]any)
	if tpl["metadata"].(map[string]any)["annotations"].(map[string]any)["can.dev/site-sha256"] != strings.Repeat("a", 64) {
		t.Fatalf("annotation: %v", tpl["metadata"])
	}
	spec := tpl["spec"].(map[string]any)
	if spec["initContainers"].([]any)[0].(map[string]any)["image"] != "can-static:dev" || spec["containers"].([]any)[0].(map[string]any)["image"] != "can-static:dev" {
		t.Fatal("both containers must use the resolved image")
	}
}

func TestSiteRoutes(t *testing.T) {
	b, err := SiteRoutes(RouteOptions{SiteHosts: []string{"acme.example"}, AdminHosts: []string{"admin.acme.example"}, TLS: true, CertResolver: "letsencrypt"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"Host(`acme.example`)", "Host(`admin.acme.example`)", "websecure", "certResolver: letsencrypt", "name: can-core", "port: 8080", "name: site", "port: 8081"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	plain, _ := SiteRoutes(RouteOptions{CatchAll: true})
	if !strings.Contains(string(plain), "PathPrefix(`/`)") || strings.Contains(string(plain), "websecure") || strings.Contains(string(plain), "can-core") {
		t.Fatalf("plain http fallback: public site only, no TLS, no admin:\n%s", plain)
	}
	for _, bad := range []string{"a`) || Host(`b", "a b", "", "A_B", "x/../y"} {
		if _, err := SiteRoutes(RouteOptions{SiteHosts: []string{bad}}); err == nil {
			t.Errorf("host %q must be refused", bad)
		}
	}
	if _, err := SiteRoutes(RouteOptions{}); err == nil {
		t.Fatal("no hosts and no catch-all must be refused")
	}
}

func TestSiteRouteHasLowPriority(t *testing.T) {
	site, _ := SiteRoutes(RouteOptions{SiteHosts: []string{"acme.example"}})
	if !strings.Contains(string(site), "priority: 1\n") {
		t.Fatalf("the site catch-all must have a low priority (can-core's /_forms route outranks it):\n%s", site)
	}
}

func TestCompanyConfigMapDefaultBundle(t *testing.T) {
	src := "schema: 1\nname: Acme\ndomain: acme.example\nprovider:\n  type: ssh\nmodules:\n  example: {}\n"
	out, added, err := WithDefaultBundle([]byte(src))
	if err != nil || !added {
		t.Fatalf("%v %v", err, added)
	}
	if !strings.Contains(string(out), "forms") || !strings.Contains(string(out), "example") {
		t.Fatalf("forms must be added next to the existing modules:\n%s", out)
	}
	again, added2, err := WithDefaultBundle(out)
	if err != nil || added2 || string(again) != string(out) {
		t.Fatalf("adding twice must change nothing: %v %v", err, added2)
	}
	none, added3, err := WithDefaultBundle([]byte("schema: 1\nname: A\ndomain: a.example\nprovider:\n  type: ssh\n"))
	if err != nil || !added3 || !strings.Contains(string(none), "forms") {
		t.Fatalf("a company without modules gets the bundle: %v %v\n%s", err, added3, none)
	}
	off, added4, err := WithDefaultBundle([]byte("schema: 1\nname: A\ndomain: a.example\nprovider:\n  type: ssh\nmodules:\n  forms: {enabled: false}\n"))
	if err != nil || added4 || !strings.Contains(string(off), "enabled: false") {
		t.Fatalf("an operator who disabled forms keeps it disabled: %v %v", err, added4)
	}
	cm, err := CompanyConfigMap(string(out))
	if err != nil {
		t.Fatal(err)
	}
	d := decodeDocs(t, []byte(cm))[0]
	if d["metadata"].(map[string]any)["name"] != "company" || d["metadata"].(map[string]any)["namespace"] != "can-system" {
		t.Fatalf("%v", d)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	return filepath.Join(wd, "..", "..")
}

func TestBuildSiteArchiveIsDeterministicAndComplete(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "examples", "acme-co")
	a, err := BuildSiteArchive(dir, "http://acme.example", 900<<10)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildSiteArchive(dir, "http://acme.example", 900<<10)
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != b.SHA256 || !bytes.Equal(a.TarGz, b.TarGz) {
		t.Fatal("the archive must be byte-identical for the same input, or every re-run would roll the site pod")
	}
	files := map[string]string{}
	gz, err := gzip.NewReader(bytes.NewReader(a.TarGz))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		files[h.Name] = string(body)
	}
	if !strings.Contains(files["index.html"], "Heavy things, handled.") {
		t.Fatalf("index.html lacks the example headline; files: %d", len(files))
	}
	if _, err := BuildSiteArchive(dir, "http://acme.example", 100); err == nil || !strings.Contains(err.Error(), "ConfigMap") {
		t.Fatalf("a site over the ConfigMap limit must be refused with a clear message, got %v", err)
	}
	cm := SiteContentConfigMap(a)
	if !strings.Contains(cm, "binaryData") || !strings.Contains(cm, "site.tar.gz") || !strings.Contains(cm, "name: site-content") {
		t.Fatalf("configmap:\n%.300s", cm)
	}
}

// A rebuilt image keeps its tag: the pod is replaced only if the manifest changes with the bytes.
func TestARebuiltImageUnderTheSameTagChangesThePodTemplate(t *testing.T) {
	core := func(sha string) string {
		b, err := CoreManifests(CoreOptions{Image: "can-core:dev", CookieSecure: true, ImageSHA256: sha})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	site := func(sha string) string {
		b, err := SiteManifests("can-static:dev", strings.Repeat("a", 64), sha)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	one, two := strings.Repeat("1", 64), strings.Repeat("2", 64)
	for name, m := range map[string]func(string) string{"can-core": core, "site": site} {
		first, again, rebuilt := m(one), m(one), m(two)
		if first != again || first == rebuilt || !strings.Contains(first, "can.dev/image-sha256") || !strings.Contains(first, one) {
			t.Errorf("%s: the image checksum is not on the pod template", name)
		}
		if strings.Contains(m(""), "can.dev/image-sha256") {
			t.Errorf("%s: a release image needs no annotation", name)
		}
	}
}
