package reconcile

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
)

// Pending is an upgrade held until the operator approves what it widens.
type Pending struct {
	Approval string   `json:"approval"`
	Widens   []string `json:"widens"`
	// Notes is Action's.
	Notes []string `json:"notes,omitempty"`
	// NotApplied and Urgent are Action's.
	NotApplied []string `json:"not_applied,omitempty"`
	Urgent     bool     `json:"urgent,omitempty"`
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
// egress by themselves: their NetworkPolicies may only join pods of their own namespace. Internet
// access is judged from what was installed (newInternetAccess), not from the recorded manifest.
func Widening(recorded, next *config.Module) []string {
	have := grants(recorded)
	var out []string
	for g := range grants(next) {
		if !have[g] {
			out = append(out, g)
		}
	}
	out = append(out, weakenedApprovals(recorded, next)...)
	sort.Strings(out)
	return out
}

// weakenedApprovals lists approvals that need fewer people than before.
func weakenedApprovals(recorded, next *config.Module) []string {
	chains := map[string][]string{}
	for _, a := range next.Approvals {
		chains[a.Type] = a.DefaultChain
	}
	var out []string
	for _, a := range recorded.Approvals {
		chain, declared := chains[a.Type]
		if !declared {
			out = append(out, "no longer declares the approval "+a.Type)
			continue
		}
		left := slices.Clone(chain)
		for _, step := range a.DefaultChain {
			if i := slices.Index(left, step); i >= 0 {
				left = slices.Delete(left, i, i+1)
			} else {
				out = append(out, "approval "+a.Type+" no longer needs "+step)
			}
		}
	}
	return out
}

func imageRepository(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
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
			if s.Via == config.ViaCanCore && s.Event == "lead.created.v1" {
				add("is sent the names, addresses and messages of website leads by can-core")
			}
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
	for _, n := range m.Notifications {
		for _, to := range n.Recipients {
			add("notifies", to, "about", n.Event)
		}
	}
	h := m.Hooks
	if h.Command != "" {
		add("hooks run the command", h.Command)
	}
	if h.Image != "" {
		add("hooks run in the image", imageRepository(h.Image)) // a new tag of the same image is an update
	}
	for kind, v := range map[string]string{"install": h.Install, "backup": h.Backup, "restore": h.Restore, "purge": h.Purge, "export": h.Export, "import": h.Import} {
		if v != "" {
			add("runs the", kind, "hook", v)
		}
	}
	if w := r.WebLogin; w != nil {
		add("signs users into its web UI at", "/"+strings.TrimPrefix(w.BasePath, "/"))
	}
	if sso := m.Requires.SSO; sso != "" && sso != "none" {
		add("uses single sign-on:", sso)
	}
	for _, c := range m.DataClasses {
		add("keeps data of class", c.Class, "("+c.Sensitivity+") for", c.Retention)
	}
	return out
}
