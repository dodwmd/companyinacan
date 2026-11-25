package validate

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/problem"
	"github.com/dodwmd/companyinacan/core/schema"
)

// ModuleFile loads and validates one module.yaml: parse, schema, then semantic rules.
// All problems are returned together.
func ModuleFile(file string) []problem.Problem {
	doc, ps := config.Load(file)
	if doc == nil {
		return ps
	}
	ps = append(ps, schema.Validate(schema.Module, doc)...)
	if m, err := config.DecodeModule(doc); err == nil {
		ps = append(ps, moduleRules(doc, m)...)
	}
	problem.Sort(ps)
	return ps
}

func moduleRules(doc *config.Doc, m *config.Module) []problem.Problem {
	var ps []problem.Problem
	add := func(rule, path, msg string, sev problem.Severity) {
		l, c := doc.Pos(path)
		ps = append(ps, problem.Problem{File: doc.File, Line: l, Column: c, Path: path, Rule: rule, Severity: sev, Message: msg})
	}

	if m.Resources.RAMLimitMB != 0 && m.Resources.RAMMB != 0 && m.Resources.RAMLimitMB < m.Resources.RAMMB {
		add("semantic.ram-limit", "/resources/ram_limit_mb",
			fmt.Sprintf("ram_limit_mb (%d) must not be lower than ram_mb (%d)", m.Resources.RAMLimitMB, m.Resources.RAMMB), problem.Error)
	}

	declared := map[string]bool{}
	for _, p := range m.Permissions {
		declared[p] = true
	}
	roles := make([]string, 0, len(m.Roles))
	for r := range m.Roles {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	for _, r := range roles {
		for i, p := range m.Roles[r] {
			if !declared[p] {
				add("semantic.role-permission", fmt.Sprintf("/roles/%s/%d", r, i),
					fmt.Sprintf("role %q grants %q, which is not declared in permissions", r, p), problem.Error)
			}
		}
	}

	for _, v := range config.PlatformViolations(m) {
		add("semantic.platform-name", v.Path, v.Message, problem.Error)
	}

	for i, dc := range m.DataClasses {
		if dc.Sensitivity == "sensitive" && m.Provider.Type != "inhouse" {
			add("semantic.sensitive-requires-inhouse", fmt.Sprintf("/data_classes/%d/sensitivity", i),
				fmt.Sprintf("data class %q is sensitive; sensitive data may only be held by in-house modules (constitution V)", dc.Class), problem.Error)
		}
	}

	served := 0
	for _, p := range m.Provides {
		if config.ProvidesServed(p.Status) {
			served++ // a planned provision has no service behind it to receive the webhook
		}
	}
	for i, s := range m.Subscribes {
		if s.Service == "" && served == 0 {
			add("semantic.subscribe-service", fmt.Sprintf("/subscribes/%d/service", i),
				"a module that serves no capability (planned ones do not count) must name the service that receives the webhook", problem.Error)
		}
	}

	if why := m.BackupHookUnrunnable(); why != "" && m.Hooks.Image != "" {
		add("semantic.backup-hooks-image", "/hooks/backup",
			fmt.Sprintf("module %q has a backup hook that cannot run: %s. While it is enabled every dump run fails and NOTHING is backed up", m.ID, why), problem.Warning)
	}
	if m.Status == "experimental" || m.Status == "not-deployable" {
		add("semantic.module-status", "/status", fmt.Sprintf("module %q is %s: it is not ready to enable on a real server (see its README)", m.ID, m.Status), problem.Warning)
	}
	for i, p := range m.Provides {
		if !config.ProvidesServed(p.Status) {
			add("semantic.provides-adapter-disabled", fmt.Sprintf("/provides/%d/status", i),
				fmt.Sprintf("capability %q is planned: nothing serves it yet, so it is ignored for provider resolution and consumers must not rely on it", p.Capability), problem.Warning)
		}
	}

	known := Capabilities()
	warn := func(name, path string) {
		if _, ok := known[name]; !ok {
			add("semantic.unknown-capability", path, fmt.Sprintf("capability %q is not in schemas/names.yaml (typo?)", name), problem.Warning)
		}
	}
	for i, c := range m.Consumes {
		warn(c, fmt.Sprintf("/consumes/%d", i))
	}
	for i, c := range m.Requires.Capabilities {
		warn(c, fmt.Sprintf("/requires/capabilities/%d", i))
	}
	for i, p := range m.Provides {
		warn(p.Capability, fmt.Sprintf("/provides/%d/capability", i))
	}

	if filepath.Base(doc.File) == "module.yaml" && m.ID != "" {
		dir := strings.TrimPrefix(filepath.Base(filepath.Dir(doc.File)), "_")
		if dir != m.ID && filepath.Dir(doc.File) != "." {
			add("semantic.id-dir", "/id", fmt.Sprintf("module id %q must match its directory name %q", m.ID, dir), problem.Error)
		}
	}
	return ps
}
