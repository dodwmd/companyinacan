package reconcile

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
)

// Pending is an upgrade held until the operator approves what it widens.
type Pending struct {
	Approval string   `json:"approval"`
	Widens   []string `json:"widens"`
}

// ParsePending reads a recorded store.Module.Pending; ok is false when nothing is held.
func ParsePending(s string) (Pending, bool) {
	var p Pending
	if s == "" || json.Unmarshal([]byte(s), &p) != nil || p.Approval == "" {
		return Pending{}, false
	}
	return p, true
}

// approvalOf identifies a set of widenings: an approval covers exactly what was shown.
func approvalOf(widens []string) string { return digest([]byte(strings.Join(widens, "\n")))[:16] }

// Widening lists what manifest next may do, reach or expose that the recorded manifest could not,
// in words an operator can approve. Removals and other changes are not listed. Charts cannot widen
// egress by themselves: their NetworkPolicies may only join pods of their own namespace.
func Widening(recorded, next *config.Module) []string {
	have := grants(recorded)
	var out []string
	for g := range grants(next) {
		if !have[g] {
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

func grants(m *config.Module) map[string]bool {
	out := map[string]bool{}
	add := func(parts ...string) { out[strings.Join(parts, " ")] = true }
	for _, p := range m.Permissions {
		add("new permission", p)
	}
	for role, ps := range m.Roles {
		for _, p := range ps {
			add("role", role, "gains", p)
		}
	}
	for _, p := range m.Publishes {
		add("may publish event", p.Event)
	}
	for _, s := range m.Subscribes {
		if config.ProvidesServed(s.Status) {
			add("receives event", s.Event)
		}
	}
	for _, p := range m.Provides {
		if config.ProvidesServed(p.Status) {
			add("serves capability", p.Capability)
		}
	}
	for _, l := range [][]string{m.Requires.Capabilities, m.Consumes} {
		for _, c := range l {
			add("may use capability", c)
		}
	}
	if db := m.Requires.Database; db == "postgres" || db == "mariadb" {
		add("gets a", db, "database")
	}
	r := m.Runtime
	if r.Subdomain != "" {
		add("public host", r.Subdomain+".<domain>")
	}
	if r.SiteRoute != nil {
		add("public path on the site", r.SiteRoute.Path)
	}
	if r.PublicLink != nil {
		add("public signed links under", r.PublicLink.Prefix)
	}
	if r.PublicSign != nil {
		add("public signing pages under", r.PublicSign.Prefix)
	}
	if r.PublicUI != nil {
		for _, p := range r.PublicUI.Exact {
			add("web UI path when exposed", p)
		}
		for _, p := range r.PublicUI.Prefixes {
			add("web UI paths when exposed under", p)
		}
	}
	// Read by name, so that it is seen whether or not this build's manifest type declares it.
	var rt map[string]any
	b, _ := json.Marshal(r)
	_ = json.Unmarshal(b, &rt)
	if on, _ := rt["internet_egress"].(bool); on {
		add("internet access")
	}
	return out
}
