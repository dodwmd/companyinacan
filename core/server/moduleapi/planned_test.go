package moduleapi

import (
	"context"
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// /v1/self reports served capabilities only.
func TestSelfSkipsPlannedProvisions(t *testing.T) {
	h := newHarness(t)
	m := manifest("ghost")
	for _, p := range []struct{ c, status string }{{"analytics", "planned"}, {"tickets2", ""}} {
		m.Provides = append(m.Provides, struct {
			Capability string `yaml:"capability"`
			Schema     string `yaml:"schema"`
			Service    string `yaml:"service"`
			Port       int    `yaml:"port"`
			Status     string `yaml:"status"`
		}{Capability: p.c, Service: "ghost", Port: 8081, Status: p.status})
	}
	if err := h.reg.Register(context.Background(), store.Module{ID: "ghost", Version: m.Version, Namespace: "mod-ghost", ServiceAccount: "ghost", State: store.StateEnabled, HMACSecret: "s", Manifest: config.Module(m)}); err != nil {
		t.Fatal(err)
	}
	h.tr.Add("tok-ghost", "mod-ghost", "ghost")
	resp, b := h.call("GET", "/v1/self", "tok-ghost", nil)
	if resp.StatusCode != 200 || strings.Contains(string(b), "analytics") || !strings.Contains(string(b), "tickets2") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}
