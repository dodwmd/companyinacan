package contract

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
)

// The module-to-module connections (specs/057-module-capability-wiring) are declared three times:
// can-core's grants, the manifests and the admission policy. They must agree.
func TestModuleGrantsMatchManifestsAndAdmissionPolicy(t *testing.T) {
	manifest := func(id string) *config.Module {
		doc, ps := config.Load("../../modules/" + id + "/module.yaml")
		if doc == nil {
			t.Fatal(ps)
		}
		m, err := config.DecodeModule(doc)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	var pairs []string
	for _, g := range modsecrets.Grants() {
		consumer, provider := manifest(g.Consumer), manifest(g.Provider)
		if !slices.Contains(consumer.Requires.Capabilities, g.Capability) && !slices.Contains(consumer.Consumes, g.Capability) {
			t.Errorf("%s is granted %q but does not ask for it", g.Consumer, g.Capability)
		}
		if !slices.ContainsFunc(modsecrets.For(g.Provider), func(s modsecrets.Spec) bool { return s.Key == g.ProviderKey }) {
			t.Errorf("%s does not generate %s", g.Provider, g.ProviderKey)
		}
		port := 0
		for _, p := range provider.Provides {
			if p.Capability != g.Capability {
				continue
			}
			port = p.Port
			// A NetworkPolicy names the pod's port: the Service must not translate it.
			var values struct {
				Ports []struct {
					Name string `yaml:"name"`
					Port int    `yaml:"port"`
				} `yaml:"ports"`
				Services []struct {
					Name     string `yaml:"name"`
					PortName string `yaml:"portName"`
					Port     int    `yaml:"port"`
				} `yaml:"services"`
			}
			b, err := os.ReadFile("../../modules/" + g.Provider + "/chart/values.yaml")
			if err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal(b, &values); err != nil {
				t.Fatal(err)
			}
			ok := false
			for _, s := range values.Services {
				for _, pp := range values.Ports {
					ok = ok || (s.Name == p.Service && s.Port == p.Port && pp.Name == s.PortName && pp.Port == p.Port)
				}
			}
			if !ok {
				t.Errorf("%s: service %s:%d is not the pod's port in the chart", g.Provider, p.Service, p.Port)
			}
		}
		if port == 0 {
			t.Errorf("%s does not provide %q", g.Provider, g.Capability)
		}
		pairs = append(pairs, fmt.Sprintf("'%s>%s:%d'", g.Consumer, g.Provider, port))
	}
	sort.Strings(pairs)
	b, err := os.ReadFile("../../deploy/base/admission-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if want := `expression: "[` + strings.Join(pairs, ", ") + `]"`; !strings.Contains(string(b), want) {
		t.Errorf("admission policy can-core-networkpolicies must list exactly the granted pairs: %s", want)
	}
}
