package modmgr

import (
	"os"
	"path/filepath"
	"testing"
)

func manifest(id, name, extra string) string {
	return "schema: 1\nid: " + id + "\nname: " + name + "\ndescription: " + id + " module\nversion: 1.0.0\n" + extra
}

// writeModules builds a modules directory: web (ready, with a settings schema), crm (experimental,
// source-available, Postgres), desk (not deployable), alt (a second provider of `leads`), plan
// (provides only a planned capability), a hostile one and an example directory that must be skipped.
func writeModules(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	w := func(id, yaml string, files map[string]string) {
		d := filepath.Join(dir, id)
		must(t, os.MkdirAll(filepath.Join(d, "chart"), 0o755))
		must(t, os.WriteFile(filepath.Join(d, "module.yaml"), []byte(yaml), 0o644))
		for n, c := range files {
			must(t, os.WriteFile(filepath.Join(d, n), []byte(c), 0o644))
		}
	}
	w("web", manifest("web", "Web forms", `provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: forms}
requires: {database: sqlite, capabilities: [email.send]}
resources: {ram_mb: 20, ram_limit_mb: 40, disk_mb: 100}
provides:
  - {capability: leads, schema: leads.v1, service: api, port: 8081}
settings_schema: ./settings.schema.json
`), map[string]string{"settings.schema.json": webSchema})
	w("crm", manifest("crm", "CRM", `status: experimental
provider: {type: oss-wrapper, name: Upstream CRM, licence: BUSL-1.1, licence_class: source-available, upstream: "https://example.com/crm"}
runtime: {chart: ./chart, subdomain: crm}
requires: {database: postgres, capabilities: [leads]}
resources: {ram_mb: 300, ram_limit_mb: 450, disk_mb: 500}
provides:
  - {capability: contacts, schema: contacts.v1, service: api, port: 8081}
`), nil)
	w("desk", manifest("desk", "Help desk", `status: not-deployable
provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: help}
requires: {database: mariadb, capabilities: [email.send]}
resources: {ram_mb: 250}
provides:
  - {capability: tickets, schema: tickets.v1, service: api, port: 8081}
`), nil)
	w("alt", manifest("alt", "Alt forms", `provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: alt}
requires: {database: sqlite}
resources: {ram_mb: 10}
provides:
  - {capability: leads, schema: leads.v1, service: api, port: 8081}
`), nil)
	w("solo", manifest("solo", "Solo", `provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: solo}
requires: {database: sqlite}
resources: {ram_mb: 10}
`), nil)
	w("plan", manifest("plan", "Planner", `status: experimental
provider: {type: inhouse, name: can}
runtime: {chart: ./chart, subdomain: plan}
requires: {database: sqlite, capabilities: [invoices]}
resources: {ram_mb: 10}
provides:
  - {capability: invoices, schema: invoices.v1, service: api, port: 8081, status: planned}
`), nil)
	w("evil", manifest("evil", `"<script>alert(1)</script>"`, `provider: {type: inhouse, name: "<img src=x onerror=alert(2)>"}
runtime: {chart: ./chart, subdomain: evil}
requires: {database: sqlite}
resources: {ram_mb: 5}
`), nil)
	must(t, os.MkdirAll(filepath.Join(dir, "_example"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "_example", "module.yaml"), []byte(manifest("example", "Example", "")), 0o644))
	return dir
}

const webSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["site_title"],
  "properties": {
    "site_title": {"type": "string", "minLength": 1, "maxLength": 20, "description": "Shown in the page header."},
    "max_rows": {"type": "integer", "minimum": 1, "maximum": 100, "default": 10},
    "verbose": {"type": "boolean", "default": false},
    "captcha": {"enum": ["none", "turnstile"], "default": "none"},
    "api_key_secret": {"type": "string", "format": "secret-ref", "description": "Name of the Secret holding the API key."},
    "origins": {"type": "array", "items": {"type": "string"}}
  }
}`

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
