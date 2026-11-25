package installer

import (
	"context"
	"testing"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/kube"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// The stub module the cluster integration tests deploy must install cleanly through the installer
// (here against the fake API; test/integration runs it against a real cluster).
func TestStubModuleOfTheIntegrationSuiteInstalls(t *testing.T) {
	dir := "../../../test/integration/testdata/modules"
	doc, ps := config.Load(dir + "/stub/module.yaml")
	if doc == nil {
		t.Fatal(ps)
	}
	man, err := config.DecodeModule(doc)
	if err != nil {
		t.Fatal(err)
	}
	api := kube.NewFake()
	in := New(Config{API: api, ModulesDir: dir, PlatformURL: "http://can-core.can-system.svc.cluster.local:8090", NodeIP: "172.18.0.2", RolloutTimeout: time.Second, Poll: time.Millisecond})
	spec := reconcile.Spec{ID: "stub", Version: man.Version, Namespace: "mod-stub", ServiceAccount: "stub", Manifest: man, HMACSecret: "x", Settings: map[string]any{"greeting": "hi"}}
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	d := get(t, api, "Deployment", "mod-stub", "stub")
	c := dig(d, "spec", "template", "spec").(map[string]any)["containers"].([]any)[0].(map[string]any)
	if got := toJSON(c["env"]); !contains(got, "hi") {
		t.Fatalf("settings not mapped onto the chart: %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
