package validate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/schema"
)

// ManifestPath resolves a module id to its manifest in modulesDir (id or _id directory).
func ManifestPath(modulesDir, id string) (string, bool) {
	for _, d := range []string{id, "_" + id} {
		p := filepath.Join(modulesDir, d, "module.yaml")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

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
			add("semantic.module-manifest-missing", "/modules/"+id, fmt.Sprintf("no manifest for module %q under %s", id, modulesDir))
			continue
		}
		ps = append(ps, ModuleFile(mp)...)
		if md, ps2 := config.Load(mp); md != nil && len(ps2) == 0 {
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
