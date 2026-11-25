package modmgr

import (
	"sort"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/validate"
)

// Footprint constants (SPEC section 3.2 budget; estimates, not measurements of the chosen server).
const (
	// PlatformBaselineMB is k3s, Traefik, CoreDNS, local-path and can-core with the site.
	PlatformBaselineMB = 650
	// DatabaseEngineMB is a shared Postgres or MariaDB engine, counted once if any module needs one.
	DatabaseEngineMB = 150
	// TightPercent is the share of the server above which the meter says "tight".
	TightPercent = 85
)

// FootprintResult is the live "does it fit in your server" total.
type FootprintResult struct {
	PlatformMB int
	RequestMB  int // sum of declared ram_mb
	LimitMB    int // sum of limits (declared, default 1.5x)
	DatabaseMB int
	TotalMB    int // PlatformMB + RequestMB + DatabaseMB
	ServerMB   int // 0 = not declared
	Verdict    string
	OverByMB   int
	// UsedPercent is TotalMB as a share of ServerMB (0 when unknown), capped at 999.
	UsedPercent int
}

// Footprint adds up the declared RAM of modules against the declared server size. An undeclared
// server size yields the verdict "unknown", never "fits".
func Footprint(mods []*config.Module, serverMB int) FootprintResult {
	f := FootprintResult{PlatformMB: PlatformBaselineMB, ServerMB: serverMB}
	engine := false
	for _, m := range mods {
		f.RequestMB += m.Resources.RAMMB
		lim := m.Resources.RAMLimitMB
		if lim == 0 {
			lim = m.Resources.RAMMB * 3 / 2
		}
		f.LimitMB += lim
		if d := m.Requires.Database; d == "postgres" || d == "mariadb" {
			engine = true
		}
	}
	if engine {
		f.DatabaseMB = DatabaseEngineMB
	}
	f.TotalMB = f.PlatformMB + f.RequestMB + f.DatabaseMB
	switch {
	case serverMB <= 0:
		f.Verdict = "unknown"
	case f.TotalMB > serverMB:
		f.Verdict, f.OverByMB = "over", f.TotalMB-serverMB
	case f.TotalMB*100 > serverMB*TightPercent:
		f.Verdict = "tight"
	default:
		f.Verdict = "fits"
	}
	if serverMB > 0 {
		f.UsedPercent = min(f.TotalMB*100/serverMB, 999)
	}
	return f
}

// Need states.
const (
	NeedMet      = "met"      // a selected module serves it
	NeedPlatform = "platform" // can-core itself provides it
	NeedPlanned  = "planned"  // only a planned (not yet served) provision exists
	NeedMissing  = "missing"  // nothing selected serves it
	NeedNone     = "none"     // no module in the catalogue provides it
)

// Need is one required capability of a selected module.
type Need struct {
	Module, Capability, State string
	Providers                 []string // selected modules serving it (NeedMet)
	Available                 []string // unselected modules that could serve it (NeedMissing)
}

// Provision is one capability a selected module declares.
type Provision struct {
	Module, Capability string
	Served             bool // false: planned, "not yet served"
}

// Conflict is an exclusive capability served by more than one selected module without a primary.
type Conflict struct {
	Capability string
	Modules    []string
}

// Analysis is the requires/provides graph of a selection.
type Analysis struct {
	Needs     []Need
	Provides  []Provision
	Conflicts []Conflict
}

// Need returns the need of module for capability, or nil.
func (a Analysis) Need(module, capability string) *Need {
	for i := range a.Needs {
		if a.Needs[i].Module == module && a.Needs[i].Capability == capability {
			return &a.Needs[i]
		}
	}
	return nil
}

// Provided returns the provision of module for capability, or nil.
func (a Analysis) Provided(module, capability string) *Provision {
	for i := range a.Provides {
		if a.Provides[i].Module == module && a.Provides[i].Capability == capability {
			return &a.Provides[i]
		}
	}
	return nil
}

// Blocking reports whether the selection cannot work: an unmet required capability or an exclusive
// capability with two providers and no primary. Planned-only needs block too (nothing serves them).
func (a Analysis) Blocking() bool {
	if len(a.Conflicts) > 0 {
		return true
	}
	for _, n := range a.Needs {
		if n.State == NeedMissing || n.State == NeedPlanned || n.State == NeedNone {
			return true
		}
	}
	return false
}

// Analyse builds the requires/provides graph for the selected module ids. primary is the company's
// capability -> module designation.
func (c *Catalogue) Analyse(selected map[string]bool, primary map[string]string) Analysis {
	var a Analysis
	known := validate.Capabilities()
	served := map[string][]string{} // capability -> selected serving modules
	planned := map[string][]string{}
	ids := sortedSelected(selected, c)
	for _, id := range ids {
		for _, p := range c.Get(id).Manifest.Provides {
			ok := config.ProvidesServed(p.Status)
			a.Provides = append(a.Provides, Provision{Module: id, Capability: p.Capability, Served: ok})
			if ok {
				served[p.Capability] = append(served[p.Capability], id)
			} else {
				planned[p.Capability] = append(planned[p.Capability], id)
			}
		}
	}
	for _, id := range ids {
		for _, cp := range c.Get(id).Manifest.Requires.Capabilities {
			n := Need{Module: id, Capability: cp}
			switch {
			case len(others(served[cp], id)) > 0:
				n.State, n.Providers = NeedMet, others(served[cp], id)
			case known[cp].Platform:
				n.State = NeedPlatform
			case len(planned[cp]) > 0:
				n.State = NeedPlanned
			default:
				n.Available = c.providersOf(cp, selected, true)
				switch {
				case len(n.Available) > 0:
					n.State = NeedMissing
				case len(c.providersOf(cp, selected, false)) > 0:
					n.State = NeedPlanned
				default:
					n.State = NeedNone
				}
			}
			a.Needs = append(a.Needs, n)
		}
	}
	caps := make([]string, 0, len(served))
	for k := range served {
		caps = append(caps, k)
	}
	sort.Strings(caps)
	for _, cp := range caps {
		if len(served[cp]) > 1 && !known[cp].Aggregate {
			if _, ok := primary[cp]; !ok {
				a.Conflicts = append(a.Conflicts, Conflict{Capability: cp, Modules: served[cp]})
			}
		}
	}
	return a
}

func others(l []string, self string) []string {
	var out []string
	for _, x := range l {
		if x != self {
			out = append(out, x)
		}
	}
	return out
}

// providersOf lists unselected deployable catalogue modules that serve (or, with served false,
// only plan) a capability.
func (c *Catalogue) providersOf(capability string, selected map[string]bool, served bool) []string {
	var out []string
	for _, e := range c.Entries {
		if selected[e.ID] || e.Manifest == nil || !e.Deployable() {
			continue
		}
		for _, p := range e.Manifest.Provides {
			if p.Capability == capability && config.ProvidesServed(p.Status) == served {
				out = append(out, e.ID)
				break
			}
		}
	}
	return out
}

func sortedSelected(selected map[string]bool, c *Catalogue) []string {
	var ids []string
	for id, on := range selected {
		if on && c.Get(id) != nil && c.Get(id).Manifest != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
