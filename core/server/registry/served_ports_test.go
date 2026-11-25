package registry

import (
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
)

// The default subscription port comes from the first SERVED provision. A planned provision
// declares a port for the contract only; no adapter listens there.
func TestFromManifestDefaultPortSkipsPlannedProvision(t *testing.T) {
	sub := func(m *config.Module) {
		m.Subscribes = append(m.Subscribes, struct {
			Event   string `yaml:"event"`
			Service string `yaml:"service"`
			Port    int    `yaml:"port"`
			Webhook string `yaml:"webhook"`
			Status  string `yaml:"status"`
		}{Event: "lead.created.v1", Webhook: "/hooks/lead"})
	}
	m := manifest("m", "a", "b") // ports 8080 (a), 8081 (b)
	m.Provides[0].Status = "planned"
	sub(m)
	_, subs, err := FromManifest(m)
	if err != nil || len(subs) != 1 || subs[0].Port != 8081 {
		t.Fatalf("default port = %+v, %v; want the first served provision's 8081", subs, err)
	}
	all := manifest("n", "a")
	all.Provides[0].Status = "planned"
	sub(all)
	_, subs, err = FromManifest(all)
	if err != nil || len(subs) != 1 || subs[0].Port != 80 {
		t.Fatalf("with every provision planned the default must be 80: %+v, %v", subs, err)
	}
}
