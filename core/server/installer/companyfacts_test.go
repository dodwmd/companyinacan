package installer

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/jobs"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
)

func secretValue(t *testing.T, api *kube.Fake, ns, key string) string {
	t.Helper()
	enc, _ := dig(get(t, api, "Secret", ns, secretModule), "data", key).(string)
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const oddName = `Acme "Widgets" {{x}}: #1`

// installFacts installs a real module chart for a company with an awkward name, with the given
// routing fact for TLS ("" means no routing facts).
func installFacts(t *testing.T, id, tls string, public bool) (*kube.Fake, string) {
	t.Helper()
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir = "../../../modules"
	if tls != "" {
		if err := api.Apply(context.Background(), kube.Object{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "can-routing", "namespace": "can-system"},
			"data":     map[string]any{"domain": "acme.example", "tls": tls}}); err != nil {
			t.Fatal(err)
		}
	}
	spec.ID, spec.Namespace, spec.ServiceAccount = id, "mod-"+id, id
	spec.Manifest = loadManifest(t, "../../../modules/"+id+"/module.yaml")
	spec.Manifest.Requires.Database = "none"
	spec.Domain, spec.CompanyName = "acme.example", oddName
	spec.Expose.Public = &public
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	text, _ := dig(get(t, api, "ConfigMap", "mod-"+id, id+"-config"), "data", id+".yaml").(string)
	return api, text
}

// The jobs service refuses to start without company.name and company.base_url. Start it with what
// the installer wrote and nothing else.
func TestJobsStartsWithWhatTheInstallerWrites(t *testing.T) {
	api, text := installFacts(t, "jobs", "", true)
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := jobs.LoadConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Company.Name != oddName || cfg.Company.BaseURL != "https://careers.acme.example" {
		t.Fatalf("company = %+v", cfg.Company)
	}
	cfg.DBPath, cfg.ResumeDir, cfg.KeyringPath = filepath.Join(dir, "jobs.db"), filepath.Join(dir, "resumes"), filepath.Join(dir, "keyring")
	if err := os.WriteFile(cfg.KeyringPath, []byte(secretValue(t, api, "mod-jobs", modsecrets.KeyringKey)), 0o400); err != nil {
		t.Fatal(err)
	}
	sec, err := jobs.SecretsFromEnv(func(k string) string { return secretValue(t, api, "mod-jobs", k) })
	if err != nil {
		t.Fatal(err)
	}
	app, err := jobs.NewApp(cfg, sec, jobs.Options{})
	if err != nil {
		t.Fatalf("jobs does not start as installed: %v", err)
	}
	_ = app.Close()
}

func TestCompanyFactsFollowRoutingAndExposure(t *testing.T) {
	if _, c := installFacts(t, "jobs", "false", false); !strings.Contains(c, "base_url: http://careers.acme.example") {
		t.Errorf("plain HTTP cluster, unrouted careers pages: %s", c)
	}
	if _, c := installFacts(t, "files", "", true); !strings.Contains(c, "base_url: https://files.acme.example") {
		t.Errorf("files share links: %s", c)
	}
	if _, c := installFacts(t, "files", "", false); strings.Contains(c, "files.acme.example") {
		t.Errorf("an unrouted files module got a public base URL: %s", c)
	}
}

func TestProvisioningGetsThePeopleDirectoryToken(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir = "../../../modules"
	enable := func(id string) error {
		s := spec
		s.ID, s.Namespace, s.ServiceAccount = id, "mod-"+id, id
		s.Manifest = loadManifest(t, "../../../modules/"+id+"/module.yaml")
		s.Manifest.Requires.Database = "none"
		return in.Enable(context.Background(), s)
	}
	if err := enable("provisioning"); err == nil || !strings.Contains(err.Error(), "enable the people module") {
		t.Fatalf("without people: %v", err)
	}
	if err := enable("people"); err != nil {
		t.Fatal(err)
	}
	if err := enable("provisioning"); err != nil {
		t.Fatal(err)
	}
	tok := secretValue(t, api, "mod-people", modsecrets.PeopleDirectoryKey)
	if len(tok) < 24 || secretValue(t, api, "mod-provisioning", modsecrets.ProvisioningDirectoryKey) != tok {
		t.Fatal("provisioning does not hold the people directory token")
	}
}
