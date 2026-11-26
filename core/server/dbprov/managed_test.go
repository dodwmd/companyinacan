package dbprov

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

func TestManagedCreatesTheServerAndKeepsTheAdminPassword(t *testing.T) {
	api := kube.NewFake()
	var urls []string
	fails := 2 // a fresh server is not accepting connections yet
	m := &Managed{API: api, Poll: time.Millisecond, provision: func(_ context.Context, url, id, pw string) (Credentials, error) {
		urls = append(urls, url)
		if fails > 0 {
			fails--
			return Credentials{}, errors.New("connection refused")
		}
		return Credentials{Kind: "postgres", Database: "mod_" + id, Password: pw}, nil
	}}
	ctx := context.Background()
	c, err := m.Ensure(ctx, "postgres", "support", strings.Repeat("a", 32))
	if err != nil || c.Database != "mod_support" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, k := range []string{"Deployment/can-data/postgres", "Service/can-data/postgres", "PersistentVolumeClaim/can-data/postgres-data", "Secret/can-data/can-postgres"} {
		if api.Objects[k] == nil {
			t.Errorf("missing %s", k)
		}
	}
	data, _ := api.Objects["Secret/can-data/can-postgres"]["data"].(map[string]any)
	pw, err := base64.StdEncoding.DecodeString(data["password"].(string))
	if err != nil || len(pw) != 48 || !strings.Contains(urls[0], ":"+string(pw)+"@postgres.can-data.svc") {
		t.Fatalf("admin password not generated or not used (%d bytes, %v)", len(pw), err)
	}
	first := urls[0]
	if _, err := m.Ensure(ctx, "postgres", "newsletter", strings.Repeat("b", 32)); err != nil {
		t.Fatal(err)
	}
	if urls[len(urls)-1] != first {
		t.Error("the admin password changed on a second module: the data directory keeps the first one")
	}
}

func TestManagedRefusesOtherEnginesAndGivesUp(t *testing.T) {
	m := &Managed{API: kube.NewFake(), Wait: time.Millisecond, Poll: time.Millisecond,
		provision: func(context.Context, string, string, string) (Credentials, error) {
			return Credentials{}, errors.New("no")
		}}
	if _, err := m.Ensure(context.Background(), "mariadb", "crm", strings.Repeat("a", 32)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("mariadb: %v", err)
	}
	if _, err := m.Ensure(context.Background(), "postgres", "support", strings.Repeat("a", 32)); err == nil || !strings.Contains(err.Error(), "did not accept") {
		t.Errorf("unreachable server: %v", err)
	}
}

func TestManagedImageIsPinnedByDigest(t *testing.T) {
	objs, err := manifests()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o["kind"] != "Deployment" {
			continue
		}
		spec := o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		img := spec["containers"].([]any)[0].(map[string]any)["image"].(string)
		_, digest, ok := strings.Cut(img, "@sha256:")
		if !ok || len(digest) != 64 {
			t.Errorf("image %q is not pinned by digest", img)
		}
		return
	}
	t.Fatal("no Deployment in the postgres manifests")
}

// An installation that used an external server and lost CAN_POSTGRES_ADMIN_URL must not get an empty
// managed one: every sign of the other server refuses, and nothing is created.
func TestManagedFailsClosedWhenTheDatabasesAreElsewhere(t *testing.T) {
	ctx := context.Background()
	external := &External{API: kube.NewFake(), Inner: NewFake()}
	if _, err := external.Ensure(ctx, "postgres", "support", strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	foreign := kube.NewFake()
	if err := foreign.Apply(ctx, kube.Object{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "postgres", "namespace": "can-data"}}); err != nil {
		t.Fatal(err)
	}
	moved := kube.NewFake()
	if err := moved.Apply(ctx, kube.Object{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "can-db", "namespace": "mod-support"},
		"stringData": map[string]any{"DB_HOST": "db.internal.example"}}); err != nil {
		t.Fatal(err)
	}
	for name, api := range map[string]kube.API{"marker": external.API, "foreign service": foreign, "module database elsewhere": moved} {
		m := &Managed{API: api, provision: func(context.Context, string, string, string) (Credentials, error) {
			t.Errorf("%s: provisioned on the managed server", name)
			return Credentials{}, nil
		}}
		if _, err := m.Ensure(ctx, "postgres", "support", strings.Repeat("a", 32)); !errors.Is(err, ErrWasExternal) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := api.Get(ctx, "Deployment", "can-data", "postgres"); !kube.IsNotFound(err) {
			t.Errorf("%s: a server was deployed", name)
		}
	}
}
