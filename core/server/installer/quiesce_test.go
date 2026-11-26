package installer

import (
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
