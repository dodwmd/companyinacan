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
		// WebLogin declares that the module's web UI accepts can-core launch tickets at
		// <UI base>/login/ticket (specs/036-module-ui-sso); nil means it does not and it is never
		// offered in the Apps launcher.
		WebLogin *WebLogin `yaml:"web_login"`
		// PublicUI says what can-core routes when company.yaml sets `expose.ui` or `expose.api`
		// (specs/040-module-routing).
		PublicUI *PublicUI `yaml:"public_ui"`
		// SiteRoute is a path on the public site's hosts (forms: /_forms).
		SiteRoute *SiteRoute `yaml:"site_route"`
		// PublicLink publishes one path of signed links (specs/045-public-access).
		PublicLink *PublicLink `yaml:"public_link"`
		// PublicConfig is merged into the chart's `config:` block while the chart's public pages
		// are routed.
		PublicConfig map[string]any `yaml:"public_config"`
		// RoutedUIHost is set by can-core in its recorded copy of the manifest when it routes the
		// web UI; never read from module.yaml. The Apps launcher derives the address from it.
		RoutedUIHost string `yaml:"-"`
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
		Image string `yaml:"image"`
		// Command is the binary in the module's own image whose subcommands the hooks are
		// (in-house modules, specs/043-module-backup-hooks); Backup and Restore are then
		// subcommand names, run without a shell.
		Command string `yaml:"command"`
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

// WebLogin is module.yaml `runtime.web_login`.
type WebLogin struct {
	// BasePath is where the UI lives on its host ("" for the root, "/ui" for work).
	BasePath string `yaml:"base_path"`
	// ViaSubdomain says that https://<runtime.subdomain>.<company domain> serves the UI. Without it
	// the operator has to give the address (CAN_MODULE_UI_URLS).
	ViaSubdomain bool `yaml:"via_subdomain"`
}

// PublicUI is module.yaml `runtime.public_ui`.
type PublicUI struct {
	Service string `yaml:"service"`
	Port    int    `yaml:"port"`
	// Exact paths and path prefixes of the web UI: nothing else of the service is routed.
	Exact    []string `yaml:"exact"`
	Prefixes []string `yaml:"prefixes"`
	// Config is merged into the chart's `config:` block while the UI is routed.
	Config map[string]any `yaml:"config"`
}

// SiteRoute is module.yaml `runtime.site_route`: Path on the site's hosts, stripped before it
// reaches the service.
type SiteRoute struct {
	Path    string `yaml:"path"`
	Service string `yaml:"service"`
	Port    int    `yaml:"port"`
}

// PublicLink is module.yaml `runtime.public_link`: signed links anyone holding one may open. Only
// Prefix is routed, on https://<id>.<domain>, and only with `expose.public: true`.
type PublicLink struct {
	Service string `yaml:"service"`
	Port    int    `yaml:"port"`
	Prefix  string `yaml:"prefix"`
	// Config is merged into the chart's `config:` block while the link is routed; BaseURLKey is
	// the key of that block that takes the public address.
	Config     map[string]any `yaml:"config"`
	BaseURLKey string         `yaml:"base_url_key"`
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
