package adminui

// Messages of the erasure of leads (specs/054-lead-erasure).
func init() {
	for k, v := range map[string]string{
		"flash.lead_erased": "Lead erased. E-mail already sent for it, and copies kept outside the forms module, are not recalled.",

		"le.title":         "Erasure requests",
		"le.link":          "Erasure request: find by email",
		"le.help":          "Find every submission that names an address, then erase the ones that are about that person. The address is not logged or stored by this search.",
		"le.email":         "Email address",
		"le.search":        "Search",
		"le.none":          "No submission names that address.",
		"le.truncated":     "There are more results than shown. Erase these, then search again.",
		"le.results":       "Submissions naming the address",
		"le.match":         "How it matches",
		"le.match_address": "A field is the address",
		"le.match_mention": "Only mentioned in a text: read it first",
		"le.review":        "Review and erase",
		"le.erase_link":    "Erase this lead",
		"le.erase_title":   "Erase lead %s",
		"le.warning":       "This cannot be undone. The submission, its notification record and any notification still waiting are deleted.",
		"le.not_recalled":  "Not recalled: e-mail already sent or queued in can-core, what a webhook or CRM copied, exports, and backups until they expire (a restore erases the lead again).",
		"le.type":          "Type %s to confirm",
		"le.submit":        "Erase for good",
		"le.err_email":     "Enter one email address.",
		"le.err_confirm":   "The text typed is not the lead id.",
		"le.err_forbidden": "The forms module refused: the credential in CAN_LEADS_TOKEN_FILE may not erase. Use the module's read and write token.",
		"le.err_scrub":     "The lead is erased, but the database files could not be scrubbed. Run `can leads erase` with the same id to retry.",
	} {
		messagesEN[k] = v
	}
}
