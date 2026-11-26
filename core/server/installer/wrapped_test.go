package installer

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/dbprov"
	"github.com/dodwmd/companyinacan/core/server/kube"
)

// The wrapped modules that are installed on a cluster in CI (specs/041): what the installer gives
// them, checked against the fake API.

func enableWrapped(t *testing.T, id string, relay *SMTPRelay) (*kube.Fake, func()) {
	t.Helper()
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir, in.cfg.Domain = "../../../modules", "acme.example"
	in.cfg.DB = dbprov.NewFake()
	current := relay
	in.cfg.Relay = func(string) (*SMTPRelay, error) { return current, nil }
	spec.ID, spec.Namespace, spec.ServiceAccount = id, "mod-"+id, id
	spec.Domain = "acme.example"
	spec.Manifest = loadManifest(t, "../../../modules/"+id+"/module.yaml")
	enable := func() {
		t.Helper()
		if err := in.Enable(context.Background(), spec); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	enable()
	return api, func() { current = nil; enable() }
}

func decoded(t *testing.T, api *kube.Fake, ns, name string) map[string]string {
	t.Helper()
	o := api.Objects["Secret/"+ns+"/"+name]
	if o == nil {
		t.Fatalf("no Secret %s/%s", ns, name)
	}
	data, _ := o["data"].(map[string]any)
	out := map[string]string{}
	for k, v := range data {
		b, err := base64.StdEncoding.DecodeString(v.(string))
		if err != nil {
			t.Fatal(err)
		}
		out[k] = string(b)
	}
	return out
}

func TestWrappedModulesGetAGeneratedAdminPassword(t *testing.T) {
	for id, key := range map[string]string{"analytics": "CAN_ANALYTICS_ADMIN_PASSWORD", "support": "CAN_SUPPORT_ADMIN_PASSWORD", "newsletter": "CAN_NEWSLETTER_ADMIN_PASSWORD"} {
		api, _ := enableWrapped(t, id, nil)
		if pw := decoded(t, api, "mod-"+id, "can-module")[key]; len(pw) < 32 || len(pw) > 50 {
			t.Errorf("%s: no generated %s (%d characters)", id, key, len(pw))
		}
		d := api.Objects["Deployment/mod-"+id+"/"+id]
		if js := toJSON(d); !strings.Contains(js, key) || !strings.Contains(js, "@sha256:") {
			t.Errorf("%s: the Deployment does not use %s or an image pinned by digest", id, key)
		}
	}
}

func TestWrappedAdminEmailAndHostComeFromTheCompanyDomain(t *testing.T) {
	for id, sub := range map[string]string{"analytics": "stats", "support": "support"} {
		api, _ := enableWrapped(t, id, nil)
		js := toJSON(api.Objects["Deployment/mod-"+id+"/"+id])
		for _, want := range []string{sub + ".acme.example", "admin@acme.example"} {
			if !strings.Contains(js, want) {
				t.Errorf("%s: the Deployment lacks %s", id, want)
			}
		}
	}
}

func TestRelayIsGivenOnlyToSMTPAppsAndWithdrawnAgain(t *testing.T) {
	relay := &SMTPRelay{Host: "smtp.example.net", Port: 587, Username: "u", Password: "p", From: "no-reply@acme.example", Encryption: "tls"}
	api, withdraw := enableWrapped(t, "support", relay)
	got := decoded(t, api, "mod-support", "can-smtp")
	if got["host"] != "smtp.example.net" || got["port"] != "587" || got["password"] != "p" || got["encryption"] != "tls" || got["from"] != "no-reply@acme.example" {
		t.Errorf("can-smtp = %v", got)
	}
	version := toJSON(api.Objects["Deployment/mod-support/support"]["spec"])
	withdraw()
	if got := decoded(t, api, "mod-support", "can-smtp"); len(got) != 0 {
		t.Errorf("a withdrawn relay left credentials behind: %d keys", len(got))
	}
	if toJSON(api.Objects["Deployment/mod-support/support"]["spec"]) == version {
		t.Error("the pods are not rolled when the relay changes")
	}
	for _, id := range []string{"analytics", "newsletter"} {
		api, _ := enableWrapped(t, id, relay)
		if api.Objects["Secret/mod-"+id+"/can-smtp"] != nil {
			t.Errorf("%s was handed the relay credentials", id)
		}
	}
}
