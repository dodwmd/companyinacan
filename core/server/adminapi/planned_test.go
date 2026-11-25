package adminapi

import (
	"strings"
	"testing"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// The admin module view lists served capabilities only; a planned one is not offered.
func TestViewSkipsPlannedProvisions(t *testing.T) {
	var m config.Module
	for _, p := range []struct {
		c, status string
	}{{"status", "planned"}, {"tickets", ""}} {
		m.Provides = append(m.Provides, struct {
			Capability string `yaml:"capability"`
			Schema     string `yaml:"schema"`
			Service    string `yaml:"service"`
			Port       int    `yaml:"port"`
			Status     string `yaml:"status"`
		}{Capability: p.c, Status: p.status})
	}
	v := view(store.Module{ID: "x", Manifest: m})
	if strings.Join(v.Provides, ",") != "tickets" {
		t.Fatalf("provides = %v, want only the served capability", v.Provides)
	}
}
