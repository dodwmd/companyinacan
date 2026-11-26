package validate

import (
	"strings"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modsettings"
)

// settingWarnings says which module settings and company facts reach nothing
// (specs/064-module-settings). All warnings: releases before it accepted any mapping, so a
// company.yaml must not start to be refused.
func settingWarnings(c *config.Company, enabled []string, manifests map[string]*config.Module, modulesDir string, add func(rule, path, msg string), warning func()) {
	warn := func(rule, path, msg string) {
		add(rule, path, msg)
		warning()
	}
	facts := modsettings.FactsOf(c.Name, c.Organisation)
	if tz := c.Organisation.Timezone; tz != "" && !modsettings.ValidTimezone(tz) {
		warn("semantic.organisation", "/organisation/timezone", "not an IANA time zone this system knows: modules run with their default")
	}
	for _, id := range enabled {
		mp, ok := ManifestPath(modulesDir, id)
		if !ok {
			continue
		}
		settings := c.Modules[id].Settings
		base := "/modules/" + id + "/settings"
		rel := manifests[id].SettingsSchema
		if rel == "" {
			continue // a module of the operator's own may read its settings itself
		}
		module, err := ModuleFS(mp)
		var schema *modsettings.Schema
		if err == nil {
			schema, err = modsettings.LoadModule(module, rel)
		}
		if err != nil {
			if len(settings) > 0 {
				warn("semantic.setting-unknown", base, "module "+id+": its settings schema is unusable ("+err.Error()+"), so nothing is known to apply the settings")
			}
			continue
		}
		for _, is := range schema.Map(settings, facts).Issues {
			path := base
			if len(is.Path) > 0 {
				path += "/" + strings.Join(is.Path, "/")
			}
			msg := "modules." + id + ".settings." + is.Name() + ": " + is.Message
			if is.Kind == modsettings.BadFact {
				path, msg = "/organisation", "module "+id+": "+is.Message
			}
			warn("semantic.setting-"+is.Kind, path, msg)
		}
	}
}
