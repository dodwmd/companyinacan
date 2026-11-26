package launch

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/routing"
)

// QA: limits that an older can switched off may have been switched off by the flag too, so a later
// `can up` with dns.proxied does not record dns.proxied as the reason.
func TestRoutingFactsKeepAnUnknownEdgeLimitReasonUnknown(t *testing.T) {
	for name, c := range map[string]struct{ have, want string }{
		"older can, off":  {`{"data":{"edgeLimit":"off"}}`, `edgeLimitReason: ""`},
		"older can, on":   {`{"data":{"edgeLimit":"on"}}`, "edgeLimitReason: proxied"},
		"recorded before": {`{"data":{"edgeLimit":"off","edgeLimitReason":"proxied"}}`, "edgeLimitReason: proxied"},
		"new server":      {"", "edgeLimitReason: proxied"},
	} {
		k := &fakeKube{rules: []kubeRule{{contains: "get configmap can-routing", out: c.have}}}
		o := Options{Routes: RouteOptions{CatchAll: true, NoEdgeLimit: true, NoEdgeLimitReason: routing.ReasonProxied}}
		o.Company = &config.Company{Domain: "acme.example"}
		r := &runner{o: o, d: Deps{Kube: k}, res: &Result{}}
		if err := r.routingFacts(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		applied := k.called("apply")
		if len(applied) != 1 || !strings.Contains(applied[0].Stdin, c.want) {
			t.Errorf("%s: want %s in %+v", name, c.want, applied)
		}
	}
}
