package installer

import (
	"context"
	"testing"

	"github.com/dodwmd/companyinacan/core/server/kube"
)

func TestQuiesce(t *testing.T) {
	obj := func(kind string) kube.Object {
		return kube.Object{"kind": kind, "metadata": map[string]any{"name": "x"}, "spec": map[string]any{"replicas": 3}}
	}
	got := map[string]map[string]any{}
	for _, o := range quiesce([]kube.Object{obj("Deployment"), obj("StatefulSet"), obj("CronJob"), obj("Job"), obj("Ingress"), obj("IngressRoute"), obj("Service"), obj("PersistentVolumeClaim")}) {
		got[o["kind"].(string)] = o["spec"].(map[string]any)
	}
	for _, gone := range []string{"Job", "Ingress", "IngressRoute"} {
		if _, ok := got[gone]; ok {
			t.Error(gone, "must not be applied during a restore")
		}
	}
	if got["Deployment"]["replicas"] != 0 || got["StatefulSet"]["replicas"] != 0 || got["CronJob"]["suspend"] != true {
		t.Error(got)
	}
	if got["Service"] == nil || got["PersistentVolumeClaim"] == nil {
		t.Error("volumes and services are kept")
	}
}

// Restoring over an existing server takes its public routes down.
func TestRemoveRoutes(t *testing.T) {
	ctx := context.Background()
	api := kube.NewFake()
	for _, kind := range []string{"IngressRoute", "Ingress", "Service"} {
		if err := api.Apply(ctx, kube.Object{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": "x", "namespace": "mod-a"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := New(Config{API: api}).removeRoutes(ctx, "mod-a"); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]bool{"IngressRoute": false, "Ingress": false, "Service": true} {
		if _, err := api.Get(ctx, kind, "mod-a", "x"); (err == nil) != want {
			t.Error(kind, err)
		}
	}
}
