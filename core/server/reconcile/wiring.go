package reconcile

// Which modules can-core connects to which (specs/057-module-capability-wiring).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/server/modsecrets"
	"github.com/dodwmd/companyinacan/core/server/store"
)

// uses reports whether the manifest asks for the capability.
func uses(m *config.Module, capability string) bool {
	return slices.Contains(m.Requires.Capabilities, capability) || slices.Contains(m.Consumes, capability)
}

func linksTo(ls []config.Link, provider string) bool {
	return slices.ContainsFunc(ls, func(l config.Link) bool { return l.Provider == provider })
}

// links are the connections module id is to have: one per grant whose capability the module asks
// for and whose provider is enabled and serves it (and is the designated primary, if one is).
//
// A provider that has failed does not take away a connection the module already has (the consumer
// fails closed instead of silently changing how it behaves), and gives none to a module that has
// none. planning also counts a provider this apply is still to install, which it does first.
func links(d Desired, recorded map[string]store.Module, id string, planning bool) []config.Link {
	enabled := d.enabled()
	man, ok := enabled[id]
	if !ok {
		return nil
	}
	var out []config.Link
	for _, g := range modsecrets.Grants() {
		prov, ok := enabled[g.Provider]
		if g.Consumer != id || !ok || !uses(man, g.Capability) {
			continue
		}
		if p := d.Company.Primary[g.Capability]; p != "" && p != g.Provider {
			continue
		}
		rec, have := recorded[g.Provider]
		running := have && rec.State == store.StateEnabled
		if planning {
			running = !have || rec.State != store.StateFailed
		}
		if !running && !linksTo(recorded[id].Manifest.Runtime.Wired, g.Provider) {
			continue
		}
		for _, p := range prov.Provides {
			if p.Capability == g.Capability && config.ProvidesServed(p.Status) {
				out = append(out, config.Link{Capability: g.Capability, Provider: g.Provider, Service: p.Service, Port: p.Port})
				break
			}
		}
	}
	return out
}

func byID(ms []store.Module) map[string]store.Module {
	out := map[string]store.Module{}
	for _, m := range ms {
		out[m.ID] = m
	}
	return out
}

// linkedHash adds the connections to a spec hash, so that gaining or losing one is an upgrade.
func linkedHash(hash string, ls []config.Link) string {
	if len(ls) == 0 {
		return hash
	}
	b, _ := json.Marshal(ls)
	sum := sha256.Sum256(append([]byte(hash+"\x00links\x00"), b...))
	return hex.EncodeToString(sum[:])
}

// linkNote says what a step changes about a module's connections ("" for nothing).
func linkNote(consumer string, had, want []config.Link) string {
	var out []string
	for _, l := range want {
		if g, ok := modsecrets.GrantFor(consumer, l.Provider); ok && !linksTo(had, l.Provider) {
			out = append(out, "connects to "+l.Provider+": "+g.Gains)
		}
	}
	for _, l := range had {
		if g, ok := modsecrets.GrantFor(consumer, l.Provider); ok && !linksTo(want, l.Provider) {
			out = append(out, "disconnects from "+l.Provider+": "+g.Loses)
		}
	}
	return strings.Join(out, "; ")
}
