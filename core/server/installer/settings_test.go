package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/config"
	"github.com/dodwmd/companyinacan/core/modsettings"
)

// A value, different from the default, for every setting an in-house module maps. A property added
// to a schema without one here fails TestEveryMappedSettingReachesTheModule.
var sampleSettings = map[string]string{
	"board":  `{retention: {board-minutes: legal, board-resolutions: legal, board-audit: legal, board-packs: 8y, board-private: 9y}}`,
	"entity": `{retention: {entity-register: legal, entity-filings: legal, entity-audit: legal, entity-person: 10y}}`,
	"equity": `{retention: {equity-ledger: legal, equity-audit: legal, equity-stakeholder: 8y, equity-grants: 9y, equity-valuation: legal}}`,
	"files":  `{max_file_mb: 100, quota_gb_per_owner: 20, trash_days: 90, scanner_required: false, inline_images: true, share: {max_ttl_hours: 48}, ui: {idle_minutes: 10, absolute_hours: 4}}`,
	"finance": `{base_currency: AUD, payment_instructions: "BSB 000-000\nAccount 1234", seller: {name: Acme Pty Ltd, address: "1 Anvil Way\nBrisbane", tax_id: ABN 12 345}, public_base_url: "https://billing.acme.example",
	  payment_provider: fake, esign_provider: fake, retention_years: 10, audit_retention_days: 0, trust_proxy: true, max_body_bytes: 2097152, rate_limit_per_minute: 0, auth_failures_per_minute: 5, signing_record_ip: none, ui: {idle_minutes: 10, absolute_hours: 4}}`,
	"forms": `{trust_proxy: false, max_body_bytes: 1048576, rate_limit_per_minute: 3, retention_days: 90, cors_allowed_origins: ["https://app.acme.example"], notify_email: [sales@acme.example]}`,
	"jobs": `{approval_required: true, collect_eeo: true, retention: {rejected_days: 30, talent_pool_days: 60, hired_days: 0, eeo_days: 90, closed_job_days: 45}, max_resume_mb: 25, scanner_required: false,
	  apply_per_hour: 3, default_valid_days: 30, consent_version: v2, consent_text: "I agree that Acme keeps my application.", eeo_min_cell: 10, ui: {idle_minutes: 10, absolute_hours: 4}}`,
	"people":       `{retention: {directory-basic: 8y, people-sensitive: 9y, people-checklists: 1y, performance-oneonone: 6m, performance-goals: 3y, performance-reviews: legal, people-timeoff: "0"}, reviews: {min_responses: 5}, ui: {idle_minutes: 10, absolute_hours: 4}}`,
	"provisioning": `{dry_run: false, privileged_groups: ["Domain Admins"], delete_after_days: 30, timezone: Australia/Brisbane, leaver_cutoff: end_of_day, compensate_with_delete: true, tasks: {joiner_template: Joiner, leaver_template: "Leaver: IT"}}`,
	"work":         `{trust_proxy: true, max_body_bytes: 2097152, rate_limit_per_minute: 10, auth_failures_per_minute: 0, retention_days: 0, audit_retention_days: 400}`,
}

func settingsOf(t *testing.T, src string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("%v in %s", err, src)
	}
	return m
}

