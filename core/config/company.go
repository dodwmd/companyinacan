package config

// Company is the typed view of company.yaml (schema company.v1).
type Company struct {
	Schema   int                    `yaml:"schema"`
	Name     string                 `yaml:"name"`
	Domain   string                 `yaml:"domain"`
	Provider Provider               `yaml:"provider"`
	DNS      map[string]any         `yaml:"dns"`
	Modules  map[string]ModuleEntry `yaml:"modules"`
	Primary  map[string]string      `yaml:"primary"`
	Site     Site                   `yaml:"site"`
	// Approvals are the approval policies by type; Notifications the delivery settings (epic E11a).
	Approvals     map[string]ApprovalPolicy `yaml:"approvals"`
	Notifications NotificationConfig        `yaml:"notifications"`
	// SSO is OpenID Connect sign-in for the admin UI (epic E11b); nil = local login only.
	SSO *SSO `yaml:"sso"`
	// Email is the outbound relay and the per-module sending rules (epic E03d); nil = nothing set here.
	Email *Email `yaml:"email"`
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
