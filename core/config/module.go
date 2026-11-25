package config

// Module is the typed view of module.yaml (schema module.v1).
type Module struct {
	Schema      int    `yaml:"schema"`
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Category    string `yaml:"category"`
	Status      string `yaml:"status"` // ready (default), experimental, not-deployable
	Version     string `yaml:"version"`
	Provider    struct {
		Type            string `yaml:"type"`
		Name            string `yaml:"name"`
		Licence         string `yaml:"licence"`
		LicenceSelected string `yaml:"licence_selected"`
		LicenceClass    string `yaml:"licence_class"`
		Upstream        string `yaml:"upstream"`
		UpstreamVersion string `yaml:"upstream_version"`
	} `yaml:"provider"`
	Runtime struct {
		Chart     string `yaml:"chart"`
		Values    string `yaml:"values"`
		Subdomain string `yaml:"subdomain"`
		// UI marks a module that serves a web UI for people (specs/036-module-ui-sso): can-core
		// offers it in the Apps launcher at https://<subdomain>.<domain><base_path>.
		UI struct {
			Service  string `yaml:"service"`
			Port     int    `yaml:"port"`
			BasePath string `yaml:"base_path"`
		} `yaml:"ui"`
	} `yaml:"runtime"`
	Requires struct {
		Database     string   `yaml:"database"`
		Capabilities []string `yaml:"capabilities"`
		SSO          string   `yaml:"sso"`
	} `yaml:"requires"`
	Resources struct {
		RAMMB      int `yaml:"ram_mb"`
		RAMLimitMB int `yaml:"ram_limit_mb"`
		DiskMB     int `yaml:"disk_mb"`
	} `yaml:"resources"`
	Provides []struct {
		Capability string `yaml:"capability"`
		Schema     string `yaml:"schema"`
		Service    string `yaml:"service"`
		Port       int    `yaml:"port"`
		Status     string `yaml:"status"` // available (default) or planned
	} `yaml:"provides"`
	Consumes  []string `yaml:"consumes"`
	Publishes []struct {
		Event  string `yaml:"event"`
		Schema string `yaml:"schema"`
	} `yaml:"publishes"`
	Subscribes []struct {
		Event   string `yaml:"event"`
		Service string `yaml:"service"`
		Port    int    `yaml:"port"`
		Webhook string `yaml:"webhook"`
		Status  string `yaml:"status"` // available (default) or planned
	} `yaml:"subscribes"`
	// Hooks are the lifecycle scripts the module ships (schema module.v1 hooks).
	Hooks struct {
		Image   string `yaml:"image"`
		Install string `yaml:"install"`
		Backup  string `yaml:"backup"`
		Restore string `yaml:"restore"`
		Purge   string `yaml:"purge"`
		Export  string `yaml:"export"`
		Import  string `yaml:"import"`
	} `yaml:"hooks"`
	Permissions []string `yaml:"permissions"`
	// SettingsSchema is the relative path of the module's settings JSON Schema (rendered by the
	// admin UI, epic E12b).
	SettingsSchema string `yaml:"settings_schema"`
	// Approvals and Notifications are the approval types and notification rules (epic E11a).
	Approvals     []ManifestApproval  `yaml:"approvals"`
	Notifications []NotifyRule        `yaml:"notifications"`
	Roles         map[string][]string `yaml:"roles"`
	DataClasses   []struct {
		Class       string `yaml:"class"`
		Retention   string `yaml:"retention"`
		Sensitivity string `yaml:"sensitivity"`
	} `yaml:"data_classes"`
}

// DecodeModule decodes a loaded document into the typed struct.
func DecodeModule(d *Doc) (*Module, error) {
	var m Module
	if err := d.Root.Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ProvidesServed reports whether a provision is actually served (status is not "planned").
func ProvidesServed(status string) bool { return status != "planned" }