func moduleSchema(t *testing.T, id string) *modsettings.Schema {
	t.Helper()
	s, err := modsettings.LoadModule(os.DirFS("../../../modules/"+id), "settings.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// renderConfig installs module id with settings and facts and returns its rendered configuration.
func renderConfig(t *testing.T, m inhouse, settings map[string]any, org config.Organisation) (map[string]any, []byte) {
	t.Helper()
	in, api, _, spec := fixture(t, nil)
	seedPeople(t, api)
	in.cfg.ModulesDir, in.cfg.Domain = "../../../modules", "acme.example"
	spec.ID, spec.Namespace, spec.ServiceAccount = m.id, "mod-"+m.id, m.id
	spec.Manifest = loadManifest(t, "../../../modules/"+m.id+"/module.yaml")
	spec.Manifest.Requires.Database, spec.Links = "none", testLinks(m.id)
	if sub, ok := inhouseSubdomains[m.id]; ok {
		spec.Manifest.Runtime.Subdomain = sub
	}
	spec.Settings, spec.Organisation, spec.CompanyName = settings, org, "Acme Co"
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatalf("%s: %v", m.id, err)
	}
	raw, _ := dig(api.Objects["ConfigMap/mod-"+m.id+"/"+m.id+"-config"], "data", m.id+".yaml").(string)
	if err := m.parseConf([]byte(raw)); err != nil {
		t.Fatalf("%s: the module's own loader rejects the configuration: %v\n%s", m.id, err, raw)
	}
	var conf map[string]any
	if err := yaml.Unmarshal([]byte(raw), &conf); err != nil {
		t.Fatal(err)
	}
	return conf, []byte(raw)
}

func at(m map[string]any, path string) (any, bool) {
	var v any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = mm[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestEveryMappedSettingReachesTheModule(t *testing.T) {
	for _, m := range inhouseModules() {
		t.Run(m.id, func(t *testing.T) {
			settings := settingsOf(t, sampleSettings[m.id])
			schema := moduleSchema(t, m.id)
			r := schema.Map(settings, modsettings.Facts{})
			for _, is := range r.Issues {
				t.Errorf("sample %s: %s", is.Name(), is.Message)
			}
			for _, p := range schema.Properties() {
				if tg := p.Annotation.Target(); tg != "" {
					if _, ok := r.Values[tg]; !ok {
						t.Errorf("no sample value reaches %s (%s)", tg, p.Name())
					}
				}
			}
			conf, raw := renderConfig(t, m, settings, config.Organisation{})
			for tg, want := range r.Values {
				got, ok := at(conf, strings.TrimPrefix(tg, "config."))
				if !ok || js(got) != js(want) {
					t.Errorf("%s: rendered %s, want %s\n%s", tg, js(got), js(want), raw)
				}
			}
		})
	}
}

func inhouseByID(t *testing.T, id string) inhouse {
	t.Helper()
	for _, m := range inhouseModules() {
		if m.id == id {
			return m
		}
	}
	t.Fatalf("no module %s", id)
	return inhouse{}
}

func TestCompanyFactsReachFinanceAndProvisioning(t *testing.T) {
	org := config.Organisation{LegalName: "Acme Pty Ltd", Address: "1 Anvil Way\nBrisbane", TaxID: "ABN 1", Currency: "AUD", Timezone: "Australia/Brisbane"}
	conf, raw := renderConfig(t, inhouseByID(t, "finance"), nil, org)
	for k, want := range map[string]string{"seller.name": org.LegalName, "seller.address": org.Address, "seller.tax_id": org.TaxID, "base_currency": "AUD"} {
		if got, _ := at(conf, k); got != want {
			t.Errorf("%s = %v, want %q\n%s", k, got, want, raw)
		}
	}
	// Without organisation the seller is the company's name, never the web address.
	conf, _ = renderConfig(t, inhouseByID(t, "finance"), nil, config.Organisation{})
	if got, _ := at(conf, "seller.name"); got != "Acme Co" {
		t.Errorf("seller.name = %v", got)
	}
	// A module setting wins over the fact.
	conf, _ = renderConfig(t, inhouseByID(t, "finance"), map[string]any{"base_currency": "EUR", "seller": map[string]any{"name": "Other"}}, org)
	if a, _ := at(conf, "base_currency"); a != "EUR" {
		t.Errorf("base_currency = %v", a)
	}
	if a, _ := at(conf, "seller.name"); a != "Other" {
		t.Errorf("seller.name = %v", a)
	}
	conf, _ = renderConfig(t, inhouseByID(t, "provisioning"), nil, org)
	if got, _ := at(conf, "workflows.timezone"); got != org.Timezone {
		t.Errorf("timezone = %v", got)
	}
}

// Text from company.yaml stays one value of the configuration, whatever it holds.
func TestHostileSettingTextStaysAValue(t *testing.T) {
	for i, s := range []string{
		"line\n---\nlisten: \":1\"", "x\nseller: {name: evil}", "  leading spaces\nthen", "$(CAN_FINANCE_ADMIN_TOKEN) ${X}", "{{ .Values.secrets }}",
		"\"quoted\" 'single' # not a comment: [a]", "tab\there\r\ncrlf", "sep para nel\u0085end", "<no value>", "null", "true", "0x1F", "- a\n- b", "trailing\n\n",
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			conf, raw := renderConfig(t, inhouseByID(t, "finance"), map[string]any{"payment_instructions": s}, config.Organisation{Address: "a"})
			got, _ := at(conf, "payment_instructions")
			if s == "<no value>" {
				return // the renderer removes this one string, as Helm does
			}
			if got != s {
				t.Errorf("got %q, want %q\n%s", got, s, raw)
			}
			if l, _ := at(conf, "listen"); l != ":8080" {
				t.Errorf("listen changed: %v\n%s", l, raw)
			}
		})
	}
}

func TestInvalidAndUnknownSettingsDoNotReachTheModule(t *testing.T) {
	settings := settingsOf(t, `{base_currency: ZZZ, listen: ":1", db_path: /tmp/x, api_token: hunter2hunter2, webhook_url: "https://x.example", retention_years: -1, seller: {name: "a\nb"}, audit_retention_days: 10, ui: {enabled: true}}`)
	conf, raw := renderConfig(t, inhouseByID(t, "finance"), settings, config.Organisation{})
	for k, want := range map[string]any{"base_currency": "USD", "listen": ":8080", "db_path": "/data/finance.db", "retention_years": 7, "audit_retention_days": 2555, "seller.name": "Acme Co"} {
		if got, _ := at(conf, k); js(got) != js(want) {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}
	for _, s := range []string{"hunter2", "x.example", "enabled: true"} {
		if strings.Contains(string(raw), s) {
			t.Errorf("%q reached the configuration:\n%s", s, raw)
		}
	}
}

// Kubernetes expands references in environment values: a setting that reaches one is escaped.
func TestWrappedSettingsAreNotExpanded(t *testing.T) {
	in, api, _, spec := fixture(t, nil)
	in.cfg.ModulesDir, in.cfg.Domain = "../../../modules", "acme.example"
	spec.ID, spec.Namespace, spec.ServiceAccount, spec.Domain = "analytics", "mod-analytics", "analytics", "acme.example"
	spec.Manifest = loadManifest(t, "../../../modules/analytics/module.yaml")
	spec.Settings = map[string]any{"admin_email": "$(CAN_ANALYTICS_ADMIN_PASSWORD)@acme.example"}
	if err := in.Enable(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	got := js(api.Objects["Deployment/mod-analytics/analytics"])
	if !strings.Contains(got, `"$$(CAN_ANALYTICS_ADMIN_PASSWORD)@acme.example"`) {
		t.Fatalf("not escaped: %s", got)
	}
}
