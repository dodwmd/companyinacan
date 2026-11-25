package config

// Company is the typed view of company.yaml (schema company.v1).
type Company struct {
	Schema   int                    `yaml:"schema"`
	Name     string                 `yaml:"name"`
	Domain   string                 `yaml:"domain"`
	Provider Provider               `yaml:"provider"`
	Modules  map[string]ModuleEntry `yaml:"modules"`
	Primary  map[string]string      `yaml:"primary"`
	Site     Site                   `yaml:"site"`
}

// Provider selects where the company runs.
type Provider struct {
	Type    string         `yaml:"type"`
	Options map[string]any `yaml:"options"`
}

// ModuleEntry enables and configures one module in a company.
type ModuleEntry struct {
	Enabled  *bool          `yaml:"enabled"`
	Settings map[string]any `yaml:"settings"`
}

// IsEnabled reports whether the module is enabled (default true).
func (m ModuleEntry) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// DecodeCompany decodes a loaded document into the typed struct.
func DecodeCompany(d *Doc) (*Company, error) {
	var c Company
	if err := d.Root.Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}
