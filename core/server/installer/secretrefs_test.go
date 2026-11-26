package installer

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/dbprov"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// A chart that names a Secret or a key can-core does not write starts no pod: the kubelet waits for
// it forever (or, if optional, the setting is silently absent). So every reference of every module
// chart, with its adapter on, is checked against what the installer actually wrote.

// notWritten lists the references known to be wrong, with the reason. crm cannot be enabled
// (not-deployable): it needs MariaDB, which can-core does not provision, and an admin account that
// nothing generates (specs/019-wiki-crm-status). Remove the entry when crm is made installable.
var notWritten = map[string]string{
	"crm": "not-deployable: refers to can-database and admin-user/admin-password",
}

type secretUse struct{ secret, key, where string }

func secretUses(t *testing.T, api *kube.Fake) (out []secretUse) {
	t.Helper()
	for name, o := range api.Objects {
		switch o["kind"] {
		case "Deployment", "StatefulSet", "Job", "CronJob":
		default:
			continue
		}
		ps := podSpec(o)
		for _, group := range []string{"initContainers", "containers"} {
			for _, c := range asList(ps[group]) {
				for _, e := range asList(asMap(c)["env"]) {
					if ref := asMap(asMap(asMap(e)["valueFrom"])["secretKeyRef"]); ref != nil {
						s, _ := ref["name"].(string)
						k, _ := ref["key"].(string)
						out = append(out, secretUse{s, k, name})
					}
				}
				for _, e := range asList(asMap(c)["envFrom"]) {
					if ref := asMap(asMap(e)["secretRef"]); ref != nil {
						s, _ := ref["name"].(string)
						out = append(out, secretUse{s, "", name})
					}
				}
			}
		}
		for _, v := range asList(ps["volumes"]) {
			sv := asMap(asMap(v)["secret"])
			if sv == nil {
				continue
			}
			s, _ := sv["secretName"].(string)
			out = append(out, secretUse{s, "", name})
			for _, it := range asList(sv["items"]) {
				k, _ := asMap(it)["key"].(string)
				out = append(out, secretUse{s, k, name})
			}
		}
	}
	return out
}

func TestChartsOnlyReferenceSecretKeysTheInstallerWrites(t *testing.T) {
	ids := []string{"analytics", "support", "newsletter", "wiki", "crm", "status"}
	for _, m := range inhouseModules() {
		ids = append(ids, m.id)
	}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			if why := notWritten[id]; why != "" {
				t.Skip(why)
			}
			dir := t.TempDir()
			copyTree(t, "../../../modules/"+id, filepath.Join(dir, id))
			values := filepath.Join(dir, id, "chart", "values.yaml")
			b, err := os.ReadFile(values) //nolint:gosec // temp copy
			if err != nil {
				t.Fatal(err)
			}
			// the wrapped charts ship their adapter off; its references must be right when it is built
			on := strings.Replace(string(b), "adapter:\n  enabled: false", "adapter:\n  enabled: true", 1)
			if err := os.WriteFile(values, []byte(on), 0o600); err != nil {
				t.Fatal(err)
			}
			in, api, _, spec := fixture(t, nil)
			in.cfg.ModulesDir, in.cfg.Domain, in.cfg.DB = dir, "acme.example", dbprov.NewFake()
			in.cfg.Relay = func(reconcile.Spec) (*SMTPRelay, error) {
				return &SMTPRelay{Host: "smtp.example.net", Port: 587, Username: "u", Password: "p", From: "a@acme.example", Encryption: "tls"}, nil
			}
			spec.ID, spec.Namespace, spec.ServiceAccount, spec.Domain = id, "mod-"+id, id, "acme.example"
			spec.Manifest = loadManifest(t, "../../../modules/"+id+"/module.yaml")
			spec.Email = optIn(id)
			if sub, ok := inhouseSubdomains[id]; ok {
				spec.Manifest.Runtime.Subdomain = sub
			}
			if err := in.Enable(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			uses := secretUses(t, api)
			if len(uses) == 0 {
				t.Fatal("no Secret reference found: the walk is broken")
			}
			for _, u := range uses {
				o := api.Objects["Secret/mod-"+id+"/"+u.secret]
				if o == nil {
					t.Errorf("%s refers to Secret %s, which can-core does not write", u.where, u.secret)
					continue
				}
				if u.key == "" {
					continue
				}
				data, _ := o["data"].(map[string]any)
				v, ok := data[u.key].(string)
				if _, err := base64.StdEncoding.DecodeString(v); !ok || err != nil {
					t.Errorf("%s reads key %s of Secret %s, which can-core does not write", u.where, u.key, u.secret)
				}
			}
		})
	}
}
