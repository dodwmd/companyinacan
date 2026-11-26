package adminui

// Messages of the business overview (specs/053-business-overview). The figure labels are keyed
// ov.fig_<module key with underscores>; a test checks every key of summary.v1 has one.
func init() {
	for k, v := range map[string]string{
		"nav.overview":         "Overview",
		"ov.title":             "Business overview",
		"ov.help":              "Totals only, and only what your roles let you see in each module. Figures can be up to 30 seconds old. Open a module to see the records behind them.",
		"ov.attention":         "Needs attention",
		"ov.nothing_attention": "Nothing needs your attention in what you can see.",
		"ov.incomplete":        "Some areas are unavailable, so this list may be incomplete.",
		"ov.item":              "Item",
		"ov.area":              "Area",
		"ov.value":             "Number",
		"ov.open":              "Details",
		"ov.open_in":           "Open %s",
		"ov.badge":             "needs attention",
		"ov.unavailable":       "%s is unavailable: its figures could not be read. Reload in a moment, or check its status.",
		"ov.failed":            "The module figures could not be collected.",
		"ov.as_of":             "As of",
		"ov.approvals":         "Approvals waiting for you",
		"ov.backup":            "Backups",
		"ov.backup_last":       "Last database dump (the off-site copy is not checked here)",
		"ov.backup_status":     "Status",
		"ov.backup_none":       "No dump has run yet",
		"ov.backup_ok":         "Succeeded at %s",
		"ov.backup_failed":     "FAILED at %s",
		"ov.backup_stale":      "Overdue: the last one was at %s",
		"ov.backup_skipped":    "Ran at %s without: %s",

		"ov.fig_leads_new":                    "New leads",
		"ov.fig_invoices_overdue":             "Overdue invoices",
		"ov.fig_invoices_outstanding":         "Receivables (unpaid invoices)",
		"ov.fig_contracts_awaiting_signature": "Contracts awaiting signature",
		"ov.fig_contracts_expiring":           "Contracts ending or renewing within 60 days",
		"ov.fig_work_urgent_overdue":          "Overdue P0 and P1 issues",
		"ov.fig_work_urgent_open":             "Open P0 and P1 issues",
		"ov.fig_people_headcount":             "Headcount you can see",
		"ov.fig_people_off_today":             "Off today, among those whose leave you can see",
		"ov.fig_requisitions_open":            "Open requisitions",
		"ov.fig_applications_new":             "New applications, not yet moved on",
		"ov.fig_interviews_due":               "Your interviews awaiting your scorecard",
		"ov.fig_grants_pending_approval":      "Grants waiting for approval",
		"ov.fig_vesting_upcoming":             "Grants with vesting due within 30 days",
		"ov.fig_storage_company":              "Storage used by all files",
		"ov.fig_storage_managed":              "Storage used in the spaces you manage",
		"ov.fig_files_retention_ending":       "Files whose retention date ends within 30 days",
		"ov.fig_filings_overdue":              "Overdue filings",
		"ov.fig_filings_upcoming":             "Filings due within 30 days",
	} {
		messagesEN[k] = v
	}
}
