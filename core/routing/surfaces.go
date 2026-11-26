package routing

import (
	"path"
	"sort"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
)

// What a module puts on the internet with its exposure settings (specs/045-public-access). The DNS
// records `can` lists and creates, the addresses the admin UI shows and what can-core routes all
// start from this.

// Kinds of public surface.
const (
	KindUI    = "ui"           // the web UI on <id>.<domain>
	KindAPI   = "api"          // /v1 on <id>.<domain>
	KindPages = "pages"        // the chart's anonymous pages on <subdomain>.<domain>
	KindLink  = "invoice-link" // signed links on <id>.<domain>, one path
	KindForms = "forms"        // a path on the site's own hosts
)

// LinkPrefixes are the only signed-link paths a module may route (runtime.public_link).
var LinkPrefixes = map[string]string{"finance": "/public/invoices/"}

// pagePaths is where a module's anonymous pages start, for addresses people are shown.
var pagePaths = map[string]string{"jobs": "/careers"}

// Surface is one public address of a module.
type Surface struct {
	Module string `json:"module"`
	Kind   string `json:"kind"`
	Host   string `json:"host"`
	URL    string `json:"url"`
}

// LinkWanted reports whether company.yaml publishes the module's signed links. Unlike other public
// surfaces this is off unless `expose.public: true` is written: upgrading must not put a new path
// on the internet.
func LinkWanted(m *config.Module, x config.Expose) bool {
	return m != nil && m.Runtime.PublicLink != nil && x.Public != nil && *x.Public
}

// LoginPath is the web UI's sign-in path (the edge limits it).
func LoginPath(m *config.Module) string {
	base := ""
	if m.Runtime.WebLogin != nil {
		base = m.Runtime.WebLogin.BasePath
	}
	return path.Join("/", base, "login")
}

// Surfaces lists the public addresses of module id.
func Surfaces(domain, id string, m *config.Module, x config.Expose) []Surface {
	if domain == "" || m == nil {
		return nil
	}
	var out []Surface
	add := func(kind, host, p string) {
		out = append(out, Surface{Module: id, Kind: kind, Host: host, URL: "https://" + host + p})
	}
	if m.Runtime.PublicUI != nil && MayClaim(id, id) {
		if x.UI {
			p := ""
			if w := m.Runtime.WebLogin; w != nil {
				p = w.BasePath
			}
			add(KindUI, id+"."+domain, p)
		}
		if x.API {
			add(KindAPI, id+"."+domain, "/v1/")
		}
	}
	if sd := m.Runtime.Subdomain; sd != "" && x.PublicOn() && MayClaim(id, sd) {
		add(KindPages, sd+"."+domain, pagePaths[id])
	}
	if LinkWanted(m, x) && LinkPrefixes[id] == m.Runtime.PublicLink.Prefix && MayClaim(id, id) {
		add(KindLink, id+"."+domain, m.Runtime.PublicLink.Prefix)
	}
	if r := m.Runtime.SiteRoute; r != nil && x.PublicOn() && SitePrefixes[id] == r.Path {
		add(KindForms, domain, r.Path)
	}
	return out
}

// CompanySurfaces lists the public addresses of every enabled module of company.yaml. manifest
// returns nil for a module whose manifest is not known; those are returned in unknown.
func CompanySurfaces(c *config.Company, manifest func(id string) *config.Module) (out []Surface, unknown []string) {
	ids := make([]string, 0, len(c.Modules))
	for id, e := range c.Modules {
		if e.IsEnabled() {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		m := manifest(id)
		if m == nil {
			unknown = append(unknown, id)
			continue
		}
		out = append(out, Surfaces(c.Domain, id, m, c.Modules[id].Exposure())...)
	}
	return out, unknown
}

// ModuleHost is a hostname below the company domain that needs a DNS record.
type ModuleHost struct {
	Host, Purpose string
}

// DNSHosts are the hostnames besides the domain itself that the surfaces need, each once.
func DNSHosts(domain string, surfaces []Surface) []ModuleHost {
	purposes := map[string][]string{}
	var order []string
	for _, s := range surfaces {
		if s.Host == domain {
			continue
		}
		if _, ok := purposes[s.Host]; !ok {
			order = append(order, s.Host)
		}
		purposes[s.Host] = append(purposes[s.Host], s.Module+" "+s.Kind)
	}
	out := make([]ModuleHost, 0, len(order))
	for _, h := range order {
		out = append(out, ModuleHost{Host: h, Purpose: strings.Join(purposes[h], ", ")})
	}
	return out
}
