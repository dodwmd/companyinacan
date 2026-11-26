package companyexport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/dodwmd/companyinacan/core/exportfmt"
)

// What of can-core's state database an export holds. Every column of every table is named here
// (TestCoreExportClassifiesEveryColumn).

type table = exportfmt.Table

const (
	whySecret   = "credential, key material or sign-in state"
	whyPlumbing = "delivery or job state of the platform, not company data"
)

func plain(name, desc, columns string) table {
	return table{Name: name, Description: desc, Columns: columns}
}

func skip(name, why string) table { return table{Name: name, Skip: why} }

var coreTables = []table{
	{Name: "admin_users", Description: "Accounts that sign in to the admin UI", Columns: "id username created_at auth_source disabled_at email email_set_by",
		Omitted: map[string]string{"password_hash": whySecret, "totp_secret": whySecret, "totp_last_step": whySecret}},
	plain("sso_identities", "Single sign-on identities", "id issuer_hash subject user_id email name groups_json amr_json created_at last_login linked_by email_verified"),
	plain("rbac_assignments", "Role assignments", "subject role scope granted_by granted_at"),
	plain("rbac_overrides", "Changes to the roles' permissions", "role permission effect set_by set_at"),
	{Name: "modules", Description: "Installed modules and their manifests", Columns: "id version namespace service_account state manifest_json updated_at",
		Omitted: map[string]string{"hmac_secret": whySecret, "settings_hash": whyPlumbing, "last_error": whyPlumbing, "content_json": whyPlumbing,
			"pending_json": whyPlumbing, "approved_upgrade": whyPlumbing, "applied_json": whyPlumbing}},
	plain("capability_providers", "Which module provides what", "capability module_id service port schema"),
	plain("primary_providers", "Primary provider per capability", "capability module_id"),
	plain("audit_log", "The hash-chained audit log of the control plane", "seq time actor action target detail prev_hash hash"),
	plain("approval_policies", "Approval policies", "type policy_json updated_at"),
	{Name: "approval_requests", Description: "Approval requests", Columns: "id module_id type requester ref title amount status outcome_reason policy_json created_at expires_at escalate_at escalated decided_at",
		Omitted: map[string]string{"payload_json": "copy of module data, which is exported with the module", "idem_key": whyPlumbing, "request_hash": whyPlumbing}},
	plain("approval_steps", "Steps of requests", "request_id idx stage name quorum status approvers"),
	plain("approval_decisions", "Decisions", "id request_id step_idx actor on_behalf_of kind comment at"),
	plain("approval_delegations", "Delegations", "id delegator delegate from_ts to_ts revoked"),
	plain("notify_prefs", "Notification choices", "subject type channel enabled"),
	plain("notify_settings", "Notification settings", "subject email quiet_start quiet_end tz digest"),
	{Name: "notify_channels", Description: "Outbound webhook channels", Columns: "name", Omitted: map[string]string{"url": whySecret}},
	plain("company_changes", "Changes to company.yaml made in the admin UI", "id actor created_at base_version patch_json acks_json summary state applied_at run_id"),
	plain("site_drafts", "Website changes previewed in the admin UI", "id actor created_at kind base_hash site_json state"),
	plain("site_revisions", "Published versions of the website's content", "id created_at actor kind hash site_json"),
	{Name: "api_tokens", Description: "Personal access tokens issued (not the tokens)", Columns: "id name module_id subject permissions created_by created_at expires_at revoked_at",
		Omitted: map[string]string{"token_hash": whySecret}},
	plain("offboarded_parties", "People whose access was ended", "party_id subject_ref effective_date recorded_at"),
	skip("sessions", whySecret), skip("login_attempts", whySecret), skip("login_tickets", whySecret), skip("ui_launch_sessions", whySecret),
	skip("user_invites", whySecret), skip("push_subscriptions", whySecret), skip("push_vapid", whySecret), skip("module_pending_secrets", whySecret),
	skip("secret_rotations", whyPlumbing), skip("subscriptions", whyPlumbing), skip("deliveries", whyPlumbing), skip("delivery_attempts", whyPlumbing),
	skip("events", "event payloads may name what has since been erased; the modules' records are authoritative"),
	skip("event_dedupe", whyPlumbing), skip("lead_links", whyPlumbing), skip("apply_runs", whyPlumbing), skip("reconcile_status", whyPlumbing),
	skip("wizard_state", whyPlumbing), skip("notifications", "transient messages; they may name what has since been erased"),
	skip("notify_deliveries", whyPlumbing), skip("email_outbox", "mail in transit, blanked once sent"),
	skip("deleted_users", "erasure bookkeeping; it names deleted accounts"), skip("schema_migrations", "database schema version"),
}

// writeCore writes company.yaml, the published site and can-core's tables.
func (s *Service) writeCore(ctx context.Context, a *exportfmt.Archive) (warnings []string, err error) {
	if s.CompanyFile != "" {
		b, err := os.ReadFile(s.CompanyFile)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			warnings = append(warnings, "no company.yaml has been applied to this server")
		case err != nil:
			return nil, fmt.Errorf("company.yaml: %w", err)
		default:
			clean, err := redactYAML(b)
			if err != nil {
				warnings = append(warnings, "company.yaml could not be parsed, so it was left out")
			} else if err := a.Bytes("company/company.yaml", clean); err != nil {
				return nil, err
			}
		}
	}
	if s.Site != nil {
		b, err := s.Site(ctx)
		if err != nil {
			warnings = append(warnings, "the published website is not in this export: "+err.Error())
		} else if err := a.Bytes("site/published.tar.gz", b); err != nil {
			return nil, err
		}
	}
	db, err := s.stateDB()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	// One read transaction: both passes over every table see the same rows.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	_, w, err := exportfmt.Database(ctx, a, tx, coreTables, exportfmt.Options{Dir: "company"})
	return append(warnings, w...), err
}

var yamlSecretKey = regexp.MustCompile(`(?i)(password|passwd|token|secret|api_?key|private_?key|credentials?)$`)

// redactYAML blanks scalar values of keys named like a credential. company.yaml is meant to hold
// none; this is the second line.
func redactYAML(b []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	changed := false
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if yamlSecretKey.MatchString(strings.TrimSpace(k.Value)) && v.Kind == yaml.ScalarNode && v.Value != "" && v.Tag == "!!str" {
					v.Value, v.Style, changed = "REDACTED", 0, true
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	if !changed {
		return b, nil
	}
	return yaml.Marshal(&doc)
}
