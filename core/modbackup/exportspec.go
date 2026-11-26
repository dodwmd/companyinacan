package modbackup

import "github.com/dodwmd/companyinacan/core/exportfmt"

// What `<binary> export` writes, table by table (specs/070-company-export). Every column of every
// table is named here: exported (Columns), encrypted at rest (Sealed) or never exported
// (Omitted); a table is left out as a whole with Skip. TestExportClassifiesEveryColumn fails for
// anything a migration adds and nobody classified.

type (
	table  = exportfmt.Table
	sealed = map[string]exportfmt.Sealed
	omit   = map[string]string
)

// Reasons for leaving things out.
const (
	whySecret   = "credential or key material"
	whyPlumbing = "delivery state of the service, not company data"
	whyReplay   = "request replay cache of the API, not company data"
	whyErasure  = "erasure bookkeeping; it names what was erased"
	whyMigr     = "database schema version"
)

// common are the tables every in-house service has in the same shape.
func common(extra ...table) []table {
	return append([]table{
		{Name: "schema_migrations", Skip: whyMigr},
	}, extra...)
}

func skip(name, why string) table { return table{Name: name, Skip: why} }

func plain(name, desc, columns string) table {
	return table{Name: name, Description: desc, Columns: columns}
}

// bound seals a column whose AAD is prefix + the row's id column + suffix.
func bound(row, prefix, suffix string) exportfmt.Sealed {
	return exportfmt.Sealed{Row: row, Prefix: prefix, Suffix: suffix}
}

// tableAAD is the "table:id:column" binding of can-jobs and can-files.
func tableAAD(tbl, row, col string) exportfmt.Sealed {
	return bound(row, tbl+":", ":"+col)
}

// idAAD is the "id:field" binding of can-equity, can-board and the directory of can-people.
func idAAD(row, field string) exportfmt.Sealed { return bound(row, "", ":"+field) }

