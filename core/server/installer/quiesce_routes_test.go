package installer

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// A restore install (specs/042-restore) of a module that company.yaml exposes publishes nothing
// and runs nothing, also over a release that was routed; the next normal apply brings both back.
func TestQuiescedInstallRoutesNothing(t *testing.T) {
	in, api, spec := routed(t, "work", config.Expose{UI: true, API: true})
	enable(t, in, spec) // a server being overwritten already has the routes
	in.cfg.Quiesce = true
	enable(t, in, spec)
	for k := range api.Objects {
		if strings.HasPrefix(k, "IngressRoute/") {
			t.Errorf("%s during a restore", k)
		}
	}
	if got := traefikPorts(t, api, spec.Namespace); got != "[]" {
		t.Errorf("the ingress controller is admitted during a restore: %s", got)
	}
	if got := toJSON(dig(get(t, api, "Deployment", spec.Namespace, "work"), "spec", "replicas")); got != "0" {
		t.Errorf("replicas during a restore: %s", got)
	}
	in.cfg.Quiesce = false
	enable(t, in, spec)
	if !exists(api, "IngressRoute", spec.Namespace, routeWeb) {
		t.Error("the route did not come back")
	}
}
