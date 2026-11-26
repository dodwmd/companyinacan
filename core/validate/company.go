package validate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/provider"
	"github.com/dodwmd/companyinacan/core/routing"
	"github.com/dodwmd/companyinacan/core/schema"
)

// CompanyFile validates company.yaml and the manifests of its enabled modules,
// including cross-module rules. All problems are returned together.
func CompanyFile(file, modulesDir string) []problem.Problem {
	doc, ps := config.Load(file)
	if doc == nil {
		return ps
	}
	return append(ps, CompanyDoc(doc, modulesDir)...)
}

// CompanyBytes validates company.yaml content that is not (yet) a file, such as an edit made in the
// admin UI, with exactly the checks of CompanyFile.
func CompanyBytes(name string, data []byte, modulesDir string) []problem.Problem {
	doc, ps := config.LoadBytes(name, data)
	if doc == nil {
		return ps
	}
	return append(ps, CompanyDoc(doc, modulesDir)...)
}

// CompanyDoc validates a loaded company document (see CompanyFile).
func CompanyDoc(doc *config.Doc, modulesDir string) []problem.Problem {
	var ps []problem.Problem
	ps = append(ps, schema.Validate(schema.Company, doc)...)
	c, err := config.DecodeCompany(doc)
	if err != nil {
		problem.Sort(ps)
		return ps
	}
	add := func(rule, path, msg string) {
		l, col := doc.Pos(path)
		ps = append(ps, problem.Problem{File: doc.File, Line: l, Column: col, Path: path, Rule: rule, Severity: problem.Error, Message: msg})
	}

	for _, is := range append(provider.CheckProviderOptions(c.Provider.Type, c.Provider.Options), provider.CheckDNS(c.Domain, c.DNS)...) {
		add(is.Rule, is.Path, is.Message)
	}

	for _, is := range config.ValidateSSO(c.SSO, c.Domain) {
		add(is.Rule, is.Path, is.Message)
	}

	for _, is := range config.ValidateEmail(c.Email) {
		add(is.Rule, is.Path, is.Message)
	}

	_, support := config.SupportMailOf(c)
	for _, is := range support {
		msg := is.Message
		if is.Invalid {
			msg += " (no mailbox setting is applied until this is fixed)"
		}
		add(is.Rule, is.Path, msg)
		ps[len(ps)-1].Severity = problem.Warning
	}

	_, leads := config.LeadIntakeOf(c)
	for _, is := range leads {
		add("semantic.lead-intake", is.Path, is.Message)
		ps[len(ps)-1].Severity = problem.Warning
	}

	if _, warning := config.SiteSupportAddress(c); warning != "" {
		add("semantic.support-section-off", "/site/support", warning)
		ps[len(ps)-1].Severity = problem.Warning
	}

	if c.Site.Contact != nil {
		if forms, err := config.FormsOfModules(modulesDir); err != nil {
			add("semantic.contact-form", "/site/contact", err.Error())
		} else {
			for _, is := range config.CheckContact(c, forms, config.AbsentUnknown).Issues {
				add(is.Rule, is.Path, is.Message)
				if is.Warning {
					ps[len(ps)-1].Severity = problem.Warning
				}
			}
		}
	}

	ids := make([]string, 0, len(c.Modules))
	for id := range c.Modules {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	manifests := map[string]*config.Module{}
	for _, id := range ids {
		if !c.Modules[id].IsEnabled() {
			continue
		}
		mp, ok := ManifestPath(modulesDir, id)
		if !ok {
			msg := fmt.Sprintf("no manifest for module %q under %s", id, modulesDir)
			if modulesDir == BuiltinModules {
				msg = fmt.Sprintf("no manifest for module %q: this can ships no module of that name, and no modules directory was found (--modules-dir)", id)
			}
			add("semantic.module-manifest-missing", "/modules/"+id, msg)
			continue
		}
		ps = append(ps, ModuleFile(mp)...)
		if md, ps2 := LoadManifest(mp); md != nil && len(ps2) == 0 {
			if m, err := config.DecodeModule(md); err == nil {
				manifests[id] = m
				if m.Status == "not-deployable" {
					// Enabling goes through this validation (`can validate`, `can apply` and the
					// reconciler's LoadDesired), so refusing here covers every enable path.
					add("semantic.module-not-deployable", "/modules/"+id,
						fmt.Sprintf("module %q is not-deployable (known not to run as shipped, see its README) and cannot be enabled; remove it from company.yaml", id))
				}
			}
		}
	}

	enabled := make([]string, 0, len(manifests))
	for id := range manifests {
		enabled = append(enabled, id)
	}
	sort.Strings(enabled)

	// routing: only what the manifest offers can be exposed (hosts are checked at install and admission)
	for _, id := range enabled {
		m, x := manifests[id], c.Modules[id].Exposure()
		if (x.UI || x.API) && m.Runtime.PublicUI == nil {
			add("semantic.module-not-exposable", "/modules/"+id+"/expose", fmt.Sprintf("module %q has no web UI that can-core can route (no runtime.public_ui in its manifest)", id))
		}
		if p := m.Runtime.PublicUI; p != nil {
			if err := routing.CheckUIPaths(p.Exact, p.Prefixes); err != nil {
				add("semantic.module-ui-paths", "/modules/"+id, fmt.Sprintf("module %q: runtime.public_ui: %v", id, err))
			}
		}
		if l := m.Runtime.PublicLink; l != nil && routing.LinkPrefixes[id] != l.Prefix {
			add("semantic.module-public-link", "/modules/"+id, fmt.Sprintf("module %q may not publish %s", id, l.Prefix))
		}
		if x.Signing && m.Runtime.PublicSign == nil {
			add("semantic.module-not-exposable", "/modules/"+id+"/expose", fmt.Sprintf("module %q has no signing pages (no runtime.public_sign in its manifest)", id))
		}
		if l := m.Runtime.PublicSign; l != nil && routing.SignPrefixes[id] != l.Prefix {
			add("semantic.module-public-sign", "/modules/"+id, fmt.Sprintf("module %q may not publish %s", id, l.Prefix))
		}
		if r := m.Runtime.SiteRoute; r != nil && routing.SitePrefixes[id] != r.Path {
			add("semantic.module-site-route", "/modules/"+id, fmt.Sprintf("module %q may not claim %s on the public site", id, r.Path))
		}
	}

	if _, ok := manifests["equity"]; ok {
		settings := c.Modules["equity"].Settings
		_, people := manifests["people"]
		st, err := config.ReadEquitySettings(settings)
		if err != nil {
			add("semantic.equity-settings", "/modules/equity/settings", "modules.equity.settings."+err.Error())
		}
		if _, err := config.ParseBreakGlass("modules.equity.settings."+config.SettingBreakGlass, st.BreakGlassUntil, time.Now()); err != nil {
			add("semantic.equity-break-glass", "/modules/equity/settings/"+config.SettingBreakGlass, err.Error())
		}
		primary := c.Primary["directory"]
		switch {
		case st.Identity == config.IdentityDirectory && (!people || (primary != "" && primary != "people")):
			// equity never falls back by itself: turning people off means saying so for equity too
			add("semantic.equity-directory", "/modules/equity/settings/"+config.SettingIdentity,
				"equity is set to identity: directory, which needs the people module enabled and providing the directory: enable people, or set modules.equity.settings.identity: attestation")
		case st.Identity == config.IdentityAttestation && err == nil && people:
			add("semantic.equity-identity", "/modules/equity",
				"equity runs in attestation mode although the people module is enabled: consider modules.equity.settings.identity: directory (read docs/equity.md first: acting users need linked records)")
			ps[len(ps)-1].Severity = problem.Warning
		}
	}

	settingWarnings(c, enabled, manifests, modulesDir, add, func() { ps[len(ps)-1].Severity = problem.Warning })

	// unique subdomains
	owner := map[string]string{}
	for _, id := range enabled {
		sd := manifests[id].Runtime.Subdomain
		if sd == "" {
			continue
		}
		if first, dup := owner[sd]; dup {
			add("semantic.duplicate-subdomain", "/modules/"+id, fmt.Sprintf("subdomain %q is already used by module %q", sd, first))
			continue
		}
		owner[sd] = id
	}

	// exclusive capabilities need a designated primary
	providers := map[string][]string{}
	for _, id := range enabled {
		for _, p := range manifests[id].Provides {
			if !config.ProvidesServed(p.Status) {
				continue // planned: no adapter serves it yet
			}
			providers[p.Capability] = append(providers[p.Capability], id)
		}
	}
	caps := make([]string, 0, len(providers))
	for k := range providers {
		caps = append(caps, k)
	}
	sort.Strings(caps)
	known := Capabilities()
	for _, cap := range caps {
		if len(providers[cap]) > 1 && !known[cap].Aggregate {
			if _, ok := c.Primary[cap]; !ok {
				add("semantic.exclusive-capability", "/modules",
					fmt.Sprintf("modules %s all provide the exclusive capability %q; designate one under primary", strings.Join(providers[cap], ", "), cap))
			}
		}
	}
	pcaps := make([]string, 0, len(c.Primary))
	for k := range c.Primary {
		pcaps = append(pcaps, k)
	}
	sort.Strings(pcaps)
	for _, cap := range pcaps {
		id := c.Primary[cap]
		ok := false
		for _, p := range providers[cap] {
			if p == id {
				ok = true
			}
		}
		if !ok {
			add("semantic.primary-invalid", "/primary/"+cap, fmt.Sprintf("primary %q for %q must be an enabled module that provides it", id, cap))
		}
	}
	problem.Sort(ps)
	return ps
}