var exportTables = map[string][]table{
	"can-forms": common(
		plain("leads", "Form submissions", "seq id form_id status email name fields note created_at updated_at retention_until consents"),
		table{Name: "lead_deliveries", Description: "Notification attempts per submission", Columns: "id lead_id kind ok attempts at",
			Omitted: omit{"last_error": "may quote a notification URL that carries a token"}},
		plain("lead_audit", "Audit log: who did what to submissions", "id at actor action target count"),
		skip("lead_email", whyPlumbing), skip("erasures", whyErasure), skip("erasure_notices", whyErasure),
	),
	"can-work": common(
		plain("projects", "Projects and their workflows", "id key name description workflow sla next_number archived created_at updated_at"),
		plain("project_members", "Project membership", "project_id subject added_by added_at"),
		plain("sprints", "Sprints", "id project_id name goal state starts_on ends_on created_at updated_at"),
		plain("issues", "Issues", "id project_id number type title description priority status reporter assignee due_date estimate parent_id sprint_id rank version created_at updated_at resolved_at external_id priority_since"),
		plain("issue_labels", "Labels of issues", "issue_id label"),
		plain("issue_links", "Links between issues", "id type source_id target_id created_by created_at"),
		plain("comments", "Comments on issues", "id issue_id author body created_at"),
		plain("activity", "History of issues", "id issue_id actor kind data at"),
		plain("watchers", "Watchers of issues", "issue_id subject"),
		plain("attachments", "Attachment references (links; the service stores no bytes)", "id issue_id filename content_type size_bytes sha256 url added_by created_at"),
		plain("saved_filters", "Saved searches", "id owner name project query sort shared created_at"),
		plain("audit_log", "Audit log", "id actor action target detail at"),
		skip("api_tokens", whySecret), skip("idempotency", whyReplay), skip("outbox", whyPlumbing), skip("erasures", whyErasure),
	),
	"can-people": common(
		plain("departments", "Departments", "id name created_at"),
		plain("teams", "Teams", "id name department_id lead_id created_at"),
		plain("positions", "Positions", "id title department_id created_at"),
		table{Name: "people", Description: "Directory records, one per person; sens_* are the restricted fields",
			Columns: "seq id kind status sso_subject given_name family_name preferred_name work_email work_phone location department_id team_id position_id manager_id start_date end_date legal_hold legal_hold_reason ended_at erased_at created_at updated_at hire_ref external_id",
			Sealed: sealed{"sens_dob": idAAD("t.id", "date_of_birth"), "sens_national_id": idAAD("t.id", "national_id"),
				"sens_address": idAAD("t.id", "home_address"), "sens_phone": idAAD("t.id", "personal_phone"), "sens_salary": idAAD("t.id", "salary")}},
		table{Name: "status_history", Description: "Employment status changes",
			Columns: "seq person_id from_status to_status effective_date applied recorded_at recorded_by",
			Sealed:  sealed{"note": idAAD("t.person_id", "status_note")}},
		plain("checklist_templates", "Onboarding and offboarding templates", "id name kind auto_status items created_at"),
		plain("checklists", "Checklists of people", "id person_id template_id kind anchor_date created_at created_by"),
		plain("checklist_tasks", "Tasks of checklists", "id checklist_id person_id ord title assignee_role assignee_id due_date action done_at done_by"),
		plain("oneonone_series", "1:1 series", "id manager_id report_id cadence created_at"),
		table{Name: "oneonone_meetings", Description: "1:1 meetings and their notes", Columns: "id series_id scheduled_at status created_at updated_at",
			Sealed: sealed{"shared_notes_sealed": idAAD("t.id", "shared_notes"), "private_notes_sealed": idAAD("t.id", "private_notes")}},
		plain("agenda_items", "1:1 agenda items", "id meeting_id author_id text created_at"),
		plain("action_items", "1:1 action items", "id meeting_id text assignee_id due_date status work_issue_id created_by created_at"),
		plain("goals", "Goals", "id scope owner_id team_id title description period visibility status parent_id created_by created_at updated_at"),
		plain("key_results", "Key results", "id goal_id title unit start_value target_value current_value created_at"),
		plain("goal_updates", "Progress updates", "id goal_id kr_id author_id value comment created_at"),
		plain("review_templates", "Review question templates", "id name questions created_by created_at"),
		plain("review_cycles", "Review cycles", "id name state template_id questions anonymity min_responses rating_max due_date created_by opened_at calibration_at closed_at created_at"),
		table{Name: "review_participants", Description: "One review per person and cycle",
			Columns: "id cycle_id person_id self_submitted_at mgr_author mgr_submitted_at calibrated_by release_state release_requested_by release_decided_by approval_ref released_at ack_at touched_by created_at updated_at",
			Sealed: sealed{"self_sealed": bound("t.id", "review:", ":self"), "mgr_sealed": bound("t.id", "review:", ":mgr"),
				"calib_sealed": bound("t.id", "review:", ":calib"), "final_sealed": bound("t.id", "review:", ":final"),
				"ack_sealed": bound("t.id", "review:", ":ack"), "release_note_sealed": bound("t.id", "review:", ":relnote")}},
		plain("review_requests", "Peer and upward feedback requests", "id participant_id reviewer_id kind status proposed_by decided_by created_at"),
		table{Name: "review_responses", Description: "Submitted feedback. Anonymous feedback is exported only where the cycle's minimum number of responses was reached",
			Columns: "id participant_id kind reviewer_id", Sealed: sealed{"answers_sealed": bound("t.id", "review_response:", "")},
			// The service never shows fewer anonymous answers than the cycle's minimum; neither does the export.
			Where: `t.reviewer_id IS NOT NULL OR (SELECT COUNT(*) FROM review_responses r WHERE r.participant_id = t.participant_id AND r.kind = t.kind) >= ` +
				`(SELECT c.min_responses FROM review_participants p JOIN review_cycles c ON c.id = p.cycle_id WHERE p.id = t.participant_id)`},
		plain("leave_types", "Leave types", "id key name paid sensitive accrual_mode grant_halves carryover_cap_halves allow_negative negative_floor_halves ignores_blackouts sets_leave_status leave_status_min_halves active created_at"),
		plain("holiday_calendars", "Holiday calendars", "id location name work_days created_at"),
		plain("holidays", "Holidays", "calendar_id date name"),
		plain("blackouts", "Leave blackout periods", "id name start_date end_date team_id created_by created_at"),
		table{Name: "leave_requests", Description: "Leave requests",
			Columns: "id person_id type_id status start_date end_date start_part end_part start_slot end_slot halves use_slots approval_ref submitted_by decided_by decided_at cancelled_by cancelled_at status_applied created_at updated_at submitted_account",
			Sealed:  sealed{"note_sealed": bound("t.id", "leave:", ":note")}},
		table{Name: "leave_ledger", Description: "Leave balance ledger (half days)", Columns: "seq person_id type_id entry_date kind halves ref created_by created_at",
			Sealed: sealed{"note_sealed": bound("t.ref", "leave_ledger:", ":note")}},
		plain("audit_log", "Hash-chained audit log", "seq at actor action resource fields outcome prev hash"),
		skip("ledger_guard", whyPlumbing), skip("outbox", whyPlumbing), skip("erasures", whyErasure),
	),
	"can-jobs": common(
		plain("stages", "Pipeline stages", "key name kind position"),
		plain("jobs", "Job openings", "id slug title department city region country postal_code remote applicant_countries employment_type salary_min salary_max salary_currency salary_period publish_salary sections hiring_manager headcount status approval approval_requested_by approval_ref valid_through opened_at closed_at close_reason created_by created_at updated_at version"),
		table{Name: "syndication", Description: "Job board syndication", Columns: "job_id board external_ref status updated_at",
			Omitted: omit{"last_error": "may quote a connector URL that carries a token"}},
		table{Name: "candidates", Description: "Candidates", Columns: "id name tags source consent_at consent_version talent_pool talent_consent_at retain_until legal_hold erased_at created_at updated_at",
			Sealed: sealed{"email_sealed": tableAAD("candidates", "t.id", "email_sealed"), "phone_sealed": tableAAD("candidates", "t.id", "phone_sealed"),
				"address_sealed": tableAAD("candidates", "t.id", "address_sealed"), "salary_exp_sealed": tableAAD("candidates", "t.id", "salary_exp_sealed"),
				"notes_sealed": tableAAD("candidates", "t.id", "notes_sealed")},
			Omitted: omit{"email_hash": "keyed lookup hash of the address"}},
		table{Name: "applications", Description: "Applications; resumes are under files/resumes/<application id>",
			Columns: "id candidate_id job_id stage status reject_reason source resume_name resume_type resume_size resume_sha256 resume_scan created_at updated_at decided_at",
			Sealed: sealed{"reject_note_sealed": tableAAD("applications", "t.id", "reject_note_sealed"),
				"cover_letter_sealed": tableAAD("applications", "t.id", "cover_letter_sealed")},
			Omitted: omit{"resume_key": "internal storage name"}},
		table{Name: "stage_history", Description: "Pipeline moves", Columns: "seq application_id from_stage to_stage status actor at",
			Sealed: sealed{"note_sealed": tableAAD("stage_history", "t.seq", "note_sealed")}},
		plain("interviewers", "Interviewer assignments", "application_id subject assigned_by at removed_at"),
		table{Name: "scorecards", Description: "Interview scorecards", Columns: "id application_id author rating recommendation status submitted_at created_at updated_at",
			Sealed: sealed{"comments_sealed": tableAAD("scorecards", "t.id", "comments_sealed")}},
		plain("audit_log", "Hash-chained audit log", "seq at actor action resource fields outcome prev hash"),
		skip("eeo_counts", "diversity answers are only published as aggregates with small cells suppressed (the jobs report)"),
		skip("api_tokens", whySecret), skip("outbox", whyPlumbing), skip("erasures", whyErasure),
	),
	"can-finance": common(
		plain("parties", "Customers and vendors", "id kind name contact_name email phone address tax_id currency notes erased_at version created_by created_at updated_at external_id"),
		plain("tax_rates", "Tax rates", "id code name rate_ppm active created_at"),
		plain("products", "Products", "id sku name description unit_price_minor currency tax_code active version created_at updated_at"),
		plain("series", "Document number series", "code next_number"),
		table{Name: "invoices", Description: "Invoices and credit notes",
			Columns: "id kind status series number number_text party_id party_snapshot project currency base_currency fx_rate_e8 issue_date due_date subtotal_minor discount_minor tax_minor total_minor base_total_minor paid_minor credited_minor original_id reason notes payment_instructions recurring_id recurring_period created_by updated_by approved_by approved_at sent_at paid_at voided_at void_reason version created_at updated_at",
			Omitted: omit{"link_epoch": "version of the signed customer links"}},
		plain("invoice_lines", "Invoice lines", "id invoice_id position description sku qty_milli unit_price_minor discount_bp tax_code tax_rate_ppm gross_minor discount_minor tax_minor total_minor"),
		plain("payments", "Payments received", "id invoice_id amount_minor currency method reference received_on source external_id created_by created_at"),
		plain("recurring", "Recurring invoices", "id party_id name currency fx_rate_e8 project series lines interval next_date end_date due_days notes status created_by version created_at updated_at"),
		plain("expenses", "Expenses and receipt metadata", "id owner description amount_minor currency category incurred_on project receipt_filename receipt_content_type receipt_size receipt_sha256 status decided_by decided_at reject_reason reimbursement_ref version created_by created_at updated_at"),
		plain("purchase_orders", "Purchase orders", "id vendor_id number number_text status currency project lines subtotal_minor tax_minor total_minor notes created_by updated_by approved_by approved_at version created_at updated_at"),
		plain("timesheets", "Timesheets", "id owner week_start status submitted_at decided_by decided_at reject_reason version created_by updated_by submitted_by created_at updated_at"),
		plain("time_entries", "Time entries", "id timesheet_id day project task minutes billable description invoice_id"),
		plain("contract_templates", "Contract templates", "id name body variables version created_by created_at updated_at"),
		plain("contracts", "Contracts", "id party_id template_id title status current_version start_date end_date renewal_date reminder_days reminded_for esign_provider esign_id sent_once signed_at signed_sha256 terminated_at terminate_reason created_by updated_by version created_at updated_at"),
		plain("contract_versions", "Contract texts", "id contract_id version body variables sha256 created_by created_at"),
		plain("signing_envelopes", "Signature requests", "id contract_id version sha256 title ordered link_days status sender created_at closed_at chain_head scrubbed"),
		table{Name: "signing_signers", Description: "Signers and what they signed",
			Columns: "id envelope_id position role name email status expires_at sends mail_status cert_status cert_sends typed_name consent_text ip signed_at declined_at decline_reason",
			Omitted: omit{"token_hash": whySecret}},
		plain("signing_events", "Signature evidence chain", "id envelope_id seq kind signer_id data at prev_hash hash"),
		plain("ledger_accounts", "Chart of accounts", "id code name type tax_code active used version created_by created_at"),
		plain("ledger_mappings", "Automatic posting accounts", "key account_id updated_by updated_at"),
		plain("journal_entries", "Journal entries", "id status source source_ref entry_date currency memo number number_text reversal_of reversed_by created_by posted_by posted_at created_at"),
		plain("journal_lines", "Journal lines", "id entry_id position account_id debit_minor credit_minor memo"),
		plain("ledger_closes", "Period closes", "id action closed_through reason by at"),
		plain("period_locks", "Period locks", "id through_date locked_by note at"),
		plain("payroll_imports", "Imported payroll files", "sha256 entry_ids imported_by imported_at"),
		plain("export_batches", "Accounting exports already handed over", "id adapter first_number last_number entry_count sha256 content created_by created_at"),
		plain("export_cursors", "Accounting export positions", "adapter last_number"),
		table{Name: "bank_accounts", Description: "Bank accounts", Columns: "id name currency ledger_account_id last4 active version created_by created_at",
			Sealed: sealed{"sealed_number": bound("t.id", "finance.bank_accounts.sealed_number:", "")}},
		plain("bank_statements", "Imported bank statements", "id bank_account_id format sha256 config_sha256 size_bytes txn_count new_count duplicate_count imported_by imported_at"),
		plain("bank_transactions", "Bank transactions", "id bank_account_id statement_id txn_key key_group booked_on amount_minor currency reference counterparty description status matched_payment_id decided_by decided_at ignore_reason created_at"),
		plain("bank_reconciliations", "Reconciliations", "id bank_account_id period txn_count matched_count ignored_count reconciled_by reconciled_at"),
		plain("core_approvals", "Approvals asked of can-core", "ref approval_id version attempt status requested_by created_at decided_at"),
		table{Name: "document_mails", Description: "Mails to customers", Columns: "id kind invoice_id party_id step status link_expires created_by created_at updated_at handed_at",
			Omitted: omit{"nonce": "part of a signed customer link", "core_id": whyPlumbing}},
		plain("dunning_policy", "Payment reminder policy", "id enabled days_after_due max_overdue_days updated_by updated_at restored_at"),
		plain("invoice_reminder_holds", "Invoices with reminders held", "invoice_id reason set_by set_at"),
		plain("party_reminder_stops", "Customers with reminders stopped", "party_id set_by set_at"),
		plain("overdue_notices", "Overdue notices already given", "invoice_id due_date notified_at"),
		plain("audit_log", "Audit log", "id actor action target detail at"),
		skip("api_tokens", whySecret), skip("idempotency", whyReplay), skip("outbox", whyPlumbing),
		skip("retention_gate", whyPlumbing), skip("erasures", whyErasure), skip("erased_external_ids", whyErasure),
	),
	"can-equity": common(
		plain("entities", "Companies in the cap table", "id name jurisdiction currency created_at"),
		plain("share_classes", "Share classes", "id entity_id name kind par_minor currency authorised created_at"),
		table{Name: "stakeholders", Description: "Holders",
			Columns: "id entity_id kind person_id legal_hold legal_hold_reason erased_at created_by created_at updated_at external_attested_by external_attested_on person_linked_by person_verified_by person_verified_on identity_by erased_by",
			Sealed:  sealed{"sens_name": idAAD("t.id", "name"), "sens_email": idAAD("t.id", "email"), "sens_tax_id": idAAD("t.id", "tax_id")}},
		plain("plans", "Equity plans", "id entity_id class_id name authorised adopted_on status created_at"),
		table{Name: "grants", Description: "Grants (quantities are 1/10000 of a share)",
			Columns: "id entity_id plan_id stakeholder_id type status version supersedes_id quantity currency strike_minor grant_date vest_start schedule expiry_date ptep_days created_by edited_by submitted_by approved_by approved_on approval_ref activated_on termination_date for_cause terminal_on terminal_reason exercised vested_notified created_at updated_at",
			Sealed:  sealed{"sens_notes": idAAD("t.id", "notes")}},
		plain("grant_milestones", "Vesting milestones reached", "id grant_id tranche_idx achieved_on recorded_by created_at"),
		plain("grant_accel_events", "Acceleration events", "id grant_id trigger occurred_on recorded_by created_at"),
		table{Name: "exercises", Description: "Option exercises",
			Columns: "id entity_id grant_id stakeholder_id quantity currency due_minor status requested_by approved_by paid_minor paid_on payment_by issued_by issued_on ledger_id approval_ref created_at updated_at",
			Sealed:  sealed{"sens_payment_ref": idAAD("t.id", "payment_ref")}},
		plain("ledger", "The equity ledger (append-only)", "seq id entity_id class_id stakeholder_id kind effective_date quantity price_minor currency group_id ref_grant_id ref_exercise_id recorded_by recorded_at"),
		table{Name: "convertibles", Description: "SAFEs and convertible notes", Columns: "id entity_id stakeholder_id kind issued_on status created_by created_at",
			Sealed: sealed{"sens_terms": idAAD("t.id", "terms")}},
		table{Name: "valuations", Description: "Valuations", Columns: "id entity_id class_id kind effective_date expires_on created_by created_at",
			Sealed: sealed{"sens_fmv": idAAD("t.id", "fmv"), "sens_notes": idAAD("t.id", "notes")}},
		plain("audit_log", "Hash-chained audit log", "seq at actor action resource fields outcome prev hash"),
		skip("idempotency", whyReplay), skip("outbox", whyPlumbing), skip("erasures", whyErasure),
	),
	"can-board": common(
		plain("bodies", "Boards and committees", "id name kind parent_id entity_ref quorum_num quorum_den quorum_min quorum_excludes_recused pass_num pass_den pass_inclusive notice_days notice_basis created_by created_at updated_at"),
		plain("body_members", "Terms of office", "id body_id person_id role start_date end_date created_by created_at ended_by"),
		plain("meetings", "Meetings", "id body_id title starts_on starts_time mode status notice_given_on notice_days_required short_notice_waiver_ref pack_issued_on created_by created_at updated_at"),
		plain("meeting_invitees", "Invitees", "meeting_id person_id capacity added_by"),
		plain("agenda_items", "Agenda items", "id meeting_id position title kind resolution_id created_by created_at"),
		plain("attendance", "Attendance", "meeting_id person_id status recorded_by recorded_at"),
		table{Name: "pack_items", Description: "Board pack references", Columns: "id meeting_id doc_ref media_type size_bytes sha256 position supplementary added_by added_at",
			Sealed: sealed{"sens_title": idAAD("t.id", "title"), "sens_note": idAAD("t.id", "note")}},
		plain("resolutions", "Resolutions and their results", "id body_id meeting_id kind secret_ballot status current_version proposer_ref proposer_person pass_num pass_den pass_inclusive consent_deadline external_type external_ref supersedes_id eligible_count quorum_required quorum_present result_for result_against result_abstain close_reason opened_by opened_at closed_by closed_on created_by created_at updated_at"),
		plain("resolution_subjects", "People a resolution is about", "resolution_id person_id"),
		table{Name: "resolution_versions", Description: "Resolution texts", Columns: "resolution_id version content_sha256 created_by created_at",
			Sealed: sealed{"sens_title": idAAD("t.resolution_id || '#' || t.version", "title"), "sens_text": idAAD("t.resolution_id || '#' || t.version", "text")}},
		table{Name: "conflicts", Description: "Declared interests", Columns: "id resolution_id person_id treatment declared_by declared_at",
			Sealed: sealed{"sens_nature": idAAD("t.id", "nature")}},
		plain("resolution_voters", "Who could vote", "resolution_id person_id"),
		plain("votes", "Open votes and written consents", "id resolution_id voter_person voter_ref choice method signature_ref cast_at"),
		table{Name: "ballots", Description: "Secret ballots of closed votes (no link to a voter)", Columns: "id resolution_id",
			Sealed: sealed{"sens_choice": idAAD("t.id", "choice")}},
		table{Name: "resolution_withdrawals", Description: "Withdrawals", Columns: "resolution_id withdrawn_by withdrawn_at",
			Sealed: sealed{"sens_reason": idAAD("t.resolution_id", "reason")}},
		table{Name: "minutes", Description: "Minutes", Columns: "id meeting_id version status content_sha256 author_ref author_person edited_by approved_by approved_on created_at updated_at",
			Sealed: sealed{"sens_content": idAAD("t.id", "content")}},
		table{Name: "action_items", Description: "Action items", Columns: "id meeting_id resolution_id assignee_person work_issue_ref due_on status created_by created_at updated_at",
			Sealed: sealed{"sens_text": idAAD("t.id", "text")}},
		plain("person_holds", "Legal holds", "person_id reason placed_by placed_at"),
		table{Name: "legal_entities", Description: "Legal entities", Columns: "id name jurisdiction entity_type status incorporated_on fiscal_year_end equity_entity_ref created_by created_at updated_at",
			Sealed: sealed{"sens_reg_number": idAAD("t.id", "reg_number"), "sens_tax_id": idAAD("t.id", "tax_id")}},
		table{Name: "entity_contacts", Description: "Registered agents and addresses", Columns: "id entity_id kind person_ref effective_from effective_to created_by created_at",
			Sealed: sealed{"sens_name": idAAD("t.id", "name"), "sens_address": idAAD("t.id", "address")}},
		table{Name: "register_entries", Description: "Statutory registers", Columns: "seq id entity_id register kind party_ref office title class_ref quantity effective_on resolution_ref corrects_id recorded_by recorded_at attested_by attested_on",
			Sealed: sealed{"sens_detail": idAAD("t.id", "detail")}},
		plain("filing_obligations", "Filing obligations", "id entity_id name authority first_due_on recurrence lead_days active created_by created_at"),
		plain("filings", "Filings", "id obligation_id entity_id due_on status filed_on filed_by doc_ref reminders_sent created_at"),
		table{Name: "entity_documents", Description: "Document references", Columns: "id entity_id kind doc_ref added_by added_at",
			Sealed: sealed{"sens_title": idAAD("t.id", "title")}},
		plain("share_class_links", "Links to equity share classes", "id entity_id class_ref created_by created_at"),
		plain("audit_log", "Hash-chained audit log", "seq at actor action resource fields outcome prev hash"),
		skip("ballot_staging", "an open secret ballot"), skip("receipt_staging", "an open secret ballot"),
		skip("vote_receipts", "pseudonymous proof of who voted in a secret ballot"),
		skip("idempotency", whyReplay), skip("outbox", whyPlumbing), skip("erasures", whyErasure),
	),
	"can-provision": common(
		plain("runs", "Provisioning runs", "id kind party_id state trigger idem_key urgent dry_run privileged effective_date initiator created_at updated_at wake_at examined_at"),
		plain("steps", "Steps of runs", "run_id ord op connector arg state sensitive optional comp_op attempts not_before next_at approval_id approved external_id error_code completed_at waits gen"),
		plain("accounts", "Downstream accounts", "party_id connector external_id state updated_at"),
		plain("applied_groups", "Groups applied downstream", "party_id connector grp"),
		plain("urgent", "Urgent terminations", "party_id flagged_by flagged_at"),
		plain("groups", "Groups", "id display_name version created_at updated_at"),
		plain("group_members", "Group membership", "group_id party_id"),
		plain("audit", "Hash-chained audit log", "seq time actor action target detail prev_hash hash"),
		skip("scim_tokens", whySecret), skip("seen_events", whyPlumbing), skip("outbox", whyPlumbing),
	),
	"can-files": common(
		table{Name: "spaces", Description: "Spaces", Columns: "id owner created_at", Sealed: sealed{"name_sealed": tableAAD("spaces", "t.id", "name_sealed")}},
		plain("space_members", "Space access", "space_id principal role added_by added_at"),
		table{Name: "folders", Description: "Folders", Columns: "id space_id parent_id depth created_by created_at",
			Sealed: sealed{"name_sealed": tableAAD("folders", "t.id", "name_sealed")}},
		table{Name: "files", Description: "Files; content is under files/<id>/v<version>", Where: "t.status != 'purging'",
			Columns: "id space_id folder_id ext subject ref status purge_reason purge_actor purge_action current_version rev legal_hold retain_until created_by created_at updated_at deleted_at erased_at",
			Sealed:  sealed{"name_sealed": tableAAD("files", "t.id", "name_sealed"), "description_sealed": tableAAD("files", "t.id", "description_sealed")}},
		table{Name: "versions", Description: "File versions", Where: "t.file_id IN (SELECT id FROM files WHERE status != 'purging')",
			Columns: "file_id n size sha256 content_type scan created_by created_at purged_at", Omitted: omit{"blob_key": "internal storage name"}},
		table{Name: "shares", Description: "Share links (the link itself is not stored)", Where: "t.file_id IN (SELECT id FROM files WHERE status != 'purging')",
			Columns: "id file_id version expires_at one_time uses created_by created_at revoked_at",
			Sealed:  sealed{"label_sealed": tableAAD("shares", "t.id", "label_sealed")}},
		plain("share_grants", "Why a share was allowed", "share_id principal"),
		plain("audit_log", "Hash-chained audit log", "seq at actor action resource fields outcome prev hash"),
		skip("pending_blobs", whyPlumbing), skip("outbox", whyPlumbing), skip("erasures", whyErasure),
	),
}

// exportFiles lists the files on the volume an export carries: (blob key, size, sha256, path in
// the archive without extension, extension source).
var exportFiles = map[string]string{
	"can-files": `SELECT v.blob_key, v.size, v.sha256, 'files/' || f.id || '/v' || v.n, f.ext
FROM versions v JOIN files f ON f.id = v.file_id
WHERE v.purged_at IS NULL AND v.blob_key != '' AND f.status IN ('active','deleted') ORDER BY f.id, v.n`,
	"can-jobs": `SELECT resume_key, resume_size, resume_sha256, 'files/resumes/' || id, resume_name
FROM applications WHERE resume_key != '' ORDER BY id`,
}
