package installer

import (
	"context"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
)

// racerNamed makes another creator's Secret of the given name appear just before our Create.
type racerNamed struct {
	*kube.Fake
	name  string
	other map[string]string
}

func (r *racerNamed) Create(ctx context.Context, obj kube.Object) (bool, error) {
	md, _ := obj["metadata"].(map[string]any)
	if obj["kind"] == "Secret" && md["name"] == r.name && r.other != nil {
		sd := map[string]any{}
		for k, v := range r.other {
			sd[k] = v
		}
		r.other = nil
		o := kube.Object{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": r.name, "namespace": "mod-stub"}, "stringData": sd}
		if err := r.Apply(ctx, o); err != nil {
			return false, err
		}
	}
	return r.Fake.Create(ctx, obj)
}

// The generated database password is race-safe too: the loser of the create sets the database user
// to the winner's password, so the Secret and the database never disagree.
func TestConcurrentCreatorsAgreeOnTheDatabasePassword(t *testing.T) {
	in, api, db, spec := fixture(t, nil)
	spec.Manifest.Requires.Database = "postgres"
	in.cfg.SecretsFor = func(string) []modsecrets.Spec { return nil }
	const first = "first-creator-password-0123456789abcdef"
	in.cfg.API = &racerNamed{Fake: api, name: "can-db", other: map[string]string{"DB_PASSWORD": first}}
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	got := secretData(get(t, api, "Secret", "mod-stub", "can-db"))["DB_PASSWORD"]
	if got != first || db.Databases["postgres/mod_stub"] != first {
		t.Fatalf("secret %v and database %q disagree or overwrote the first creator", got, db.Databases["postgres/mod_stub"])
	}
}
