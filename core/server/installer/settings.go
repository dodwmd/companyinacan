package installer

import (
	"fmt"
	"os"

	"github.com/dodwmd/companyinacan/core/modsettings"
	"github.com/dodwmd/companyinacan/core/server/reconcile"
)

// settingValues are the chart values the module's settings.schema.json maps company.yaml's settings
// and company facts to (specs/064-module-settings). What is unknown or invalid is left out, so the
// module runs with its default; `can validate` and the admin UI say so.
func (in *Installer) settingValues(s reconcile.Spec) (map[string]any, error) {
	if s.Manifest.SettingsSchema == "" {
		return nil, nil
	}
	dir, err := in.moduleDir(s.ID)
	if err != nil {
		return nil, err
	}
	schema, err := modsettings.LoadModule(os.DirFS(dir), s.Manifest.SettingsSchema)
	if err != nil {
		// A module of the operator's own with an unusable schema installs as it did before.
		if s.Manifest.Provider.Type != "inhouse" {
			in.cfg.Log.Warn("no setting is mapped: the settings schema is unusable", "module", s.ID, "error", err.Error())
			return nil, nil
		}
		return nil, fmt.Errorf("module %s: %w", s.ID, err)
	}
	return schema.Map(s.Settings, modsettings.FactsOf(s.CompanyName, s.Organisation)).Nested(), nil
}
