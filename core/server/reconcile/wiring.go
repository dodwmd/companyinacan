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
// for and whose provider is enabled and serves it (and is the designated primary, if one is). A
// grant with an opt-in also needs the company to have set it.
//
// A connection the company opted into is wanted whatever the provider's state: the module is not
// installed without it (it would run in the mode the company turned away from). Otherwise a provider that has failed does not take away a connection the module already has (the consumer
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
		if g.Consumer != id || !ok || !uses(man, g.Capability) || !g.Asked(d.Company.Modules[id].Settings) {
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
		if g.OptInKey == "" && !running && !linksTo(recorded[id].Manifest.Runtime.Wired, g.Provider) {
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

// linkWidening is what an upgrade's connections and equity's modes newly allow or stop checking: a
// new connection (network access and a credential of another module), leaving a mode the company
// had opted into, and a new break-glass window.
func linkWidening(d Desired, id string, recorded store.Module, wanted []config.Link) []string {
	had := recorded.Manifest.Runtime.Wired
	var out []string
	for _, l := range wanted {
		if g, ok := modsecrets.GrantFor(id, l.Provider); ok && !linksTo(had, l.Provider) {
			out = append(out, "connects to "+l.Provider+": "+g.Gains)
		}
	}
	for _, l := range had {
		if g, ok := modsecrets.GrantFor(id, l.Provider); ok && g.OptInKey != "" && !linksTo(wanted, l.Provider) {
			out = append(out, "disconnects from "+l.Provider+": "+g.Loses)
		}
	}
	if until := breakGlass(d, id); until != "" {
		var was Applied
		if json.Unmarshal([]byte(recorded.Applied), &was) != nil || was.BreakGlass != until {
			out = append(out, "equity break-glass until "+until+": its service account may act without a person")
		}
	}
	return out
}

// breakGlass is the end of equity's break-glass window in company.yaml, "" for none.
func breakGlass(d Desired, id string) string {
	if id != "equity" {
		return ""
	}
	st, err := config.ReadEquitySettings(d.Company.Modules[id].Settings)
	if err != nil {
		return ""
	}
	return st.BreakGlassUntil
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

// ModeNotes says what a change of company.yaml does to how a module behaves, for an operator who
// applies it: was is the company.yaml in force (nil when there is none), next the new one.
func ModeNotes(was, next *config.Company) []string {
	read := func(c *config.Company) (config.EquitySettings, bool) {
		if c == nil {
			return config.EquitySettings{Identity: config.IdentityAttestation}, false
		}
		e, ok := c.Modules["equity"]
		st, _ := config.ReadEquitySettings(e.Settings)
		return st, ok && e.IsEnabled()
	}
	before, had := read(was)
	after, on := read(next)
	if !on {
		return nil
	}
	if !had {
		before = config.EquitySettings{Identity: config.IdentityAttestation}
	}
	var out []string
	g, _ := modsecrets.GrantFor("equity", "people")
	switch {
	case before.Identity == after.Identity:
	case after.Identity == config.IdentityDirectory:
		out = append(out, g.Gains+"; until both are done those actions are refused (docs/equity.md)")
	default:
		out = append(out, g.Loses)
	}
	if after.BreakGlassUntil != "" && after.BreakGlassUntil != before.BreakGlassUntil {
		out = append(out, "equity break-glass until "+after.BreakGlassUntil+": its service account may act without a person")
	}
	return out
}
